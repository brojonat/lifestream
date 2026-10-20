// What the agent is doing right now, for the dashboard's live panel. A
// snapshot is computed from Pi Durable's conversation view (pi.live: the
// in-flight response, the tool round, the script's nested calls; pi.inbox;
// pi.usage) and the task graph (running look-backs and the subagent
// conversations they own). It only reads; Go relays it to the browser.

import type { Context } from "@earendil-works/chord/context";
import type { ConversationId, ConversationView, InboxState, LiveState, TaskGraph, UsageState } from "@earendil-works/pi-durable";

/** One script call, as the panel lists it. */
export type CallView = { name: string; args: string; status: string; error?: string; ms?: number };

/** One conversation's state. Text fields are cut to what a panel can show. */
export type ConvSnapshot = {
	id: string;
	busy: boolean;
	handling?: string; // the newest input: the event (or job) it's on
	thinking?: string; // the in-flight response's thinking
	text?: string; // the in-flight response's text
	retry?: string; // a failed attempt waiting to retry
	script?: string; // the newest code mode script: being written, running, or the last one run
	calls: CallView[]; // that script's calls (live while it runs, from its result after)
	output?: string; // that script's output, once it finished
	queued: number; // inputs waiting for the current run
	last?: string; // the last reply, when idle
	cost_usd: number; // this conversation's spend so far
};

export type Snapshot = {
	at: string;
	main: ConvSnapshot;
	lookbacks: { task_id: string; kind: string; phase: string; child?: ConvSnapshot }[];
};

type Part = { type?: string; text?: string; thinking?: string; id?: string; name?: string; arguments?: unknown };
type Msg = { role?: string; content?: string | Part[]; toolCallId?: string; details?: unknown };
/** A finished script's calls, as code mode records them in its result's details. */
type DoneCall = { name: string; args: string; status: string; durationMs?: number; error?: string };

const tail = (s: string, n: number) => (s.length > n ? `…${s.slice(-n)}` : s);
const head = (s: string, n: number) => (s.length > n ? `${s.slice(0, n)}…` : s);
const parts = (m: Msg | undefined): Part[] => (typeof m?.content === "string" ? [{ type: "text", text: m.content }] : (m?.content ?? []));
const textOf = (m: Msg | undefined, kind: "text" | "thinking" = "text") =>
	parts(m).map((p) => (p.type === kind ? (kind === "text" ? p.text : p.thinking) ?? "" : "")).join("").trim();
const codeOf = (p: Part | undefined) => {
	const code = (p?.arguments as { code?: unknown } | undefined)?.code;
	return typeof code === "string" ? code : undefined;
};

/** The snapshot of one conversation's view. */
export function convSnapshot(view: ConversationView): ConvSnapshot {
	const live = (view.docs["pi.live"] ?? {}) as LiveState;
	const inbox = view.docs["pi.inbox"] as InboxState | undefined;
	const usage = view.docs["pi.usage"] as UsageState | undefined;
	const messages = view.entries.flatMap((e) => (e.model ?? []) as readonly Msg[]);
	const lastUser = messages.findLast((m) => m.role === "user");
	const lastAssistant = messages.findLast((m) => m.role === "assistant");
	const partial = live.generation?.message as Msg | undefined;
	const busy = live.run !== undefined;

	// The script: still streaming in the response, else the newest one in the
	// transcript, running or finished. Nested calls leave pi.live when the
	// script settles (often within milliseconds), so finished calls come from
	// its result's details.
	const streaming = codeOf(parts(partial).findLast((p) => p.type === "toolCall" && p.name === "codemode"));
	const call = messages.flatMap(parts).findLast((p) => p.type === "toolCall" && p.name === "codemode");
	const result = call && messages.find((m) => m.role === "toolResult" && m.toolCallId === call.id);
	const script = streaming ?? codeOf(call);
	const doneCalls = ((result?.details as { calls?: DoneCall[] } | undefined)?.calls ?? []).map((c) => ({
		name: c.name,
		args: head(c.args, 200),
		status: c.status === "ok" ? "done" : c.status,
		error: c.error,
		ms: c.durationMs,
	}));

	const cost = Object.values({ ...usage?.models, ...usage?.tools }).reduce((sum, u) => sum + ((u as { cost?: { total?: number } }).cost?.total ?? 0), 0);
	const snap: ConvSnapshot = {
		id: String(view.conversation.id),
		busy,
		calls: streaming
			? []
			: live.nestedTools?.length
				? live.nestedTools.map((c) => ({
						name: c.name,
						args: head(JSON.stringify(c.arguments), 200),
						status: c.summary?.isError ? "error" : c.status,
						error: c.summary?.error,
						ms: c.summary?.durationMs,
					}))
				: doneCalls,
		queued: inbox?.items.length ?? 0,
		cost_usd: Math.round(cost * 1e6) / 1e6,
	};
	if (busy && lastUser) snap.handling = head(textOf(lastUser), 600);
	if (partial) {
		snap.thinking = tail(textOf(partial, "thinking"), 1500) || undefined;
		snap.text = tail(textOf(partial), 1500) || undefined;
	}
	if (live.generation?.retry) snap.retry = `${live.generation.retry.error} (attempt ${live.generation.attempt})`;
	if (script) snap.script = head(script, 4000);
	if (result && !streaming) snap.output = head(textOf(result), 800) || undefined;
	if (!busy && lastAssistant) snap.last = head(textOf(lastAssistant), 600) || undefined;
	return snap;
}

