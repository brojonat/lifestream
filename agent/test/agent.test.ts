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
import { handler } from "../src/server.ts";

const ctx = BACKGROUND_CONTEXT;
const OWNER = "01OWNER";

/** A stand-in for Go's internal API: remembers appends, serves collections. */
class FakeGo {
	appends: { body: { owner: string; type: string; parent_id?: string; payload: any }; key: string | undefined }[] = [];
	collections: Collection[] = [];
	rules: { id: string; text: string; source: string }[] = [];
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

function open(responses: Parameters<ReturnType<typeof fauxProvider>["setResponses"]>[0]) {
	const faux = fauxProvider();
	faux.setResponses(responses);
	const models = createModels();
	models.setProvider(faux.provider);
	return Agent.open({ storagePath: join(dir, "agent.db"), models, model: { provider: "faux", modelId: "faux-1" }, api: new GoApi(go.url) }, ctx);
}

test("steering, labeling, values, dedup and restart", async () => {
	const seenPrompts: string[] = [];
	const remember = (msg: ReturnType<typeof fauxAssistantMessage>) => (context: unknown) => {
		seenPrompts.push(JSON.stringify(context));
		return msg;
	};
	let agent = await open([
		// "start tracking my weight"
		remember(fauxAssistantMessage([fauxToolCall("create_collection", { event_id: "E1", name: "weight", kind: "metric", unit: "lb", include: "texts reporting body weight" })], { stopReason: "toolUse" })),
		fauxAssistantMessage([fauxText("started weight")]),
		// "weight 175": the section now lists the collection
		remember(fauxAssistantMessage([
			fauxToolCall("label_event", { event_id: "E2", collection_id: "01DERIVED1", definition_version: 1, reason: "reports weight" }),
			fauxToolCall("log_value", { event_id: "E2", collection_id: "01DERIVED1", value: 175 }),
		], { stopReason: "toolUse" })),
		fauxAssistantMessage([fauxText("filed 175 lb")]),
	]);

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

	agent = await open([]);
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

test("large payloads become pointers", () => {
	const big = { ...sms("E9", ""), type: "artifact.saved", payload: { title: "Postgres notes", html: "x".repeat(POINTER_BYTES) } };
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
