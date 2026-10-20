// The agent's flows against a fake model (pi-ai's faux provider) and a fake
// Go API, so they run in CI without API calls. The model's tool calls are
// code mode scripts, run for real in the QuickJS sandbox.

import assert from "node:assert/strict";
import { mkdtemp, rm } from "node:fs/promises";
import { createServer, type IncomingMessage, type Server } from "node:http";
import type { AddressInfo } from "node:net";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { after, before, test } from "node:test";
import { BACKGROUND_CONTEXT } from "@earendil-works/chord/context";
import { createModels } from "@earendil-works/pi-ai/models";
import { fauxAssistantMessage, fauxProvider, fauxText, fauxToolCall } from "@earendil-works/pi-ai/providers/faux";
import { Agent, describe, POINTER_BYTES } from "../src/agent.ts";
import { type Collection, GoApi, type QueryResult, type StoredEvent } from "../src/goapi.ts";
import { newLogger } from "../src/log.ts";
import { lookBackJob } from "../src/lookback.ts";
import { type LiveSource, type Snapshot, watchOwner } from "../src/live.ts";
import { handler } from "../src/server.ts";

const ctx = BACKGROUND_CONTEXT;
const OWNER = "01OWNER";

/** A stand-in for Go's internal API: remembers appends and queries, serves collections. */
class FakeGo {
	appends: { body: { owner: string; type: string; parent_id?: string; payload: any }; key: string | undefined }[] = [];
	queries: { sql: string; params: unknown[] }[] = [];
	answer: (sql: string) => QueryResult = () => ({ columns: [], rows: [], truncated: false });
	hold?: Promise<void>; // when set, queries wait for it (a slow query, for the live panel)
	collections: Collection[] = [];
	rules: { id: string; text: string; source: string }[] = [];
	events: (StoredEvent & { seq: number })[] = []; // raw events the look-back can find
	members = new Map<string, Set<string>>();
	submitted = new Map<string, StoredEvent & { seq: number }>();
	server!: Server;
	url = "";

	async start() {
		this.server = createServer(async (req, res) => {
			const json = (status: number, body: unknown) => {
				res.writeHead(status, { "Content-Type": "application/json" });
				res.end(JSON.stringify(body));
			};
			const url = new URL(req.url!, "http://x");
			if (req.method === "GET" && url.pathname === "/api/collections") {
				assert.equal(url.searchParams.get("owner"), OWNER);
				return json(200, { collections: this.collections });
			}
			if (req.method === "GET" && url.pathname === "/api/schema") {
				return json(200, { tables: "CREATE TABLE records (event_id TEXT, kind TEXT, data TEXT);", events: [{ type: "record.saved", payload: "{kind: string, data: JSON object}", does: "keeps data" }] });
			}
			if (req.method === "POST" && url.pathname === "/api/query") {
				const q = JSON.parse(await read(req));
				if (/^\s*(delete|update|insert)/i.test(q.sql)) return json(400, { error: "query: attempt to write a readonly database" });
				this.queries.push({ sql: q.sql, params: q.params });
				await this.hold;
				return json(200, this.answer(q.sql));
			}
			if (req.method === "GET" && url.pathname === "/api/events") {
				const after = Number(url.searchParams.get("after") ?? 0);
				const limit = Number(url.searchParams.get("limit") ?? 100);
				return json(200, { events: this.events.filter((e) => e.seq > after).slice(0, limit).map((e) => ({ ...e, payload_size: 10 })) });
			}
			const m = /^\/api\/collections\/(\w+)\/members$/.exec(url.pathname);
			if (req.method === "GET" && m) {
				return json(200, { members: [...(this.members.get(m[1]) ?? [])].map((event_id) => ({ event_id, definition_version: 1 })) });
			}
			const one = /^\/api\/events\/(\w+)$/.exec(url.pathname);
			if (req.method === "GET" && one) {
				// Events a test submitted straight to the agent aren't in the list; serve what it sent.
				const e = this.events.find((x) => x.id === one[1]) ?? this.submitted.get(one[1]) ?? { ...sms(one[1], ""), seq: 0 };
				return json(200, url.searchParams.has("offset") ? { ...e, payload: null, data: JSON.stringify(e.payload) } : e);
			}
			if (req.method === "GET" && url.pathname === "/api/rules") {
				return json(200, { rules: this.rules });
			}
			if (req.method === "POST" && url.pathname === "/api/events") {
				const body = JSON.parse(await read(req));
				if (body.type === "label.applied" && body.payload.definition_version > 1) {
					return json(422, { error: "definition_version 9 is not a version" });
				}
				this.appends.push({ body, key: req.headers["idempotency-key"] as string | undefined });
				const id = `01DERIVED${this.appends.length}`;
				if (body.type === "rule.added") this.rules.push({ id, text: body.payload.text, source: "agent" });
				const set = this.members.get(body.payload?.collection_id) ?? new Set<string>();
				if (body.type === "label.applied") this.members.set(body.payload.collection_id, set.add(body.payload.event_id));
				if (body.type === "label.removed") set.delete(body.payload.event_id);
				if (body.type === "collection.amended") {
					const c = this.collections.find((x) => x.id === body.payload.collection_id);
					if (c) c.version++;
				}
				if (body.type === "collection.started") {
					this.collections.push({ id, name: body.payload.name, kind: body.payload.kind, unit: body.payload.unit, status: "open", version: 1, rules: [{ kind: "include", text: body.payload.include, version: 1 }] });
				}
				return json(201, { id, ...body, source: "agent" });
			}
			json(404, { error: "not found" });
		});
		await new Promise<void>((r) => this.server.listen(0, "127.0.0.1", r));
		this.url = `http://127.0.0.1:${(this.server.address() as AddressInfo).port}`;
	}
}

