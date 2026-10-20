// The `data` extension: the agent as the steward of one person's data. The
// model gets one tool, code mode (see agent.ts); its scripts call `query`
// (one read-only SQL statement over the whole store) and `append` (any
// event type Go accepts). Writes are events, so everything the agent does
// is traced to what asked for it, replay-safe, and reversible. Sections
// give the model the schema, the open collections and the owner's rules,
// fetched from Go before every request, so a change applies to the very
// next event with no restart.

import type { Context } from "@earendil-works/chord/context";
import { Type } from "@earendil-works/pi-ai";
import { type ConversationId, defineDoc, defineExtension, defineTool, section, type ToolExecutionApi } from "@earendil-works/pi-durable";
import { ApiError, type Collection, type GoApi, type Schema } from "./goapi.ts";
import { DEFAULT_LOOKBACK_DAYS, lookBackTask } from "./lookback.ts";

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

export const INSTRUCTIONS = `You are the steward of one person's data. Each user message is one event from their append-only event store: an SMS, a note, a correction they made on the dashboard, JSON from another source, a turn of a coding session. It is already stored. Your job is to turn it into structured data that can be queried: collections, numbers, records. Nobody reads your replies; your work is what you write.

You act by writing JavaScript for the codemode tool. Scripts call:
- tools.query({sql, params?, limit?}) -> {columns, rows, truncated}: one read-only SQLite statement over the whole store (<schema> lists the tables; rows default to 200).
- tools.append({type, parent_id, payload}) -> {id}: append one event (<schema> lists the types you may append and their payloads). parent_id is the event that led to it, usually the one you are handling, so every change traces back.
Do related work in one script. A rejected call throws with the reason, e.g. a 422 when an event contradicts the current state; read it and fix the call.

How the data is shaped:
- Collections are named sets of events with plain-English include and exclude rules (<collections>). A metric collection also holds numbers (metric_values) and gets a chart.
- Records (record.saved) keep anything else worth keeping, in whatever shape fits: a short kind ("workout", "meal") and a JSON object. Query the records table first and reuse the kinds and field names already there, so the data stays consistent. When a collection comes to cover data you kept as records, file it in the collection and withdraw those records, so nothing is counted twice.
- Events are append-only, and query can't write. Change data by appending: replaces= supersedes a value or record; metric.removed, record.removed and label.removed withdraw.

For each event:
1. Steering commands change collections: "track my weight" (collection.started), "also include Y in X" (collection.amended with include), "don't include Z in X" (exclude), "stop X" / "resume X" (collection.stopped / collection.resumed).
2. Data: label it into every open collection whose rules it matches (label.applied, at the collection's current version; an exclude beats an include). For a metric collection, also append a metric.observed for each number it reports ("175 today, 176 yesterday" is two). Use the collection's unit unless the event names another. Keep anything else worth keeping as a record.
3. A value or record is dated at the event's time unless the owner says when it happened ("yesterday", "on 10-19", "Monday morning"): then pass observed_at, counting from the event's timestamp, in the owner's timezone if <owner_rules> gives one (noon when no time of day is given, e.g. 2026-10-19T12:00:00-07:00), else UTC.
4. Corrections and retractions ("the 176 was 175", "that was Tuesday", "delete yesterday's weight", "that wasn't about the migration"): query for what they mean, then append the change.
5. If you can't tell what the owner meant (an implausible number, an ambiguous command), append review.flagged with a short question instead of guessing. The answer comes back as a review.answered event with the question and the original event_id; act on it then. If later events answer it first, act on that and append review.withdrawn (parent_id: the review.flagged event) so the owner isn't asked a settled question.
6. A label.corrected event from the dashboard means the owner fixed a label you made (or missed). Learn the boundary from it; if it shows a rule is missing, amend the collection. collection.* events from the dashboard are changes the owner already made: don't repeat or reverse them.
7. When the owner teaches you something ("w means weight", "I'm on Pacific time", "ignore anything from ~/scratch"), append a rule.added. Background work and future conversations only see what is written down. Follow every entry in <owner_rules>.
8. Many events need nothing more than being stored. Then do nothing.

Starting a collection or adding an include files the last 7 days of events in the background (lookback_days changes the window); an exclude re-checks current members. Leave that backfill to the job rather than doing it yourself, so the owner's next text isn't kept waiting; pass lookback_days: 0 only when the owner asks for no look-back. The job goes to a copy of you as a "Look-back job" message: it comes from the system, not the owner, so do what it asks, including cleaning up records and questions the collection now settles. A "Look-back ... finished" message is that job reporting back; nothing to do.

A large payload arrives as a pointer; read it with query (SELECT substr(payload, 1, 8000) FROM events WHERE id = ?).

Reply with at most one short line summarizing what you did.`;

export function collectionsSection(c: Collection): string {
	const lines = [`- id=${c.id} name=${JSON.stringify(c.name)} kind=${c.kind} version=${c.version}`];
	if (c.kind === "metric") lines[0] += ` metric=${c.metric ?? c.name}${c.unit ? ` unit=${c.unit}` : ""}`;
	for (const r of c.rules) lines.push(`    ${r.kind} (v${r.version}): ${r.text}`);
	return lines.join("\n");
}

