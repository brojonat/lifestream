// Look-back: when a collection starts or gains an include rule, label the
// owner's events from the last N days (default 7); after an exclude, re-check
// current members. Runs as a Pi Durable background task that checkpoints
// after every batch, so a crash resumes where it stopped. Background tasks
// can't call tools, so it asks the model directly and writes through Go's
// API with deterministic idempotency keys: a rerun batch, or a duplicate
// task, writes nothing twice.

import type { Context } from "@earendil-works/chord/context";
import type { Models } from "@earendil-works/pi-ai/models";
import { defineTask, type TaskRuntime } from "@earendil-works/pi-durable";
import { ApiError, type Collection, type GoApi, type StoredEvent } from "./goapi.ts";
import type { redactionTask } from "./redaction.ts";
import { type Retry, retrying, retryPhase } from "./retry.ts";

type RedactTask = ReturnType<typeof redactionTask>;

export const DEFAULT_LOOKBACK_DAYS = 7;
const BATCH = 20;

/** Raw events worth judging; derived and bookkeeping events are not. */
const SKIP_TYPES = /^(collection\.|label\.|rule\.|review\.|metric\.|redaction\.)/;

export type LookBackInput = {
	owner: string;
	collectionId: string;
	mode: "include" | "exclude";
	since: string; // RFC 3339
	triggerEventId: string;
};

type Scan = { phase: "scan"; after: number; checked: number; changed: number; attempt?: number };
type Report = { phase: "report"; checked: number; changed: number; name: string };
type State = Scan | Retry<Scan> | Report;

export type Verdict = { event_id: string; member: boolean; value?: number; reason?: string };

/** Asks a model about a batch. Tests pass a stub; otherwise modelJudge. */
export type Judge = (c: Collection, mode: "include" | "exclude", events: StoredEvent[], ctx: Context) => Promise<Verdict[]>;

/** A judge that asks the given model, one request per batch. */
export function modelJudge(models: Models, ref: { provider: string; modelId: string }): Judge {
	return async (c, mode, events) => {
		const model = models.getModel(ref.provider, ref.modelId);
		if (!model) throw new Error(`unknown model ${ref.provider}/${ref.modelId}`);
		const reply = await models.completeSimple(model, {
			systemPrompt: "You sort a person's events into collections. Reply with JSON only.",
			messages: [{ role: "user", content: judgePrompt(c, mode, events), timestamp: Date.now() }],
		});
		if (reply.stopReason === "error") throw new Error(`look-back model call failed: ${reply.errorMessage ?? "unknown error"}`);
		const text = reply.content.map((p) => (p.type === "text" ? p.text : "")).join("");
		return parseVerdicts(text);
	};
}

export function judgePrompt(c: Collection, mode: "include" | "exclude", events: StoredEvent[]): string {
	const rules = c.rules.map((r) => `- ${r.kind}: ${r.text}`).join("\n");
	const items = events
		.map((e) => `- id=${e.id} type=${e.type} source=${e.source} at=${e.occurred_at} payload=${JSON.stringify(e.payload).slice(0, 1500)}`)
		.join("\n");
	const ask =
		mode === "include"
			? "For each event, decide whether it belongs in the collection."
			: "These events are current members. A rule changed; decide for each whether it still belongs.";
	const value = c.kind === "metric" ? ` For members, also give "value": the number the event reports for ${c.metric ?? c.name}${c.unit ? ` in ${c.unit}` : ""}.` : "";
	return `Collection ${JSON.stringify(c.name)} (${c.kind}). Rules, where an exclude beats an include:\n${rules}\n\n${ask}${value}\nAnswer with only a JSON array, one object per event: {"event_id": "...", "member": true|false, "reason": "a few words"${c.kind === "metric" ? ', "value": 0' : ""}}.\n\nEvents:\n${items}`;
}

/** Pulls the JSON array out of a model reply. */
export function parseVerdicts(text: string): Verdict[] {
	const start = text.indexOf("[");
	const end = text.lastIndexOf("]");
	if (start < 0 || end < start) throw new Error(`no JSON array in reply: ${text.slice(0, 200)}`);
	const parsed = JSON.parse(text.slice(start, end + 1)) as Verdict[];
	return parsed.filter((v) => typeof v?.event_id === "string" && typeof v.member === "boolean");
}

export type LookBackOptions = { judge?: Judge; retryBaseMs?: number };

