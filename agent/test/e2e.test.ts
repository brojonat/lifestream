// End to end against the real Go service: Go's dispatcher -> this agent
// (faux model) -> Go's internal API. Skipped unless ../bin/lifestream exists
// (`mise run build`).

import assert from "node:assert/strict";
import { execFileSync, spawn } from "node:child_process";
import { existsSync } from "node:fs";
import { mkdtemp, rm } from "node:fs/promises";
import { createServer } from "node:http";
import type { AddressInfo } from "node:net";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";
import { BACKGROUND_CONTEXT } from "@earendil-works/chord/context";
import { createModels } from "@earendil-works/pi-ai/models";
import { fauxAssistantMessage, fauxProvider, fauxText, fauxToolCall } from "@earendil-works/pi-ai/providers/faux";
import { Agent } from "../src/agent.ts";
import { GoApi } from "../src/goapi.ts";
import { newLogger } from "../src/log.ts";
import { handler } from "../src/server.ts";

const BIN = join(import.meta.dirname, "..", "..", "bin", "lifestream");
const ctx = BACKGROUND_CONTEXT;

async function freePort() {
	const s = createServer();
	await new Promise<void>((r) => s.listen(0, "127.0.0.1", r));
	const port = (s.address() as AddressInfo).port;
	await new Promise((r) => s.close(r));
	return port;
}

async function until<T>(what: string, f: () => Promise<T | undefined>, ms = 10000): Promise<T> {
	const end = Date.now() + ms;
	for (;;) {
		try {
			const v = await f();
			if (v !== undefined) return v;
		} catch {}
		if (Date.now() > end) throw new Error(`timed out waiting for ${what}`);
		await new Promise((r) => setTimeout(r, 50));
	}
}

// The id of the event in the newest user message.
const eventId = (context: { messages: { role: string; content: unknown }[] }) => {
	const users = context.messages.filter((m) => m.role === "user");
	return /Event (\w+):/.exec(JSON.stringify(users.at(-1)?.content))![1];
};

type Responses = Parameters<ReturnType<typeof fauxProvider>["setResponses"]>[0];

/** A model turn that runs one code mode script. */
const script = (code: string) => fauxAssistantMessage([fauxToolCall("codemode", { code })], { stopReason: "toolUse" });

/** Go (real binary, its own data dir) plus this agent with a faux model, wired as in the pod. */
async function stack(responses: Responses) {
	const dir = await mkdtemp(join(tmpdir(), "lifestream-e2e-"));
	const user = JSON.parse(execFileSync(BIN, ["users", "add", "--data-dir", dir, "--name", "me", "--phone", "+15555550100"], { encoding: "utf8" }));
	const [webPort, apiPort, agentPort] = [await freePort(), await freePort(), await freePort()];
	const goApi = `http://127.0.0.1:${apiPort}`;
	const faux = fauxProvider();
	faux.setResponses(responses);
	const models = createModels();
	models.setProvider(faux.provider);
	const agent = await Agent.open({ storagePath: join(dir, "agent.db"), models, model: { provider: "faux", modelId: "faux-1" }, api: new GoApi(goApi) }, ctx);
	const agentServer = createServer(handler(agent, newLogger({ LOG_LEVEL: "error" }), ctx));
	await new Promise<void>((r) => agentServer.listen(agentPort, "127.0.0.1", r));
	const go = spawn(BIN, ["serve", "--data-dir", dir, "--addr", `127.0.0.1:${webPort}`, "--api-addr", `127.0.0.1:${apiPort}`,
		"--agent-url", `http://127.0.0.1:${agentPort}`], { stdio: ["ignore", "ignore", "pipe"] });
	let log = "";
	go.stderr.on("data", (d) => (log += d));
	await until("go healthz", async () => ((await fetch(`${goApi}/healthz`)).ok ? true : undefined));
	return {
		dir, user, goApi, agent, web: `http://127.0.0.1:${webPort}`, api: new GoApi(goApi),
		log: () => log,
		/** POST /events with the user's token; body is sent as is. */
		push: (body: string) => fetch(`http://127.0.0.1:${webPort}/events`, { method: "POST", headers: { Authorization: `Bearer ${user.token}` }, body }),
		members: async (cid: string) => ((await (await fetch(`${goApi}/api/collections/${cid}/members`)).json()) as { members: { event_id: string }[] }).members,
		async close() {
			go.kill("SIGTERM");
			agentServer.close();
			await agent.close(ctx);
			await rm(dir, { recursive: true, force: true });
		},
	};
}

const skip = !existsSync(BIN) && "build ../bin/lifestream first";

