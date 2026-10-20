// Look-back: when a collection starts or gains an include rule, file the
// owner's earlier events (the last N days, default 7); after an exclude,
// re-check current members. It runs as a background task that owns a
// subagent: a child conversation that starts as a copy of the steward (same
// model, code mode, query and append, same sections), given the job as one
// message. So earlier events are filed exactly as the steward files new
// ones (labels, dated values, records), without blocking the owner's next
// text. The child is created once and its id checkpointed, and the job is
// submitted with a fixed requestId, so a restart resumes the same work; the
// child's writes are idempotent per nested tool call.

import type { Context } from "@earendil-works/chord/context";
import { type ConversationId, defineTask, type TaskRuntime } from "@earendil-works/pi-durable";
import { OwnerDoc } from "./data.ts";

export const DEFAULT_LOOKBACK_DAYS = 7;

export type LookBackInput = {
	owner: string;
	collectionId: string;
	mode: "include" | "exclude";
	since: string; // RFC 3339
	triggerEventId: string;
};

type State = { phase: "start" } | { phase: "run"; child: ConversationId };

/** The job, as the child reads it. Collection details come from its <collections> section. */
export function lookBackJob(i: LookBackInput): string {
	const head = `Look-back job (from the system, not an event). Event ${i.triggerEventId} changed collection ${i.collectionId}; see <collections> for its rules and current version.`;
	const work =
		i.mode === "include"
			? `Go through the owner's events that occurred at or after ${i.since} (events with dispatch = 1 and source != 'agent', other than ${i.triggerEventId} and those already in the collection), in pages, and file each one that belongs exactly as you would have when it arrived: label.applied at the current version, and for a metric collection its values with their dates. The parent_id of each write is the event it files.`
			: `A rule now excludes some events. Re-check the collection's current members against its rules, and for each that no longer belongs append label.removed at the current version (parent_id ${i.triggerEventId}) and withdraw its values with metric.removed.`;
	return `${head}\n${work}\nReply with one line: how many events you checked and how many you changed.`;
}

/** The child's answer: the text of the newest message in its transcript (the entry belongs to the child, so the task can't read it by id). */
async function answerText(runtime: TaskRuntime<LookBackInput, State, null, object>, child: ConversationId, ctx: Context): Promise<string> {
	const { messages } = await runtime.context(child, ctx);
	const last = messages.at(-1);
	if (last?.role !== "assistant") return "";
	return last.content.map((p) => (p.type === "text" ? p.text : "")).join("").trim();
}

export function lookBackTask() {
	return defineTask<LookBackInput, State, null>({
		name: "lifestream.lookback",
		version: 2,
		initial: () => ({ phase: "start" }),
		phases: {
			// The child starts as a copy of this task's conversation's agent; it
			// needs its own owner doc for the sections and writes.
			start: (task, runtime, ctx) =>
				runtime.commit(async (tx) => {
					const created = await tx.createConversation({ ownership: { kind: "task", taskId: task.id } });
					const doc = await tx.doc(OwnerDoc, created.id);
					doc.owner = task.input.owner;
					return { status: "running", checkpoint: { phase: "run", child: created.id } };
				}, ctx),
			run: async (task, runtime, ctx) => {
				const st = task.state.checkpoint as Extract<State, { phase: "run" }>;
				const child = await runtime.conversation(st.child, ctx);
				if (!child) throw new Error(`look-back ${task.id}: child conversation ${st.child} vanished`);
				const settled = await (await child.submit({ type: "input", content: lookBackJob(task.input), requestId: `lookback-job:${task.id}` }, ctx)).wait(ctx);
				const summary = settled.status === "done" ? await answerText(runtime, st.child, ctx) : `it ended ${settled.status}`;
				const parent = await runtime.conversation(runtime.conversationId, ctx);
				await parent?.submit(
					{ type: "input", content: `Look-back for collection ${task.input.collectionId} finished: ${summary || "no summary"}. No action needed.`, whenBusy: "followUp", requestId: `lookback:${task.id}` },
					ctx,
				);
				await runtime.commit(() => ({ status: "terminal", outcome: { status: "completed", result: null } }), ctx);
			},
		},
		abort: (_t, runtime, ctx) => runtime.commit(() => ({ status: "terminal", outcome: { status: "aborted" } }), ctx),
	});
}