export function schemaSection(owner: string, s: Schema): string {
	return [
		`The owner's id is ${owner}; filter by it (tables without an owner column join to one that has it).`,
		"Times are RFC 3339 UTC text; payload and data columns are JSON text (use json_extract).",
		"",
		s.tables.trim(),
		"",
		"Events you may append (type payload: what it does):",
		...s.events.map((e) => `- ${e.type} ${e.payload}: ${e.does}`),
	].join("\n");
}

export const text = (s: string) => ({ output: [{ type: "text" as const, text: s }] });
export const fail = (s: string) => ({ ...text(s), isError: true });

/** Builds the extension. `api` is the Go client. */
export function dataExtension(api: GoApi) {
	const LookBack = lookBackTask();
	// Starts the look-back over the last N days (or a re-check of members).
	const lookBack = (tapi: ToolExecutionApi, ctx: Context, owner: string, collectionId: string, mode: "include" | "exclude", days: number | undefined, triggerEventId: string) => {
		if (days === 0) return;
		const since = new Date(Date.now() - (days ?? DEFAULT_LOOKBACK_DAYS) * 86400_000).toISOString();
		return tapi.createTask(LookBack, { owner, collectionId, mode, since, triggerEventId }, { ownership: { kind: "conversation" }, background: true }, ctx);
	};
	// A refusal from Go is the script's mistake: it rejects in the script with
	// the reason. Anything else is ours and fails the call.
	const refused = (err: unknown) => (err instanceof ApiError && err.status < 500 ? fail(err.message) : undefined);

	const query = defineTool({
		name: "query",
		description: "Run one read-only SQLite statement over the whole store. Returns {columns, rows, truncated}.",
		parameters: Type.Object({
			sql: Type.String(),
			params: Type.Optional(Type.Array(Type.Unknown(), { description: "values for ? placeholders" })),
			limit: Type.Optional(Type.Integer({ minimum: 1, maximum: 1000, description: "max rows; default 200" })),
		}),
		structuredOutputSchema: Type.Object({
			columns: Type.Array(Type.String()),
			rows: Type.Array(Type.Array(Type.Unknown())),
			truncated: Type.Boolean(),
		}),
		callers: ["tools"],
		replay: "safe",
		execute: async (a) => {
			try {
				const res = await api.query(a.sql, a.params ?? [], a.limit);
				return { ...text(JSON.stringify(res)), structuredOutput: res };
			} catch (err) {
				const r = refused(err);
				if (r) return r;
				throw err;
			}
		},
	});

	const append = defineTool({
		name: "append",
		description: "Append one event (a type from <schema>). parent_id is the event that led to it. Returns {id}.",
		parameters: Type.Object({
			type: Type.String(),
			parent_id: Type.String(),
			payload: Type.Record(Type.String(), Type.Unknown()),
		}),
		structuredOutputSchema: Type.Object({ id: Type.String() }),
		callers: ["tools"],
		replay: "safe",
		execute: async (a, tapi, ctx) => {
			try {
				const owner = await ownerOf(tapi, tapi.conversationId, ctx);
				// The tool task id is stable when a replay-safe call reruns after a
				// crash, so Go stores the event once.
				const e = await api.append(owner, a.type, a.payload, { parentId: a.parent_id, idempotencyKey: `tool:${String(tapi.taskId)}` });
				const p = a.payload as { collection_id?: string; include?: string; exclude?: string; lookback_days?: number };
				if (a.type === "collection.started") await lookBack(tapi, ctx, owner, e.id, "include", p.lookback_days, a.parent_id);
				if (a.type === "collection.amended" && p.collection_id && (p.include || p.exclude)) {
					await lookBack(tapi, ctx, owner, p.collection_id, p.exclude ? "exclude" : "include", p.lookback_days, a.parent_id);
				}
				return { ...text(`ok: ${a.type} ${e.id}`), structuredOutput: { id: e.id } };
			} catch (err) {
				const r = refused(err);
				if (r) return r;
				throw err;
			}
		},
	});

	return defineExtension({
		name: "data",
		tasks: [LookBack],
		tools: [query, append],
		sections: [
			// Not the agent's own `instructions` (reserved, and stored per
			// conversation at creation): a section follows the code on deploy.
			section("role", () => INSTRUCTIONS),
			// Deterministic text: each only changes when its data does, so the
			// prompt cache survives every other request (see LEARNINGS.md).
			section("schema", async (input, ctx) => schemaSection(await ownerOf(input.read, input.conversationId, ctx), await api.schema())),
			section("collections", async (input, ctx) => {
				const open = await api.collections(await ownerOf(input.read, input.conversationId, ctx), "open");
				return open.length ? open.map(collectionsSection).join("\n") : "No open collections.";
			}),
			section("owner_rules", async (input, ctx) => {
				const rules = await api.rules(await ownerOf(input.read, input.conversationId, ctx));
				return rules.length ? rules.map((r) => `- ${r.text}`).join("\n") : "None yet.";
			}),
		],
	});
}
