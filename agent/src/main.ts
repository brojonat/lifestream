// Entry point. Configuration is environment variables only:
//   AGENT_ADDR            listen address (default 127.0.0.1:8082)
//   LIFESTREAM_API_URL    Go's internal API (default http://127.0.0.1:8081)
//   DATA_DIR              where agent.db lives (default ~/.lifestream)
//   AGENT_MODEL           provider/modelId (default anthropic/claude-sonnet-5-5)
//   ANTHROPIC_API_KEY     read by the Anthropic provider
//   LOG_LEVEL, LOG_FORMAT

import { createServer } from "node:http";
import { homedir } from "node:os";
import { join } from "node:path";
import { BACKGROUND_CONTEXT } from "@earendil-works/chord/context";
import { createModels } from "@earendil-works/pi-ai/models";
import { anthropicProvider } from "@earendil-works/pi-ai/providers/anthropic";
import { Agent } from "./agent.ts";
import { GoApi } from "./goapi.ts";
import { newLogger } from "./log.ts";
import { handler } from "./server.ts";

const env = process.env;
const log = newLogger();
const ctx = BACKGROUND_CONTEXT;

const [provider, ...rest] = (env.AGENT_MODEL ?? "anthropic/claude-sonnet-5-5").split("/");
const model = { provider, modelId: rest.join("/") };
const models = createModels();
models.setProvider(anthropicProvider());

const dataDir = env.DATA_DIR ?? join(homedir(), ".lifestream");
const agent = await Agent.open(
	{
		storagePath: join(dataDir, "agent.db"),
		models,
		model,
		api: new GoApi(env.LIFESTREAM_API_URL ?? "http://127.0.0.1:8081"),
		onReport: (err) => log.warn("harness report", { error: String(err) }),
	},
	ctx,
);

const [host, port] = (env.AGENT_ADDR ?? "127.0.0.1:8082").split(":");
const server = createServer(handler(agent, log, ctx));
server.listen(Number(port), host, () => log.info("listening", { addr: `${host}:${port}`, model: `${model.provider}/${model.modelId}`, data_dir: dataDir }));

// A failed harness can't recover in place; exit so the pod restarts us.
agent.closed.then((end) => {
	if (end.reason === "failed") {
		log.error("harness failed; exiting", { error: String((end as { error?: unknown }).error) });
		process.exit(1);
	}
});

const shutdown = async (signal: string) => {
	log.info("shutting down", { signal });
	server.close();
	await agent.close(ctx);
	process.exit(0);
};
process.on("SIGTERM", () => void shutdown("SIGTERM"));
process.on("SIGINT", () => void shutdown("SIGINT"));
