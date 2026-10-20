// The `collections` extension: steering commands, labeling every event,
// and extracting values for metric collections. Collection definitions live
// in Go; a prompt section fetches the open ones before every model request,
// so a steering change applies to the very next event with no restart.

import type { Context } from "@earendil-works/chord/context";
import { Type } from "@earendil-works/pi-ai";
import { type ConversationId, defineDoc, defineExtension, defineTool, section, type ToolExecutionApi } from "@earendil-works/pi-durable";
import { ApiError, type Collection, type GoApi } from "./goapi.ts";
import { DEFAULT_LOOKBACK_DAYS, type LookBackOptions, lookBackTask } from "./lookback.ts";

/** Which owner a conversation belongs to. Written when the conversation is created. */
export const OwnerDoc = defineDoc<{ owner: string }>({
	kind: "lifestream.owner",
	version: 1,
	scope: "conversation",
	history: "latest",
	fork: "current",
	initial: () => ({ owner: "" }),
});

type DocReader = {
	snapshot<T extends Record<string, unknown>>(token: typeof OwnerDoc, conversationId: ConversationId, ctx: Context): Promise<Readonly<T> | undefined>;
};

export async function ownerOf(reader: unknown, conversationId: ConversationId, ctx: Context): Promise<string> {
	const doc = await (reader as DocReader).snapshot<{ owner: string }>(OwnerDoc, conversationId, ctx);
	if (!doc?.owner) throw new Error(`conversation ${conversationId} has no owner`);
	return doc.owner;
}

export const INSTRUCTIONS = `You file one person's events into collections. Nobody reads your replies; your work is the tool calls.

Each user message is one event from the owner's event store: an SMS, a note, a correction they made on the dashboard, or JSON from another source. For each event:

1. Decide whether it is a steering command or data.
   - Commands change collections: "start capturing for X" / "track my weight" (create_collection), "also include Y in X" (amend_collection with include), "don't include Z in X" (amend_collection with exclude), "stop capturing X" (stop_collection), "resume X" (resume_collection). Pass the command event's id as event_id.
   - Everything else is data.
2. For data, check it against every open collection's rules (the <collections> section). Label it into each collection it matches with label_event, passing that collection's current version. An exclude rule beats an include rule.
3. For a metric collection, also call log_value with the number the event reports (e.g. "weight 175" -> 175). Use the collection's unit unless the event names another. The reading is dated at the event's time unless the owner says when it was taken ("yesterday", "on 10-19", "Monday morning"): then pass observed_at as an RFC 3339 timestamp, counting relative days from the event's timestamp, and for a day with no time of day use 12:00:00Z on that day. If the owner corrects an earlier reading (its number or its date), call log_value again with the original event's event_id; that replaces the earlier value.
4. If you can't tell what the owner meant (an implausible number, an ambiguous command), call flag_for_review with a short question instead of guessing. The answer comes back later as a new event.
5. A label.corrected event from the dashboard means the owner fixed a label you made (or missed). Learn the boundary from it; if it shows a rule is missing, amend the collection so future events are filed correctly.
6. A review.answered event is the owner's answer to a question you flagged; its payload has the question and the original event_id. Act on it now (label, log the value, amend a rule), and write down anything general it teaches.
7. collection.* events from the dashboard are changes the owner already made, often undoing one of yours. They are applied: don't repeat or reverse them, but don't make the same change again either.
8. If the owner teaches you something ("w means weight", "ignore anything from ~/scratch"), make it durable with add_rule. Background work and future conversations only see what is written down. Follow every entry in <owner_rules>.

Starting a collection or adding an include automatically labels the last 7 days of events in the background (pass lookback_days if the owner asks for another window); an exclude re-checks current members. A "Look-back ... finished" message is that job reporting; nothing to do.

Large payloads arrive as a pointer; call read_event to see the content when the title isn't enough.

Reply with at most one short line summarizing what you did.`;

