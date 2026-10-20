// The agent's flows against a fake model (pi-ai's faux provider) and a fake
// Go API, so they run in CI without API calls.

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
import { type Collection, GoApi, type StoredEvent } from "../src/goapi.ts";
import { newLogger } from "../src/log.ts";
import { type Judge, judgePrompt, parseVerdicts } from "../src/lookback.ts";
import { handler } from "../src/server.ts";

const ctx = BACKGROUND_CONTEXT;
const OWNER = "01OWNER";

/** A stand-in for Go's internal API: remembers appends, serves collections. */
class FakeGo {
	appends: { body: { owner: string; type: string; parent_id?: string; payload: any }; key: string | undefined }[] = [];
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
			if (req.method === "GET" && url.pathname === "/api/events") {
				const after = Number(url.searchParams.get("after") ?? 0);
				const limit = Number(url.searchParams.get("limit") ?? 100);
				return json(200, { events: this.events.filter((e) => e.seq > after).slice(0, limit).map((e) => ({ ...e, payload_size: 10 })) });
			}
			const m = /^\/api\/collections\/(\w+)\/members$/.exec(url.pathname);
			if (req.method === "GET" && m) {
				return json(200, { members: [...(this.members.get(m[1]) ?? [])].map((event_id) => ({ event_id, definition_version: 1 })) });
			}
			const kids = /^\/api\/events\/(\w+)\/children$/.exec(url.pathname);
			if (req.method === "GET" && kids) {
				return json(200, { events: this.appends.filter((a) => a.body.parent_id === kids[1]).map((a) => ({ type: a.body.type })) });
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
function open(responses: Parameters<ReturnType<typeof fauxProvider>["setResponses"]>[0], opts: { judge?: Judge; db?: string } = {}) {
	const faux = fauxProvider();
	faux.setResponses(responses);
	const models = createModels();
	models.setProvider(faux.provider);
	const storagePath = join(dir, opts.db ?? `agent-${++dbs}.db`); // each test its own harness state
	return Agent.open({ storagePath, models, model: { provider: "faux", modelId: "faux-1" }, api: new GoApi(go.url), lookBack: { judge: opts.judge, retryBaseMs: 20 } }, ctx);
}

test("steering, labeling, values, dedup and restart", async () => {
	const seenPrompts: string[] = [];
	const remember = (msg: ReturnType<typeof fauxAssistantMessage>) => (context: unknown) => {
		seenPrompts.push(JSON.stringify(context));
		return msg;
	};
	let agent = await open([
		// "start tracking my weight" (no look-back here; it has its own test)
		remember(fauxAssistantMessage([fauxToolCall("create_collection", { event_id: "E1", name: "weight", kind: "metric", unit: "lb", include: "texts reporting body weight", lookback_days: 0 })], { stopReason: "toolUse" })),
		fauxAssistantMessage([fauxText("started weight")]),
		// "weight 175": the section now lists the collection
		remember(fauxAssistantMessage([
			fauxToolCall("label_event", { event_id: "E2", collection_id: "01DERIVED1", definition_version: 1, reason: "reports weight" }),
			fauxToolCall("log_value", { event_id: "E2", collection_id: "01DERIVED1", value: 175 }),
		], { stopReason: "toolUse" })),
		fauxAssistantMessage([fauxText("filed 175 lb")]),
	], { db: "restart.db" });

	const first = await agent.submit(OWNER, sms("E1", "start tracking my weight"), ctx);
	assert.equal((await first.wait(ctx)).status, "done");
	const second = await agent.submit(OWNER, sms("E2", "weight 175"), ctx);
	assert.equal((await second.wait(ctx)).status, "done");

	assert.deepEqual(go.appends.map((a) => a.body.type), ["collection.started", "label.applied", "metric.observed"]);
	for (const a of go.appends) {
		assert.equal(a.body.owner, OWNER);
		assert.match(a.key ?? "", /^tool:/, "every write carries the tool task id as its idempotency key");
	}
	assert.equal(new Set(go.appends.map((a) => a.key)).size, 3, "keys differ per tool call");
	assert.equal(go.appends[0].body.parent_id, "E1");
	assert.deepEqual(go.appends[2].body.payload, { collection_id: "01DERIVED1", value: 175 });
	assert.match(seenPrompts[0], /No open collections/);
	assert.match(seenPrompts[1], /id=01DERIVED1 name=\\"weight\\" kind=metric version=1 metric=weight unit=lb/);
	assert.match(seenPrompts[1], /include \(v1\): texts reporting body weight/);

	// Redelivery after a crash: same event id, same submission, no model call
	// (the faux queue is empty and would fail).
	const again = await agent.submit(OWNER, sms("E2", "weight 175"), ctx);
	assert.equal(String(again.id), String(second.id));
	const conv = await agent.conversationFor(OWNER, ctx);
	await agent.close(ctx);

	agent = await open([], { db: "restart.db" });
	assert.equal((await agent.conversationFor(OWNER, ctx)).id, conv.id, "same conversation after restart");
	const afterRestart = await agent.submit(OWNER, sms("E2", "weight 175"), ctx);
	assert.equal(String(afterRestart.id), String(second.id));
	assert.equal(go.appends.length, 3);
	await agent.close(ctx);
});

test("a rejected write goes back to the model as a tool error", async () => {
	let toolResult = "";
	const agent = await open([
		fauxAssistantMessage([fauxToolCall("label_event", { event_id: "E3", collection_id: "01DERIVED1", definition_version: 9, reason: "x" })], { stopReason: "toolUse" }),
		(context) => {
			const last = context.messages.at(-1) as { role: string; content: unknown; isError?: boolean };
			toolResult = `${last.role} ${last.isError} ${JSON.stringify(last.content)}`;
			return fauxAssistantMessage([fauxText("will fix")]);
		},
	]);
	const appended = go.appends.length;
	const sub = await agent.submit(OWNER, sms("E3", "weight 9000"), ctx);
	assert.equal((await sub.wait(ctx)).status, "done");
	await agent.close(ctx);
	assert.equal(go.appends.length, appended, "nothing stored");
	assert.match(toolResult, /^toolResult true .*rejected: .*422 definition_version 9/);
});

test("a reading the owner dates goes out with observed_at", async () => {
	const agent = await open([
		fauxAssistantMessage([fauxToolCall("log_value", { event_id: "E5", collection_id: "01DERIVED1", value: 177.4, observed_at: "2026-10-19T12:00:00Z" })], { stopReason: "toolUse" }),
		fauxAssistantMessage([fauxText("filed 177.4 lb for 10-19")]),
	]);
	const sub = await agent.submit(OWNER, sms("E5", "my weight yesterday 10-19 was 177.4"), ctx);
	assert.equal((await sub.wait(ctx)).status, "done");
	await agent.close(ctx);
	const logged = go.appends.at(-1)!;
	assert.equal(logged.body.type, "metric.observed");
	assert.deepEqual(logged.body.payload, { collection_id: "01DERIVED1", value: 177.4, observed_at: "2026-10-19T12:00:00Z" });
});

test("what the owner teaches becomes a rule the next request sees", async () => {
	let prompt = "";
	const agent = await open([
		fauxAssistantMessage([fauxToolCall("add_rule", { event_id: "E4", text: "w means weight" })], { stopReason: "toolUse" }),
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

test("starting a collection labels the last 7 days in the background; an exclude re-checks members", async () => {
	go.collections = [];
	go.appends = [];
	go.members = new Map(); // ids restart with appends; don't inherit another test's members
	const now = Date.now();
	const raw = (id: string, body: string, daysAgo: number, seq: number) =>
		({ ...sms(id, body), seq, occurred_at: new Date(now - daysAgo * 86400_000).toISOString() });
	go.events = [
		raw("L1", "weight 176", 3, 101),
		raw("L2", "groceries: eggs", 2, 102),
		{ ...raw("L3", "", 2, 103), source: "agent", type: "label.applied" }, // derived: never judged
		raw("L4", "dog weighed 40", 1, 104),
		raw("L5", "start tracking weight", 0, 105), // the trigger itself
	];
	const judged: string[][] = [];
	let failures = 1; // the first call fails, as if the model API hiccuped; the task must retry, not die
	const judge: Judge = async (c, mode, events) => {
		if (failures-- > 0) throw new Error("model overloaded");
		judged.push([mode, ...events.map((e) => e.id)]);
		return events.map((e) => {
			const text = JSON.stringify(e.payload);
			const member = mode === "include" ? /weigh/.test(text) : !/dog/.test(text);
			return { event_id: e.id, member, value: Number(/(\d+)/.exec(text)?.[1]), reason: "test" };
		});
	};
	const agent = await open([
		fauxAssistantMessage([fauxToolCall("create_collection", { event_id: "L5", name: "weight", kind: "metric", unit: "lb", include: "weight readings" })], { stopReason: "toolUse" }),
		fauxAssistantMessage([fauxText("started")]),
		fauxAssistantMessage([fauxText("ok")]), // the look-back's report
		fauxAssistantMessage([fauxToolCall("amend_collection", { event_id: "L6", collection_id: "01DERIVED1", exclude: "pets' weight" })], { stopReason: "toolUse" }),
		fauxAssistantMessage([fauxText("amended")]),
		fauxAssistantMessage([fauxText("ok")]), // the re-check's report
	], { judge });
	const conv = await agent.conversationFor(OWNER, ctx);
	await (await agent.submit(OWNER, sms("L5", "start tracking weight"), ctx)).wait(ctx);
	const until = async (f: () => boolean) => {
		for (let i = 0; i < 200 && !f(); i++) await new Promise((r) => setTimeout(r, 10));
		assert.ok(f(), `timed out; appends=${go.appends.map((a) => a.body.type)} judged=${JSON.stringify(judged)}`);
	};
	// The look-back is a background task: wait for its writes, not for the conversation.
	await until(() => go.appends.filter((a) => a.body.type === "label.applied").length === 2 && go.appends.filter((a) => a.body.type === "metric.observed").length === 2);
	await conv.waitForIdle(ctx);
	assert.deepEqual(judged[0], ["include", "L1", "L2", "L4"]);
	const labels = go.appends.filter((a) => a.body.type === "label.applied");
	assert.deepEqual(labels.map((a) => a.body.payload.event_id), ["L1", "L4"]);
	assert.match(labels[0].key ?? "", /^lookback:01DERIVED1:v1:L1:label$/);
	assert.deepEqual(go.appends.filter((a) => a.body.type === "metric.observed").map((a) => a.body.payload.value), [176, 40]);

	await (await agent.submit(OWNER, sms("L6", "don't include the dog in weight"), ctx)).wait(ctx);
	await until(() => go.appends.some((a) => a.body.type === "label.removed"));
	await conv.waitForIdle(ctx);
	assert.deepEqual(judged[1], ["exclude", "L1", "L4"]);
	const removed = go.appends.filter((a) => a.body.type === "label.removed");
	assert.deepEqual(removed.map((a) => a.body.payload.event_id), ["L4"]);
	assert.equal(removed[0].body.payload.definition_version, 2);
	await agent.close(ctx);
});

test("judge prompt and verdict parsing", () => {
	const c = { id: "C", name: "weight", kind: "metric" as const, unit: "lb", status: "open" as const, version: 2, rules: [{ kind: "include" as const, text: "weights", version: 1 }, { kind: "exclude" as const, text: "the dog", version: 2 }] };
	const p = judgePrompt(c, "include", [sms("A", "weight 175")]);
	assert.match(p, /- include: weights\n- exclude: the dog/);
	assert.match(p, /"value": the number the event reports for weight in lb/);
	assert.match(p, /id=A type=message.received/);
	assert.deepEqual(parseVerdicts('Sure!\n[{"event_id":"A","member":true,"value":175},{"bogus":1}]\nDone.'), [{ event_id: "A", member: true, value: 175 }]);
	assert.throws(() => parseVerdicts("no idea"), /no JSON array/);
});

test("large payloads become pointers", () => {
	const big = { ...sms("E9", ""), type: "note", payload: { title: "Postgres notes", html: "x".repeat(POINTER_BYTES) } };
	const text = describe(big);
	assert.match(text, /Payload is \d+ bytes, title "Postgres notes"\. Use read_event/);
	assert.ok(!text.includes("xxxx"));
	assert.match(describe(sms("E1", "weight 175")), /"Body":"weight 175"/);
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
