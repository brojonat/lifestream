// Client for the Go service's internal API. This is the agent's only way to
// read or write data; it never opens Go's database.

export type Rule = { kind: "include" | "exclude"; text: string; version: number };

export type Collection = {
	id: string;
	name: string;
	kind: "set" | "metric";
	metric?: string;
	unit?: string;
	status: "open" | "closed";
	version: number;
	lookback_days?: number;
	rules: Rule[];
};

export type OwnerRule = { id: string; text: string; source: string };

export type StoredEvent = {
	seq?: number;
	id: string;
	owner: string;
	source: string;
	type: string;
	occurred_at: string;
	parent_id?: string;
	payload: unknown;
};

/** A non-2xx answer from the Go API. `status` 422 means the event contradicts current state. */
export class ApiError extends Error {
	readonly status: number;
	constructor(status: number, message: string) {
		super(message);
		this.status = status;
	}
}

export class GoApi {
	readonly #base: string;
	readonly #fetch: typeof fetch;

	constructor(baseUrl: string, fetchImpl: typeof fetch = fetch) {
		this.#base = baseUrl.replace(/\/$/, "");
		this.#fetch = fetchImpl;
	}

	async collections(owner: string, status?: "open" | "closed"): Promise<Collection[]> {
		const q = new URLSearchParams({ owner });
		if (status) q.set("status", status);
		const body = (await this.#request("GET", `/api/collections?${q}`)) as { collections: Collection[] };
		return body.collections;
	}

	async rules(owner: string): Promise<OwnerRule[]> {
		const body = (await this.#request("GET", `/api/rules?${new URLSearchParams({ owner })}`)) as { rules: OwnerRule[] };
		return body.rules;
	}

	/** Events that occurred at or after `since`, in log order after seq `after`. Big payloads come back null. */
	/** `dispatched` keeps only what the dispatcher sends the agent: raw events, minus each session's tail. */
	async events(owner: string, opts: { since?: string; after?: number; limit?: number; maxPayload?: number; dispatched?: boolean }): Promise<(StoredEvent & { payload_size: number })[]> {
		const q = new URLSearchParams({ owner });
		if (opts.since) q.set("since", opts.since);
		if (opts.after) q.set("after", String(opts.after));
		if (opts.limit) q.set("limit", String(opts.limit));
		if (opts.maxPayload) q.set("max_payload", String(opts.maxPayload));
		if (opts.dispatched) q.set("dispatched", "1");
		const body = (await this.#request("GET", `/api/events?${q}`)) as { events: (StoredEvent & { payload_size: number })[] };
		return body.events;
	}

	/** Ids of collection members with no redaction.created yet, oldest first. */
	async unredacted(owner: string, limit = 100): Promise<string[]> {
		const body = (await this.#request("GET", `/api/unredacted?${new URLSearchParams({ owner, limit: String(limit) })}`)) as { events: string[] };
		return body.events;
	}

	/** Events derived from this one (payloads over 4 KB come back null). */
	async children(id: string): Promise<StoredEvent[]> {
		const body = (await this.#request("GET", `/api/events/${encodeURIComponent(id)}/children`)) as { events: StoredEvent[] };
		return body.events;
	}

	async members(collectionId: string): Promise<{ event_id: string; definition_version: number }[]> {
		const body = (await this.#request("GET", `/api/collections/${encodeURIComponent(collectionId)}/members`)) as {
			members: { event_id: string; definition_version: number }[];
		};
		return body.members;
	}

	/** One event; with offset/limit, a byte range of its payload as `data`. */
	async event(id: string, range?: { offset: number; limit: number }): Promise<unknown> {
		const q = range ? `?offset=${range.offset}&limit=${range.limit}` : "";
		return this.#request("GET", `/api/events/${encodeURIComponent(id)}${q}`);
	}

	/** Append a derived event. The idempotency key makes a replayed tool call safe. */
	async append(
		owner: string,
		type: string,
		payload: unknown,
		opts: { parentId?: string; idempotencyKey: string },
	): Promise<StoredEvent> {
		const body: Record<string, unknown> = { owner, type, payload };
		if (opts.parentId) body.parent_id = opts.parentId;
		return (await this.#request("POST", "/api/events", body, { "Idempotency-Key": opts.idempotencyKey })) as StoredEvent;
	}

	async #request(method: string, path: string, body?: unknown, headers: Record<string, string> = {}): Promise<unknown> {
		const res = await this.#fetch(this.#base + path, {
			method,
			headers: body === undefined ? headers : { "Content-Type": "application/json", ...headers },
			body: body === undefined ? undefined : JSON.stringify(body),
		});
		const text = await res.text();
		if (!res.ok) {
			let msg = text;
			try {
				msg = (JSON.parse(text) as { error?: string }).error ?? text;
			} catch {}
			throw new ApiError(res.status, `${method} ${path}: ${res.status} ${msg}`);
		}
		return JSON.parse(text);
	}
}