/** `redact` is the redaction task, started for every event the look-back labels. */
export function lookBackTask(api: GoApi, redact: RedactTask, { judge, retryBaseMs }: LookBackOptions = {}) {
	// One batch: judge it, write the results, checkpoint the cursor. Writes
	// use deterministic keys, so rerunning a batch after a crash or a retry
	// changes nothing twice.
	async function scanBatch(input: LookBackInput, st: Scan, runtime: TaskRuntime<LookBackInput, State, null, object>, ctx: Context) {
		const { owner, collectionId, mode, since, triggerEventId } = input;
		const c = (await api.collections(owner)).find((x) => x.id === collectionId);
		const done = (name: string) =>
			runtime.commit(() => ({ status: "running", checkpoint: { phase: "report", checked: st.checked, changed: st.changed, name } }), ctx);
		if (!c || c.status !== "open") return done(c?.name ?? collectionId);

		const members = await api.members(collectionId);
		const memberIds = new Set(members.map((m) => m.event_id));
		let batch: StoredEvent[];
		let next: number;
		if (mode === "include") {
			const page = await api.events(owner, { since, after: st.after, limit: BATCH, maxPayload: 2000 });
			if (page.length === 0) return done(c.name);
			next = page.at(-1)!.seq ?? st.after + page.length;
			batch = page.filter((e) => e.source !== "agent" && !SKIP_TYPES.test(e.type) && e.id !== triggerEventId && !memberIds.has(e.id));
		} else {
			// `after` indexes the member list, oldest first.
			const slice = members.slice(st.after, st.after + BATCH);
			if (slice.length === 0) return done(c.name);
			next = st.after + slice.length;
			const got = await Promise.all(
				slice.map((m) =>
					(api.event(m.event_id, { offset: 0, limit: 2000 }) as Promise<StoredEvent & { data?: string }>).catch((err) => {
						if (err instanceof ApiError && err.status === 404) return undefined; // gone; nothing to judge
						throw err;
					}),
				),
			);
			batch = got.filter((e) => e !== undefined).map((e) => ({ ...e, payload: e.data ?? e.payload }));
		}

		let changed = 0;
		const labeled: string[] = [];
		if (batch.length > 0) {
			let ask = judge;
			if (!ask) {
				const agent = await runtime.agent(ctx);
				if (!agent.model) throw new Error("conversation has no model");
				ask = modelJudge(runtime.models, agent.model);
			}
			const verdicts = await ask(c, mode, batch, ctx);
			const ids = new Set(batch.map((e) => e.id));
			for (const v of verdicts) {
				if (!ids.has(v.event_id)) continue; // the model invented an id
				const key = `lookback:${collectionId}:v${c.version}:${v.event_id}`;
				if (mode === "include" && v.member) {
					await api.append(owner, "label.applied", { event_id: v.event_id, collection_id: c.id, definition_version: c.version, reason: `look-back: ${v.reason ?? "matches"}` }, { parentId: v.event_id, idempotencyKey: `${key}:label` });
					labeled.push(v.event_id);
					if (c.kind === "metric" && typeof v.value === "number") {
						await api.append(owner, "metric.observed", { collection_id: c.id, value: v.value }, { parentId: v.event_id, idempotencyKey: `${key}:value` });
					}
					changed++;
				} else if (mode === "exclude" && !v.member) {
					await api.append(owner, "label.removed", { event_id: v.event_id, collection_id: c.id, definition_version: c.version, reason: v.reason ?? "excluded" }, { parentId: v.event_id, idempotencyKey: `${key}:remove` });
					changed++;
				}
			}
		}
		// Removed members drop out of the list, so the exclude cursor only moves
		// past the ones that stayed.
		if (mode === "exclude") next -= changed;
		// The checkpoint and the redaction tasks for this batch commit together.
		await runtime.commit(async (tx) => {
			for (const eventId of labeled) {
				await tx.createTask(redact, { owner, eventId }, { ownership: { kind: "conversation" }, background: true });
			}
			return { status: "running", checkpoint: { phase: "scan", after: next, checked: st.checked + batch.length, changed: st.changed + changed } };
		}, ctx);
	}

	return defineTask<LookBackInput, State, null>({
		name: "lifestream.lookback",
		version: 1,
		initial: () => ({ phase: "scan", after: 0, checked: 0, changed: 0 }),
		phases: {
			scan: retrying<LookBackInput, Scan, State, null>(retryBaseMs, (task, runtime, ctx) => scanBatch(task.input, task.state.checkpoint, runtime, ctx)),
			retry: retryPhase<LookBackInput, Scan, State, null>(),
			report: async (task, runtime, ctx) => {
				const st = task.state.checkpoint as Report;
				const what = task.input.mode === "include" ? `labeled ${st.changed}` : `removed ${st.changed}`;
				const conv = await runtime.conversation(runtime.conversationId, ctx);
				await conv?.submit(
					{ type: "input", content: `Look-back for ${JSON.stringify(st.name)} finished: checked ${st.checked} events, ${what}. No action needed.`, whenBusy: "followUp", requestId: `lookback:${task.id}` },
					ctx,
				);
				await runtime.commit(() => ({ status: "terminal", outcome: { status: "completed", result: null } }), ctx);
			},
		},
		abort: (_t, runtime, ctx) => runtime.commit(() => ({ status: "terminal", outcome: { status: "aborted" } }), ctx),
	});
}
