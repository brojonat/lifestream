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

test("text 'weight 175' end to end", { skip: !existsSync(BIN) && "build ../bin/lifestream first" }, async () => {
	const dir = await mkdtemp(join(tmpdir(), "lifestream-e2e-"));
	const user = JSON.parse(execFileSync(BIN, ["users", "add", "--data-dir", dir, "--name", "me", "--phone", "+15555550100"], { encoding: "utf8" }));
	const [webPort, apiPort, agentPort] = [await freePort(), await freePort(), await freePort()];
	const goApi = `http://127.0.0.1:${apiPort}`;

	let collectionId = "";
	const faux = fauxProvider();
	faux.setResponses([
		(c) => fauxAssistantMessage([fauxToolCall("create_collection", { event_id: eventId(c), name: "weight", kind: "metric", unit: "lb", include: "body weight readings" })], { stopReason: "toolUse" }),
		fauxAssistantMessage([fauxText("started")]),
		(c) => {
			const id = eventId(c);
			return fauxAssistantMessage([
				fauxToolCall("label_event", { event_id: id, collection_id: collectionId, definition_version: 1, reason: "weight" }),
				fauxToolCall("log_value", { event_id: id, collection_id: collectionId, value: 175 }),
			], { stopReason: "toolUse" });
		},
		fauxAssistantMessage([fauxText("filed")]),
	]);
	const models = createModels();
	models.setProvider(faux.provider);
	const agent = await Agent.open({ storagePath: join(dir, "agent.db"), models, model: { provider: "faux", modelId: "faux-1" }, api: new GoApi(goApi) }, ctx);
	const quiet = newLogger({ LOG_LEVEL: "error" });
	const agentServer = createServer(handler(agent, quiet, ctx));
	await new Promise<void>((r) => agentServer.listen(agentPort, "127.0.0.1", r));

	const go = spawn(BIN, ["serve", "--data-dir", dir, "--addr", `127.0.0.1:${webPort}`, "--api-addr", `127.0.0.1:${apiPort}`,
		"--agent-url", `http://127.0.0.1:${agentPort}`], { stdio: ["ignore", "ignore", "pipe"] });
	let goLog = "";
	go.stderr.on("data", (d) => (goLog += d));
	try {
		await until("go healthz", async () => ((await fetch(`${goApi}/healthz`)).ok ? true : undefined));
		const push = (body: string) =>
			fetch(`http://127.0.0.1:${webPort}/events`, {
				method: "POST",
				headers: { Authorization: `Bearer ${user.token}` },
				body: JSON.stringify({ type: "message.received", source: "twilio.sms", payload: { Body: body, From: "+15555550100" } }),
			});

		assert.equal((await push("track my weight")).status, 200);
		const api = new GoApi(goApi);
		const weight = await until("collection", async () => (await api.collections(user.id)).find((c) => c.name === "weight"));
		collectionId = weight.id;
		await (await agent.conversationFor(user.id, ctx)).waitForIdle(ctx);

		assert.equal((await push("weight 175")).status, 200);
		const members = await until("member", async () => {
			const res = await fetch(`${goApi}/api/collections/${weight.id}/members`);
			const { members } = (await res.json()) as { members: unknown[] };
			return members.length === 1 ? members : undefined;
		});
		assert.equal(members.length, 1);

		// The dashboard shows the reading.
		await until("dashboard", async () => {
			const html = await (await fetch(`http://127.0.0.1:${webPort}/`)).text();
			return html.includes('<div class="value">175<span class="unit">lb</span>') ? true : undefined;
		});
		const chain = execFileSync(BIN, ["verify", "--data-dir", dir], { encoding: "utf8" });
		assert.match(chain, /"ok":true/);
	} catch (err) {
		console.error(goLog);
		throw err;
	} finally {
		go.kill("SIGTERM");
		agentServer.close();
		await agent.close(ctx);
		await rm(dir, { recursive: true, force: true });
	}
});