async function read(req: IncomingMessage) {
	let s = "";
	for await (const c of req) s += c;
	return s;
}

const sms = (id: string, body: string): StoredEvent => ({
	id, owner: OWNER, source: "twilio.sms", type: "message.received", occurred_at: "2026-10-17T07:00:00Z", payload: { Body: body },
});

/** A model turn that runs one code mode script. */
const script = (code: string) => fauxAssistantMessage([fauxToolCall("codemode", { code })], { stopReason: "toolUse" });

/** The newest tool result the model saw, as text. */
const lastResult = (context: { messages: unknown[] }) => {
	const last = context.messages.at(-1) as { role: string; content: unknown; isError?: boolean };
	return `${last.role} ${last.isError} ${JSON.stringify(last.content)}`;
};

let dir: string;
let go: FakeGo;
before(async () => {
	dir = await mkdtemp(join(tmpdir(), "lifestream-agent-"));
	go = new FakeGo();
	await go.start();
});
after(async () => {
	go.server.close();
	await rm(dir, { recursive: true, force: true });
});

let dbs = 0;
function open(responses: Parameters<ReturnType<typeof fauxProvider>["setResponses"]>[0], opts: { db?: string; modelId?: string } = {}) {
	const faux = fauxProvider();
	faux.setResponses(responses);
	const models = createModels();
	models.setProvider(faux.provider);
	const storagePath = join(dir, opts.db ?? `agent-${++dbs}.db`); // each test its own harness state
	return Agent.open({ storagePath, models, model: { provider: "faux", modelId: opts.modelId ?? "faux-1" }, api: new GoApi(go.url) }, ctx);
}