test("text 'weight 175' end to end", { skip }, async () => {
	const s = await stack([
		// look-back has its own test
		(c) => script(`await tools.append({ type: "collection.started", parent_id: "${eventId(c)}", payload: { name: "weight", kind: "metric", unit: "lb", include: "body weight readings", lookback_days: 0 } })`),
		fauxAssistantMessage([fauxText("started")]),
		// The script finds the collection with real SQL, files the reading and keeps a record.
		(c) => script(`
			const id = "${eventId(c)}";
			const [[cid, version]] = (await tools.query({ sql: "SELECT id, version FROM collections WHERE name = ?", params: ["weight"] })).rows;
			await tools.append({ type: "label.applied", parent_id: id, payload: { event_id: id, collection_id: cid, definition_version: version, reason: "weight" } });
			await tools.append({ type: "metric.observed", parent_id: id, payload: { collection_id: cid, value: 175 } });
			await tools.append({ type: "record.saved", parent_id: id, payload: { kind: "mood", data: { felt: "good" } } });
		`),
		fauxAssistantMessage([fauxText("filed")]),
	]);
	const sms = (body: string) => s.push(JSON.stringify({ type: "message.received", source: "twilio.sms", payload: { Body: body, From: "+15555550100" } }));
	try {
		assert.equal((await sms("track my weight")).status, 200);
		const weight = await until("collection", async () => (await s.api.collections(s.user.id)).find((c) => c.name === "weight"));
		await (await s.agent.conversationFor(s.user.id, ctx)).waitForIdle(ctx);

		assert.equal((await sms("weight 175")).status, 200);
		const members = await until("member", async () => {
			const members = await s.members(weight.id);
			return members.length === 1 ? members : undefined;
		});
		assert.equal(members.length, 1);
		const records = await s.api.query("SELECT kind, json_extract(data, '$.felt') FROM records WHERE owner = ?", [s.user.id]);
		assert.deepEqual(records.rows, [["mood", "good"]]);

		// The dashboard shows the reading.
		await until("dashboard", async () => {
			const html = await (await fetch(`${s.web}/`)).text();
			return html.includes('<div class="value">175<span class="unit">lb</span>') ? true : undefined;
		});
		const chain = execFileSync(BIN, ["verify", "--data-dir", s.dir], { encoding: "utf8" });
		assert.match(chain, /"ok":true/);
	} catch (err) {
		console.error(s.log());
		throw err;
	} finally {
		await s.close();
	}
});

test("a captured session: its head is judged, the rest join without the model", { skip }, async () => {
	let collectionId = "";
	const seen: string[] = []; // event ids the model was shown
	// One responder for every model call: it starts the collection for the
	// command, labels the session's first message, and otherwise just replies.
	const respond: Responses[number] = (c) => {
		const last = c.messages.at(-1)!;
		if (last.role === "toolResult") return fauxAssistantMessage([fauxText("ok")]);
		const text = JSON.stringify(last.content);
		seen.push(eventId(c));
		if (text.includes("track the migration")) {
			return script(`await tools.append({ type: "collection.started", parent_id: "${eventId(c)}", payload: { name: "pg migration", include: "work on the postgres migration", lookback_days: 0 } })`);
		}
		if (text.includes("upgrade postgres")) {
			const id = eventId(c);
			return script(`await tools.append({ type: "label.applied", parent_id: "${id}", payload: { event_id: "${id}", collection_id: "${collectionId}", definition_version: 1, reason: "migration session" } })`);
		}
		return fauxAssistantMessage([fauxText("nothing to file")]);
	};
	const s = await stack(Array(40).fill(respond));
	try {
		assert.equal((await s.push(JSON.stringify({ type: "message.received", source: "twilio.sms", payload: { Body: "track the migration" } }))).status, 200);
		collectionId = (await until("collection", async () => (await s.api.collections(s.user.id)).find((c) => c.name === "pg migration"))).id;
		await (await s.agent.conversationFor(s.user.id, ctx)).waitForIdle(ctx);

		const texts = ["upgrade postgres to 17; key AKIAIOSFODNN7EXAMPLE", "reading the release notes", "pg_upgrade --check", "fixing the extension", "done"];
		const batch = texts.map((t, i) => JSON.stringify({ type: "llm.message", source: "claude-code",
			payload: { session_id: "sess-1", sequence: i, harness: "claude-code", message: { type: i % 2 ? "assistant" : "user", text: t } } })).join("\n");
		assert.equal((await s.push(batch)).status, 200);
		assert.equal((await s.push(batch)).status, 200, "a resent batch is fine");

		const members = await until("the whole session", async () => {
			const ms = await s.members(collectionId);
			return ms.length === texts.length ? ms : undefined;
		});
		const ids = (await s.api.events(s.user.id, {})).filter((e) => e.type === "llm.message").map((e) => e.id);
		assert.equal(ids.length, texts.length, "no duplicates from the resend");
		assert.deepEqual(members.map((m) => m.event_id).sort(), [...ids].sort());

		await (await s.agent.conversationFor(s.user.id, ctx)).waitForIdle(ctx);
		const shown = seen.filter((id) => ids.includes(id));
		assert.ok(shown.length <= 3, `the model saw ${shown.length} session events; only the head (3) should reach it`);
		assert.ok(!seen.includes(ids[3]) && !seen.includes(ids[4]), "the tail never reaches the model");
	} catch (err) {
		console.error(s.log());
		throw err;
	} finally {
		await s.close();
	}
});
