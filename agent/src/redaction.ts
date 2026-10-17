// Redaction: every event, as soon as it is labeled into a collection, gets
// a redacted copy (redaction.created, parent = the event). Layers: the
// deterministic scanner, then a model review of what's left (client names,
// internal hostnames, anything the rules doc names), chunk by chunk so a
// large trace is reviewed in pieces. Runs as a background task that
// checkpoints after each chunk and retries failures. One copy per event:
// later labels of an already-redacted event do nothing.

import type { Context } from "@earendil-works/chord/context";
import type { Models } from "@earendil-works/pi-ai/models";
import { defineTask, type TaskRuntime } from "@earendil-works/pi-durable";
import type { GoApi } from "./goapi.ts";
import { type Retry, retrying, retryPhase } from "./retry.ts";
import { type Finding, redactStrings, scanJSON } from "./scanner.ts";

/** Review chunks; the rest of a giant payload is scanned but not reviewed. */
export const CHUNK_CHARS = 12_000;
export const MAX_CHUNKS = 10;

export type RedactInput = { owner: string; eventId: string };
export type Span = { text: string; kind: string };

type Review = { phase: "review"; chunk: number; spans: Span[]; attempt?: number };
type Start = { phase: "start"; attempt?: number };
type Write = { phase: "write"; spans: Span[]; chunks: number; attempt?: number };
type State = Start | Review | Write | Retry<Start | Review | Write>;

/** Asks a model for sensitive spans in one chunk. Tests pass a stub. */
export type Reviewer = (chunk: string, rules: string[], ctx: Context) => Promise<Span[]>;

export function reviewPrompt(chunk: string, rules: string[]): string {
	const owner = rules.length ? `\nThe owner's rules:\n${rules.map((r) => `- ${r}`).join("\n")}\n` : "";
	return `This text is about to be shown to strangers. Secrets are already masked as [REDACTED:kind]. Find anything sensitive that is left: names of clients, employers or private people; internal hostnames, URLs or IP addresses; account or customer numbers; street addresses; anything the owner's rules say to hide.${owner}
Reply with only a JSON array of {"text": "<exact substring to hide>", "kind": "<short label>"}, or [] if nothing.

Text:
${chunk}`;
}

export function parseSpans(reply: string): Span[] {
	const start = reply.indexOf("[");
	const end = reply.lastIndexOf("]");
	if (start < 0 || end < start) throw new Error(`no JSON array in review reply: ${reply.slice(0, 200)}`);
	const parsed = JSON.parse(reply.slice(start, end + 1)) as Span[];
	return parsed.filter((s) => typeof s?.text === "string" && s.text.trim() !== "" && typeof s.kind === "string");
}

export function modelReviewer(models: Models, ref: { provider: string; modelId: string }): Reviewer {
	return async (chunk, rules) => {
		const model = models.getModel(ref.provider, ref.modelId);
		if (!model) throw new Error(`unknown model ${ref.provider}/${ref.modelId}`);
		const reply = await models.completeSimple(model, {
			systemPrompt: "You review text for private information before it is published. Reply with JSON only.",
			messages: [{ role: "user", content: reviewPrompt(chunk, rules), timestamp: Date.now() }],
		});
		if (reply.stopReason === "error") throw new Error(`review model call failed: ${reply.errorMessage ?? "unknown error"}`);
		return parseSpans(reply.content.map((p) => (p.type === "text" ? p.text : "")).join(""));
	};
}

export type RedactionOptions = { reviewer?: Reviewer; retryBaseMs?: number };

export function redactionTask(api: GoApi, { reviewer, retryBaseMs }: RedactionOptions = {}) {
	// The scanned payload, recomputed from the event on every phase: it is
	// deterministic, so the checkpoint only has to carry the model's spans.
	const scanned = async (eventId: string) => {
		const e = (await api.event(eventId)) as { payload: unknown };
		return scanJSON(e.payload);
	};
	const reviewerFor = async (runtime: TaskRuntime<RedactInput, State, null, object>, ctx: Context) => {
		if (reviewer) return reviewer;
		const agent = await runtime.agent(ctx);
		if (!agent.model) throw new Error("conversation has no model");
		return modelReviewer(runtime.models, agent.model);
	};
	const next = (runtime: TaskRuntime<RedactInput, State, null, object>, ctx: Context, checkpoint: State) =>
		runtime.commit(() => ({ status: "running", checkpoint }), ctx);

	return defineTask<RedactInput, State, null>({
		name: "lifestream.redact",
		version: 1,
		initial: () => ({ phase: "start" }),
		phases: {
			start: retrying<RedactInput, Start, State, null>(retryBaseMs, async (task, runtime, ctx) => {
				const kids = await api.children(task.input.eventId);
				if (kids.some((k) => k.type === "redaction.created")) {
					return runtime.commit(() => ({ status: "terminal", outcome: { status: "completed", result: null } }), ctx);
				}
				await next(runtime, ctx, { phase: "review", chunk: 0, spans: [] });
			}),
			review: retrying<RedactInput, Review, State, null>(retryBaseMs, async (task, runtime, ctx) => {
				const st = task.state.checkpoint;
				const text = JSON.stringify((await scanned(task.input.eventId)).value);
				const chunks = Math.min(Math.ceil(text.length / CHUNK_CHARS), MAX_CHUNKS);
				if (st.chunk >= chunks) return next(runtime, ctx, { phase: "write", spans: st.spans, chunks });
				const rules = (await api.rules(task.input.owner)).map((r) => r.text);
				const ask = await reviewerFor(runtime, ctx);
				const found = await ask(text.slice(st.chunk * CHUNK_CHARS, (st.chunk + 1) * CHUNK_CHARS), rules, ctx);
				await next(runtime, ctx, { phase: "review", chunk: st.chunk + 1, spans: [...st.spans, ...found] });
			}),
			write: retrying<RedactInput, Write, State, null>(retryBaseMs, async (task, runtime, ctx) => {
				const st = task.state.checkpoint;
				const { value, findings } = await scanned(task.input.eventId);
				const text = JSON.stringify(value);
				const payload = {
					event_id: task.input.eventId,
					payload: redactStrings(value, st.spans),
					findings: [...findings, ...countSpans(st.spans)],
					reviewed_chars: Math.min(text.length, st.chunks * CHUNK_CHARS),
					total_chars: text.length,
				};
				await api.append(task.input.owner, "redaction.created", payload, { parentId: task.input.eventId, idempotencyKey: `redact:${task.input.eventId}` });
				await runtime.commit(() => ({ status: "terminal", outcome: { status: "completed", result: null } }), ctx);
			}),
			retry: retryPhase<RedactInput, Start | Review | Write, State, null>(),
		},
		abort: (_t, runtime, ctx) => runtime.commit(() => ({ status: "terminal", outcome: { status: "aborted" } }), ctx),
	});
}

function countSpans(spans: Span[]): Finding[] {
	const counts = new Map<string, number>();
	for (const s of spans) counts.set(`model:${s.kind}`, (counts.get(`model:${s.kind}`) ?? 0) + 1);
	return [...counts].map(([kind, count]) => ({ kind, count }));
}
