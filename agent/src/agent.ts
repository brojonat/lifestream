// The harness: one Pi Durable conversation per owner, stored in SQLite.
// Raw events arrive from Go's dispatcher and are submitted with the event id
// as requestId, so a redelivered event is never processed twice.

import type { Context } from "@earendil-works/chord/context";
import type { Models } from "@earendil-works/pi-ai/models";
import { type Conversation, type ConversationId, configure, createRegistry, defineDoc, Harness } from "@earendil-works/pi-durable";
import { openNodeSqliteStorage } from "@earendil-works/pi-durable/storage/sqlite/node";
import { collectionsExtension, OwnerDoc } from "./collections.ts";
import type { GoApi, StoredEvent } from "./goapi.ts";
import { rulesExtension } from "./rules.ts";

/** owner -> conversation id; conversations have no keys of their own. */
const Owners = defineDoc<{ byOwner: Record<string, number> }>({
	kind: "lifestream.owners",
	version: 1,
	scope: "session",
	initial: () => ({ byOwner: {} }),
});

/** Payloads bigger than this reach the model as a pointer (read_event fetches them). */
export const POINTER_BYTES = 4096;

/** Sources whose events are the owner talking; they steer a busy conversation. */
const OWNER_SOURCES = new Set(["twilio.sms", "dashboard"]);

export type AgentOptions = {
	storagePath: string;
	models: Models;
	model: { provider: string; modelId: string };
	api: GoApi;
	onReport?: (err: unknown) => void;
};

export class Agent {
	readonly #harness: Harness;
	readonly #model: AgentOptions["model"];

	private constructor(harness: Harness, model: AgentOptions["model"]) {
		this.#harness = harness;
		this.#model = model;
	}

	static async open(opts: AgentOptions, ctx: Context): Promise<Agent> {
		const registry = createRegistry();
		registry.install(collectionsExtension(opts.api));
		registry.install(rulesExtension(opts.api));
		const harness = await Harness.open(
			await openNodeSqliteStorage(opts.storagePath),
			{ models: opts.models, registry, onReport: opts.onReport },
			ctx,
		);
		harness.resume(); // finish work the last process left behind
		return new Agent(harness, opts.model);
	}

	/** Resolves if the harness fails; the host should exit and be restarted. */
	get closed() {
		return this.#harness.closed;
	}

	/** The owner's conversation, created on their first event. */
	async conversationFor(owner: string, ctx: Context): Promise<Conversation> {
		const id = await this.#harness.commit(async (tx) => {
			const owners = await tx.doc(Owners);
			const existing = owners.byOwner[owner];
			if (existing !== undefined) return existing as ConversationId;
			const created = await tx.createConversation({ ownership: { kind: "ownerless" } });
			await configure(tx, created.id, { model: this.#model });
			const doc = await tx.doc(OwnerDoc, created.id);
			doc.owner = owner;
			owners.byOwner[owner] = created.id;
			return created.id;
		}, ctx);
		const conv = await this.#harness.conversation(id, ctx);
		if (!conv) throw new Error(`conversation ${id} for owner ${owner} vanished`);
		return conv;
	}

	/** Admit one event. Returns once it is durably queued, not when the model is done. */
	async submit(owner: string, event: StoredEvent, ctx: Context) {
		const conv = await this.conversationFor(owner, ctx);
		return conv.submit(
			{
				type: "input",
				content: describe(event),
				requestId: event.id,
				whenBusy: OWNER_SOURCES.has(event.source) ? "steer" : "followUp",
			},
			ctx,
		);
	}

	async close(ctx: Context) {
		await this.#harness.close(ctx);
	}
}

/** The text the model sees for an event. */
export function describe(e: StoredEvent): string {
	const head = `Event ${e.id}: ${e.type} from ${e.source} at ${e.occurred_at}${e.parent_id ? ` (parent ${e.parent_id})` : ""}`;
	const payload = JSON.stringify(e.payload ?? null);
	if (payload.length > POINTER_BYTES) {
		const title = (e.payload as { title?: unknown } | null)?.title;
		return `${head}\nPayload is ${payload.length} bytes${typeof title === "string" ? `, title ${JSON.stringify(title)}` : ""}. Use read_event to see it.`;
	}
	return `${head}\n${payload}`;
}