test("steering, labeling, values, dedup and restart", async () => {
	const seen: string[] = [];
	const remember = (msg: ReturnType<typeof fauxAssistantMessage>) => (context: unknown) => {
		seen.push(JSON.stringify(context));
		return msg;
	};
	let agent = await open([
		// "start tracking my weight" (no look-back here; it has its own test)
		remember(script(`await tools.append({ type: "collection.started", parent_id: "E1", payload: { name: "weight", kind: "metric", unit: "lb", include: "texts reporting body weight", lookback_days: 0 } })`)),
		fauxAssistantMessage([fauxText("started weight")]),
		// "weight 175": the section now lists the collection
		remember(script(`
			const w = "01DERIVED1";
			await tools.append({ type: "label.applied", parent_id: "E2", payload: { event_id: "E2", collection_id: w, definition_version: 1, reason: "reports weight" } });
			const { id } = await tools.append({ type: "metric.observed", parent_id: "E2", payload: { collection_id: w, value: 175 } });
			text("logged " + id);
		`)),
		(context) => {
			assert.match(lastResult(context), /logged 01DERIVED3/, "scripts get each append's id back");
			return fauxAssistantMessage([fauxText("filed 175 lb")]);
		},
	], { db: "restart.db" });

	const first = await agent.submit(OWNER, sms("E1", "start tracking my weight"), ctx);
	assert.equal((await first.wait(ctx)).status, "done");
	const second = await agent.submit(OWNER, sms("E2", "weight 175"), ctx);
	assert.equal((await second.wait(ctx)).status, "done");

	assert.deepEqual(go.appends.map((a) => a.body.type), ["collection.started", "label.applied", "metric.observed"]);
	for (const a of go.appends) {
		assert.equal(a.body.owner, OWNER);
		assert.match(a.key ?? "", /^tool:/, "every write carries its nested tool task id as the idempotency key");
	}
	assert.equal(new Set(go.appends.map((a) => a.key)).size, 3, "keys differ per call");
	assert.equal(go.appends[0].body.parent_id, "E1");
	assert.deepEqual(go.appends[2].body.payload, { collection_id: "01DERIVED1", value: 175 });
	const offered = (await (await agent.conversationFor(OWNER, ctx)).agent(ctx)).tools.map((t) => t.name);
	assert.deepEqual(offered, ["codemode"], "the model is offered code mode only");
	assert.match(seen[0], /No open collections/);
	assert.match(seen[0], /The owner's id is 01OWNER/);
	assert.match(seen[0], /- record.saved \{kind: string, data: JSON object\}: keeps data/);
	assert.match(seen[0], /append\(/, "the codemode section declares the tools scripts can call");
	assert.match(seen[1], /id=01DERIVED1 name=\\"weight\\" kind=metric version=1 metric=weight unit=lb/);
	assert.match(seen[1], /include \(v1\): texts reporting body weight/);

	// Redelivery after a crash: same event id, same submission, no model call
	// (the faux queue is empty and would fail).
	const again = await agent.submit(OWNER, sms("E2", "weight 175"), ctx);
	assert.equal(String(again.id), String(second.id));
	const conv = await agent.conversationFor(OWNER, ctx);
	await agent.close(ctx);

	agent = await open([], { db: "restart.db", modelId: "faux-2" });
	const reopened = await agent.conversationFor(OWNER, ctx);
	assert.equal(reopened.id, conv.id, "same conversation after restart");
	assert.equal((await reopened.agent(ctx)).model?.modelId, "faux-2", "a new AGENT_MODEL moves the conversation");
	const afterRestart = await agent.submit(OWNER, sms("E2", "weight 175"), ctx);
	assert.equal(String(afterRestart.id), String(second.id));
	assert.equal(go.appends.length, 3);
	await agent.close(ctx);
});

test("a rejected write throws in the script and the model sees why", async () => {
	let result = "";
	const agent = await open([
		script(`await tools.append({ type: "label.applied", parent_id: "E3", payload: { event_id: "E3", collection_id: "01DERIVED1", definition_version: 9, reason: "x" } })`),
		(context) => {
			result = lastResult(context);
			return fauxAssistantMessage([fauxText("will fix")]);
		},
	]);
	const appended = go.appends.length;
	const sub = await agent.submit(OWNER, sms("E3", "weight 9000"), ctx);
	assert.equal((await sub.wait(ctx)).status, "done");
	await agent.close(ctx);
	assert.equal(go.appends.length, appended, "nothing stored");
	assert.match(result, /^toolResult true .*422 definition_version 9/);
});

test("scripts query, then change data from what they found", async () => {
	go.answer = () => ({ columns: ["event_id", "value"], rows: [["01VALUE1", 176], ["01VALUE2", 177]], truncated: false });
	let result = "";
	const agent = await open([
		script(`
			const r = await tools.query({ sql: "SELECT event_id, value FROM metric_values WHERE collection_id = ? ORDER BY observed_at", params: ["01W"] });
			for (const [id, value] of r.rows) {
				if (value === 176) await tools.append({ type: "metric.observed", parent_id: "E6", payload: { collection_id: "01W", value: 175, replaces: id } });
				else await tools.append({ type: "metric.removed", parent_id: "E6", payload: { value_id: id, reason: "owner retracted it" } });
			}
			try { await tools.query({ sql: "DELETE FROM events" }); } catch (e) { text("refused: " + e.message); }
		`),
		(context) => {
			result = lastResult(context);
			return fauxAssistantMessage([fauxText("fixed")]);
		},
	]);
	const before = go.appends.length;
	const sub = await agent.submit(OWNER, sms("E6", "the 176 was 175; drop the other"), ctx);
	assert.equal((await sub.wait(ctx)).status, "done");
	await agent.close(ctx);
	assert.deepEqual(go.queries.at(-1), { sql: "SELECT event_id, value FROM metric_values WHERE collection_id = ? ORDER BY observed_at", params: ["01W"] });
	assert.deepEqual(go.appends.slice(before).map((a) => [a.body.type, a.body.parent_id, a.body.payload]), [
		["metric.observed", "E6", { collection_id: "01W", value: 175, replaces: "01VALUE1" }],
		["metric.removed", "E6", { value_id: "01VALUE2", reason: "owner retracted it" }],
	]);
	assert.match(result, /refused: .*400 query: attempt to write a readonly database/);
});

test("what the owner teaches becomes a rule the next request sees", async () => {
	let prompt = "";
	const agent = await open([
		script(`await tools.append({ type: "rule.added", parent_id: "E4", payload: { text: "w means weight" } })`),
		(context) => {
			prompt = JSON.stringify(context);
			return fauxAssistantMessage([fauxText("noted")]);
		},
	]);
	const sub = await agent.submit(OWNER, sms("E4", "fyi w means weight"), ctx);
	assert.equal((await sub.wait(ctx)).status, "done");
	await agent.close(ctx);
	const added = go.appends.at(-1)!;
	assert.equal(added.body.type, "rule.added");
	assert.equal(added.body.parent_id, "E4");
	assert.match(prompt, /<owner_rules>\\n- w means weight\\n<\/owner_rules>/);
});

test("look-backs run as a background copy of the steward and report back", async () => {
	go.collections = [];
	go.appends = [];
	go.members = new Map(); // ids restart with appends; don't inherit another test's members
	// What the child's query finds: the owner's recent texts.
	go.answer = () => ({ columns: ["id", "body"], rows: [["L1", "weight 176"], ["L2", "groceries: eggs"], ["L4", "dog weighed 40"]], truncated: false });
	const prompts = { parent: [] as string[], child: [] as string[] };
	// One responder for every model call, in either conversation: it acts on
	// the newest user message the way the real model would.
	const respond = (context: { messages: { role: string; content: unknown }[] }) => {
		const all = JSON.stringify(context);
		const child = all.includes("Look-back job (from the system"); // the job message, not the prompt that mentions it
		(child ? prompts.child : prompts.parent).push(all);
		const last = context.messages.at(-1)!;
		if (last.role === "toolResult") return fauxAssistantMessage([fauxText(child ? "checked 3, changed 2" : "done")]);
		const said = JSON.stringify(last.content);
		if (said.includes("start tracking weight")) {
			return script(`await tools.append({ type: "collection.started", parent_id: "L5", payload: { name: "weight", kind: "metric", unit: "lb", include: "weight readings" } })`);
		}
		if (said.includes("don't include the dog")) {
			return script(`await tools.append({ type: "collection.amended", parent_id: "L6", payload: { collection_id: "01DERIVED1", exclude: "pets' weight" } })`);
		}
		if (said.includes("Look-back job") && said.includes("occurred at or after")) {
			return script(`
				const { rows } = await tools.query({ sql: "SELECT id, json_extract(payload, '$.Body') FROM events WHERE owner = ? AND occurred_at >= ?", params: ["01OWNER", "2026-10-10T00:00:00Z"] });
				for (const [id, body] of rows) {
					if (!/weigh/.test(body)) continue;
					await tools.append({ type: "label.applied", parent_id: id, payload: { event_id: id, collection_id: "01DERIVED1", definition_version: 1, reason: "look-back: a weight" } });
					await tools.append({ type: "metric.observed", parent_id: id, payload: { collection_id: "01DERIVED1", value: Number(/\\d+/.exec(body)[0]) } });
				}
			`);
		}
		if (said.includes("Look-back job") && said.includes("Re-check")) {
			return script(`await tools.append({ type: "label.removed", parent_id: "L6", payload: { event_id: "L4", collection_id: "01DERIVED1", definition_version: 2, reason: "the dog" } })`);
		}
		return fauxAssistantMessage([fauxText("ok")]); // e.g. a look-back's report
	};
	const agent = await open(Array(30).fill(respond));
	const conv = await agent.conversationFor(OWNER, ctx);
	const until = async (f: () => boolean) => {
		for (let i = 0; i < 300 && !f(); i++) await new Promise((r) => setTimeout(r, 10));
		assert.ok(f(), `timed out; appends=${go.appends.map((a) => a.body.type)}; parent saw: ${prompts.parent.at(-1)?.slice(-400)}`);
	};
	const reported = (n: number) => prompts.parent.filter((p) => p.includes("finished: checked 3, changed 2")).length >= n;

	await (await agent.submit(OWNER, sms("L5", "start tracking weight"), ctx)).wait(ctx);
	// The look-back is a background task: wait for its report, not for the conversation.
	await until(() => reported(1));
	await conv.waitForIdle(ctx);
	assert.deepEqual(go.appends.map((a) => [a.body.type, a.body.parent_id]), [
		["collection.started", "L5"],
		["label.applied", "L1"],
		["metric.observed", "L1"],
		["label.applied", "L4"],
		["metric.observed", "L4"],
	]);
	assert.deepEqual(go.appends.filter((a) => a.body.type === "metric.observed").map((a) => a.body.payload.value), [176, 40]);
	assert.match(prompts.child[0], /The owner's id is 01OWNER/, "the child has the steward's sections");
	assert.match(prompts.child[0], /id=01DERIVED1 name=\\"weight\\"/);
	assert.ok(!prompts.parent.some((p) => p.includes("Look-back job (from the system")), "the job runs in its own conversation");
	assert.match(prompts.parent.at(-1)!, /Look-back for collection 01DERIVED1 finished: checked 3, changed 2\. No action needed\./);

	await (await agent.submit(OWNER, sms("L6", "don't include the dog in weight"), ctx)).wait(ctx);
	await until(() => go.appends.some((a) => a.body.type === "label.removed") && reported(2));
	await conv.waitForIdle(ctx);
	assert.deepEqual(go.appends.at(-1)!.body.payload, { event_id: "L4", collection_id: "01DERIVED1", definition_version: 2, reason: "the dog" });
	await agent.close(ctx);
});

test("look-back jobs say what to do", () => {
	const job = { owner: OWNER, collectionId: "C1", since: "2026-10-13T00:00:00Z", triggerEventId: "T1" };
	assert.match(lookBackJob({ ...job, mode: "include" }), /^Look-back job \(from the system, not an event\)\. Event T1 changed collection C1.*occurred at or after 2026-10-13T00:00:00Z/s);
	assert.match(lookBackJob({ ...job, mode: "exclude" }), /Re-check the collection's current members.*label\.removed/s);
});

test("large payloads become pointers", () => {
	const big = { ...sms("E9", ""), type: "note", payload: { title: "Postgres notes", html: "x".repeat(POINTER_BYTES) } };
	const text = describe(big);
	assert.match(text, /Payload is \d+ bytes, title "Postgres notes"\. Query it to read it\./);
	assert.ok(!text.includes("xxxx"));
	assert.match(describe(sms("E1", "weight 175")), /"Body":"weight 175"/);
});

test("GET /live streams what the agent is doing: the event, the script, its calls", async () => {
	let release!: () => void;
	go.hold = new Promise<void>((r) => (release = r));
	go.answer = () => ({ columns: ["n"], rows: [[3]], truncated: false });
	const agent = await open([
		script(`const r = await tools.query({ sql: "SELECT count(*) AS n FROM events" }); text("count " + r.rows[0][0]);`),
		fauxAssistantMessage([fauxText("counted 3 events")]),
	]);
	const quiet = newLogger({ LOG_LEVEL: "error" }, { write: () => true });
	const srv = createServer(handler(agent, quiet, ctx));
	await new Promise<void>((r) => srv.listen(0, "127.0.0.1", r));
	const base = `http://127.0.0.1:${(srv.address() as AddressInfo).port}`;
	const abort = new AbortController();
	try {
		assert.equal((await fetch(`${base}/live`)).status, 400, "owner is required");
		const res = await fetch(`${base}/live?owner=${OWNER}`, { signal: abort.signal });
		assert.equal(res.headers.get("content-type"), "text/event-stream");
		const reader = res.body!.getReader();
		let buf = "";
		/** Reads snapshots until one passes `want`. */
		const next = async (want: (s: Snapshot) => boolean) => {
			for (;;) {
				const i = buf.indexOf("\n\n");
				if (i >= 0) {
					const s = JSON.parse(buf.slice(0, i).replace(/^data: /, "")) as Snapshot;
					buf = buf.slice(i + 2);
					if (want(s)) return s;
					continue;
				}
				const { value, done } = await reader.read();
				if (done) throw new Error("stream ended");
				buf += new TextDecoder().decode(value);
			}
		};
		assert.equal((await next(() => true)).main.busy, false, "the first snapshot comes at once");

		await agent.submit(OWNER, sms("E11", "how many events?"), ctx);
		const working = await next((s) => s.main.calls.some((c) => c.name === "query"));
		assert.equal(working.main.busy, true);
		assert.match(working.main.handling ?? "", /how many events\?/);
		assert.match(working.main.script ?? "", /tools\.query/);
		assert.deepEqual(working.main.calls.map((c) => [c.name, c.status]), [["query", "running"]]);
		assert.match(working.main.calls[0].args, /SELECT count/);

		release();
		const idle = await next((s) => !s.main.busy && s.main.last === "counted 3 events");
		assert.match(idle.main.script ?? "", /tools\.query/, "the last script stays on the panel");
		assert.deepEqual(idle.main.calls.map((c) => [c.name, c.status]), [["query", "done"]], "finished calls come from the script's result");
		assert.match(idle.main.output ?? "", /^Script completed\n.*\ncount 3$/s);
		assert.equal(typeof idle.main.cost_usd, "number");
	} finally {
		go.hold = undefined;
		abort.abort();
		srv.close();
		await (await agent.conversationFor(OWNER, ctx)).waitForIdle(ctx);
		await agent.close(ctx);
	}
});

test("look-back subagents appear in the live snapshot and leave with their task", async () => {
	// A fake source: states whose value tests set and whose subscribers they wake.
	const state = <T,>(value: T) => {
		const subs = new Set<() => void>();
		return { value, subscribe: (f: () => void) => (subs.add(f), () => subs.delete(f)), dispose() {}, wake: () => subs.forEach((f) => f()) };
	};
	const view = (id: number, docs: Record<string, unknown>, model: unknown[] = []) =>
		({ conversation: { id }, entries: [{ model }], docs }) as never;
	const main = state(view(1, {}));
	const child = state(view(2, { "pi.live": { run: { taskId: 9, inputs: [] } } }, [{ role: "user", content: "Look-back job (from the system, not an event). ..." }]));
	const graph = state({ tasks: {} as Record<string, unknown> });
	const src = {
		conversationId: async () => 1,
		viewState: async (id: unknown) => (Number(id) === 1 ? main : child),
		taskGraph: async () => graph,
	} as unknown as LiveSource;
	const seen: Snapshot[] = [];
	const stop = await watchOwner(src, OWNER, ctx, (s) => seen.push(s), 1);
	const settle = () => new Promise((r) => setTimeout(r, 20));
	assert.deepEqual(seen.at(-1)!.lookbacks, []);

	graph.value = { tasks: { "9": { id: 9, kind: "lifestream.lookback", conversationId: 1, background: true, abortRequested: false, state: { status: "running", phase: "run" }, conversations: [2] } } };
	graph.wake();
	await settle();
	const lb = seen.at(-1)!.lookbacks;
	assert.deepEqual(lb.map((l) => [l.task_id, l.kind, l.phase]), [["9", "lifestream.lookback", "run"]]);
	assert.equal(lb[0].child?.busy, true);
	assert.match(lb[0].child?.handling ?? "", /^Look-back job/);

	graph.value = { tasks: {} };
	graph.wake();
	await settle();
	assert.deepEqual(seen.at(-1)!.lookbacks, []);
	stop();
});

test("POST /submit validates and admits", async () => {
	const agent = await open([fauxAssistantMessage([fauxText("noted")])]);
	const quiet = newLogger({ LOG_LEVEL: "error" }, { write: () => true });
	const srv = createServer(handler(agent, quiet, ctx));
	await new Promise<void>((r) => srv.listen(0, "127.0.0.1", r));
	const base = `http://127.0.0.1:${(srv.address() as AddressInfo).port}`;
	try {
		assert.equal((await fetch(`${base}/healthz`)).status, 200);
		assert.equal((await fetch(`${base}/submit`, { method: "POST", body: "{" })).status, 400);
		assert.equal((await fetch(`${base}/submit`, { method: "POST", body: JSON.stringify({ owner: OWNER }) })).status, 400);
		const res = await fetch(`${base}/submit`, { method: "POST", body: JSON.stringify({ owner: OWNER, event: sms("E10", "note to self") }) });
		assert.equal(res.status, 202);
		assert.ok(((await res.json()) as { submission?: string }).submission);
	} finally {
		srv.close();
		await (await agent.conversationFor(OWNER, ctx)).waitForIdle(ctx);
		await agent.close(ctx);
	}
});