/** What the live feed needs from the agent. */
export type LiveSource = {
	conversationId(owner: string, ctx: Context): Promise<ConversationId>;
	viewState(id: ConversationId, ctx: Context): Promise<{ value: ConversationView; subscribe(f: () => void): () => void; dispose(): void }>;
	taskGraph(ctx: Context): Promise<{ value: TaskGraph; subscribe(f: () => void): () => void; dispose(): void }>;
};

/**
 * Calls `emit` with the owner's snapshot now and after every change (at most
 * every `throttleMs`), until the returned function is called. Look-back
 * subagents are attached as they appear and dropped when their task ends.
 */
export async function watchOwner(src: LiveSource, owner: string, ctx: Context, emit: (s: Snapshot) => void, throttleMs = 200): Promise<() => void> {
	const mainId = await src.conversationId(owner, ctx);
	const main = await src.viewState(mainId, ctx);
	const graph = await src.taskGraph(ctx);
	const children = new Map<string, Awaited<ReturnType<LiveSource["viewState"]>>>();
	const unsubs: (() => void)[] = [];
	let closed = false;
	let timer: ReturnType<typeof setTimeout> | undefined;

	const lookbacks = () => Object.values(graph.value.tasks).filter((t) => String(t.conversationId) === String(mainId) && t.conversations.length > 0);
	const render = async () => {
		timer = undefined;
		if (closed) return;
		// Attach new subagents, drop finished ones.
		const live = new Set<string>();
		for (const t of lookbacks()) {
			const id = String(t.conversations[0]);
			live.add(id);
			if (!children.has(id)) {
				const st = await src.viewState(t.conversations[0], ctx);
				if (closed) return st.dispose();
				children.set(id, st);
				unsubs.push(st.subscribe(schedule));
			}
		}
		for (const [id, st] of children) if (!live.has(id)) (st.dispose(), children.delete(id));
		emit({
			at: new Date().toISOString(),
			main: convSnapshot(main.value),
			lookbacks: lookbacks().map((t) => {
				const st = children.get(String(t.conversations[0]));
				return { task_id: String(t.id), kind: t.kind, phase: t.state.status === "completing" ? "completing" : t.state.phase, child: st && convSnapshot(st.value) };
			}),
		});
	};
	const schedule = () => {
		if (!closed && !timer) timer = setTimeout(() => void render(), throttleMs);
	};
	unsubs.push(main.subscribe(schedule), graph.subscribe(schedule));
	await render();
	return () => {
		closed = true;
		clearTimeout(timer);
		for (const u of unsubs) u();
		for (const st of [main, graph, ...children.values()]) st.dispose();
	};
}