export function collectionsSection(c: Collection): string {
	const lines = [`- id=${c.id} name=${JSON.stringify(c.name)} kind=${c.kind} version=${c.version}`];
	if (c.kind === "metric") lines[0] += ` metric=${c.metric ?? c.name}${c.unit ? ` unit=${c.unit}` : ""}`;
	for (const r of c.rules) lines.push(`    ${r.kind} (v${r.version}): ${r.text}`);
	return lines.join("\n");
}

export const text = (s: string) => ({ output: [{ type: "text" as const, text: s }] });
export const fail = (s: string) => ({ ...text(s), isError: true });

/**
 * Builds the extension. `api` is the Go client; `lookBack` tunes the
 * look-back task (tests).
 */
export function collectionsExtension(api: GoApi, opts: { lookBack?: LookBackOptions } = {}) {
	const LookBack = lookBackTask(api, opts.lookBack);
	// Every write: owner from the conversation, idempotency key from the tool
	// task (stable when a replay-safe tool reruns after a crash).
	// Starts the look-back over the last N days (or a re-check of members).
	const lookBack = (tapi: ToolExecutionApi, ctx: Context, collectionId: string, mode: "include" | "exclude", days: number | undefined, triggerEventId: string) => {
		const since = new Date(Date.now() - (days ?? DEFAULT_LOOKBACK_DAYS) * 86400_000).toISOString();
		return async (owner: string) =>
			days === 0 ? undefined : tapi.createTask(LookBack, { owner, collectionId, mode, since, triggerEventId }, { ownership: { kind: "conversation" }, background: true }, ctx);
	};
	const write = async (
		tapi: { taskId: unknown; conversationId: ConversationId },
		ctx: Context,
		type: string,
		payload: unknown,
		parentId?: string,
		after?: (owner: string, id: string) => Promise<unknown>,
	) => {
		try {
			const owner = await ownerOf(tapi, tapi.conversationId, ctx);
			const e = await api.append(owner, type, payload, { parentId, idempotencyKey: `tool:${String(tapi.taskId)}` });
			await after?.(owner, e.id);
			return text(`ok: ${type} ${e.id}`);
		} catch (err) {
			if (err instanceof ApiError && err.status < 500) return fail(`rejected: ${err.message}`);
			throw err;
		}
	};

	const createCollection = defineTool({
		name: "create_collection",
		description: "Start a new collection. kind=metric for things with a number to chart (weight, sleep hours).",
		parameters: Type.Object({
			event_id: Type.String({ description: "id of the event that asked for it" }),
			name: Type.String(),
			include: Type.String({ description: "plain-English rule for what belongs" }),
			kind: Type.Optional(Type.Union([Type.Literal("set"), Type.Literal("metric")])),
			unit: Type.Optional(Type.String()),
			lookback_days: Type.Optional(Type.Integer({ minimum: 0, description: "how far back to label existing events; default 7, 0 for none, or what the owner asked for" })),
		}),
		replay: "safe",
		execute: (a, tapi, ctx) =>
			write(tapi, ctx, "collection.started", { name: a.name, include: a.include, kind: a.kind, unit: a.unit, lookback_days: a.lookback_days }, a.event_id,
				(owner, id) => lookBack(tapi, ctx, id, "include", a.lookback_days, a.event_id)(owner)),
	});

	const amendCollection = defineTool({
		name: "amend_collection",
		description: "Add one include or one exclude rule to a collection.",
		parameters: Type.Object({
			event_id: Type.String(),
			collection_id: Type.String(),
			include: Type.Optional(Type.String()),
			exclude: Type.Optional(Type.String()),
			lookback_days: Type.Optional(Type.Integer({ minimum: 0, description: "for an include: how far back to label; default 7" })),
		}),
		replay: "safe",
		execute: (a, tapi, ctx) =>
			write(tapi, ctx, "collection.amended", { collection_id: a.collection_id, include: a.include, exclude: a.exclude }, a.event_id,
				(owner) => lookBack(tapi, ctx, a.collection_id, a.exclude ? "exclude" : "include", a.lookback_days, a.event_id)(owner)),
	});

	const statusTool = (name: string, type: string, description: string) =>
		defineTool({
			name,
			description,
			parameters: Type.Object({ event_id: Type.String(), collection_id: Type.String() }),
			replay: "safe",
			execute: (a, tapi, ctx) => write(tapi, ctx, type, { collection_id: a.collection_id }, a.event_id),
		});

	const labelEvent = defineTool({
		name: "label_event",
		description: "Put an event into a collection. Pass the collection's current version and a short reason.",
		parameters: Type.Object({
			event_id: Type.String(),
			collection_id: Type.String(),
			definition_version: Type.Integer({ minimum: 1 }),
			reason: Type.String(),
		}),
		replay: "safe",
		execute: (a, tapi, ctx) =>
			write(tapi, ctx, "label.applied", {
				event_id: a.event_id, collection_id: a.collection_id, definition_version: a.definition_version, reason: a.reason,
			}, a.event_id),
	});

	const logValue = defineTool({
		name: "log_value",
		description: "Record the number an event reports, for a metric collection. Logging again for the same event_id replaces its value.",
		parameters: Type.Object({
			event_id: Type.String(),
			collection_id: Type.String(),
			value: Type.Number(),
			unit: Type.Optional(Type.String()),
			observed_at: Type.Optional(Type.String({ description: "RFC 3339; only when the owner says when the reading was taken (default: the event's time)" })),
		}),
		replay: "safe",
		execute: (a, tapi, ctx) =>
			write(tapi, ctx, "metric.observed", { collection_id: a.collection_id, value: a.value, unit: a.unit, observed_at: a.observed_at }, a.event_id),
	});

	const flagForReview = defineTool({
		name: "flag_for_review",
		description: "Ask the owner a question on the dashboard instead of guessing.",
		parameters: Type.Object({ event_id: Type.String(), question: Type.String() }),
		replay: "safe",
		execute: (a, tapi, ctx) => write(tapi, ctx, "review.flagged", { question: a.question }, a.event_id),
	});

	const readEvent = defineTool({
		name: "read_event",
		description: "Read an event's full payload, or a byte range of a large one.",
		parameters: Type.Object({
			event_id: Type.String(),
			offset: Type.Optional(Type.Integer({ minimum: 0 })),
			limit: Type.Optional(Type.Integer({ minimum: 1, maximum: 65536 })),
		}),
		replay: "safe",
		execute: async (a) => {
			try {
				const range = a.offset !== undefined || a.limit !== undefined ? { offset: a.offset ?? 0, limit: a.limit ?? 16384 } : undefined;
				return text(JSON.stringify(await api.event(a.event_id, range)));
			} catch (err) {
				if (err instanceof ApiError && err.status < 500) return fail(err.message);
				throw err;
			}
		},
	});

	return defineExtension({
		name: "collections",
		tasks: [LookBack],
		tools: [
			createCollection,
			amendCollection,
			statusTool("stop_collection", "collection.stopped", "Stop a collection: no new labels until resumed."),
			statusTool("resume_collection", "collection.resumed", "Reopen a stopped collection."),
			labelEvent,
			logValue,
			flagForReview,
			readEvent,
		],
		sections: [
			// Not the agent's own `instructions` (reserved, and stored per
			// conversation at creation): a section follows the code on deploy.
			section("role", () => INSTRUCTIONS),
			// Deterministic text: it only changes when a definition does, so the
			// prompt cache survives every other request (see LEARNINGS.md).
			section("collections", async (input, ctx) => {
				const owner = await ownerOf(input.read, input.conversationId, ctx);
				const open = await api.collections(owner, "open");
				if (open.length === 0) return "No open collections.";
				return open.map(collectionsSection).join("\n");
			}),
		],
	});
}
