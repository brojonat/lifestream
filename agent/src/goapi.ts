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
