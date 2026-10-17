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

/** Go (real binary, its own data dir) plus this agent with a faux model, wired as in the pod. */
async function stack(responses: Responses, reviewer: (chunk: string) => Promise<{ text: string; kind: string }[]> = async () => []) {
	const dir = await mkdtemp(join(tmpdir(), "lifestream-e2e-"));
	const user = JSON.parse(execFileSync(BIN, ["users", "add", "--data-dir", dir, "--name", "me", "--phone", "+15555550100"], { encoding: "utf8" }));
	const [webPort, apiPort, agentPort] = [await freePort(), await freePort(), await freePort()];
	const goApi = `http://127.0.0.1:${apiPort}`;
	const faux = fauxProvider();
	faux.setResponses(responses);
	const models = createModels();
	models.setProvider(faux.provider);
	const agent = await Agent.open({ storagePath: join(dir, "agent.db"), models, model: { provider: "faux", modelId: "faux-1" }, api: new GoApi(goApi),
		redaction: { reviewer } }, ctx);
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
		children: async (id: string) => ((await (await fetch(`${goApi}/api/events/${id}/children`)).json()) as { events: { type: string; payload: any }[] }).events,
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
	let collectionId = "";
	const s = await stack([
		(c) => fauxAssistantMessage([fauxToolCall("create_collection", { event_id: eventId(c), name: "weight", kind: "metric", unit: "lb", include: "body weight readings", lookback_days: 0 })], { stopReason: "toolUse" }), // look-back has its own test
		fauxAssistantMessage([fauxText("started")]),
		(c) => {
			const id = eventId(c);
			return fauxAssistantMessage([
				fauxToolCall("label_event", { event_id: id, collection_id: collectionId, definition_version: 1, reason: "weight" }),
				fauxToolCall("log_value", { event_id: id, collection_id: collectionId, value: 175 }),
			], { stopReason: "toolUse" });
		},
		fauxAssistantMessage([fauxText("filed")]),
	], async () => [{ text: "+15555550100", kind: "phone-again" }]);
	const sms = (body: string) => s.push(JSON.stringify({ type: "message.received", source: "twilio.sms", payload: { Body: body, From: "+15555550100" } }));
	try {
		assert.equal((await sms("track my weight")).status, 200);
		const weight = await until("collection", async () => (await s.api.collections(s.user.id)).find((c) => c.name === "weight"));
		collectionId = weight.id;
		await (await s.agent.conversationFor(s.user.id, ctx)).waitForIdle(ctx);

		assert.equal((await sms("weight 175")).status, 200);
		const members = await until("member", async () => {
			const members = await s.members(weight.id);
			return members.length === 1 ? members : undefined;
		});
		assert.equal(members.length, 1);

		// The labeled SMS got a redacted copy in Go: the sender's number masked.
		const msgId = ((await s.api.events(s.user.id, {})).find((e) => (e.payload as { Body?: string })?.Body === "weight 175"))!.id;
		const copy = await until("redaction", async () => (await s.children(msgId)).find((k) => k.type === "redaction.created"));
		assert.equal(copy.payload.payload.From, "[REDACTED:phone]");
		assert.equal(copy.payload.payload.Body, "weight 175");

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

test("a captured session: its head is judged, the rest join without the model and are redacted", { skip }, async () => {
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
			return fauxAssistantMessage([fauxToolCall("create_collection", { event_id: eventId(c), name: "pg migration", include: "work on the postgres migration", lookback_days: 0 })], { stopReason: "toolUse" });
		}
		if (text.includes("upgrade postgres")) {
			return fauxAssistantMessage([fauxToolCall("label_event", { event_id: eventId(c), collection_id: collectionId, definition_version: 1, reason: "migration session" })], { stopReason: "toolUse" });
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

		// The sweep (a timer in main.ts) gives every member its copy.
		const copies = await until("redacted copies", async () => {
			await s.agent.sweep(ctx);
			const cs = await Promise.all(ids.map(async (id) => (await s.children(id)).find((k) => k.type === "redaction.created")));
			return cs.every(Boolean) ? cs : undefined;
		});
		assert.equal(copies[0]!.payload.payload.message.text, "upgrade postgres to 17; key [REDACTED:aws_access_key]");
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
