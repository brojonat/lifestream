// POST /submit {owner, event} from Go's dispatcher, and GET /healthz. 202
// means the event is durably admitted; Go marks it delivered then.

import type { IncomingMessage, ServerResponse } from "node:http";
import type { Context } from "@earendil-works/chord/context";
import type { Agent } from "./agent.ts";
import type { StoredEvent } from "./goapi.ts";
import type { Logger } from "./log.ts";

const MAX_BODY = 64 << 20;

export function handler(agent: Agent, log: Logger, ctx: Context) {
	return async (req: IncomingMessage, res: ServerResponse) => {
		const send = (status: number, body: unknown) => {
			res.writeHead(status, { "Content-Type": "application/json" });
			res.end(JSON.stringify(body));
		};
		try {
			if (req.method === "GET" && req.url === "/healthz") {
				res.writeHead(200, { "Content-Type": "text/plain" });
				res.end("ok\n");
				return;
			}
			if (req.method !== "POST" || req.url !== "/submit") return send(404, { error: "not found" });

			let size = 0;
			const chunks: Buffer[] = [];
			for await (const chunk of req) {
				size += chunk.length;
				if (size > MAX_BODY) return send(413, { error: "body too large" });
				chunks.push(chunk);
			}
			let body: { owner?: unknown; event?: StoredEvent };
			try {
				body = JSON.parse(Buffer.concat(chunks).toString("utf8"));
			} catch {
				return send(400, { error: "body is not JSON" });
			}
			const { owner, event } = body;
			if (typeof owner !== "string" || !owner || typeof event?.id !== "string" || typeof event.type !== "string") {
				return send(400, { error: "want {owner, event: {id, type, ...}}" });
			}
			const sub = await agent.submit(owner, event, ctx);
			log.info("submitted", { event: event.id, type: event.type, owner, submission: String(sub.id) });
			send(202, { submission: String(sub.id) });
		} catch (err) {
			log.error("submit failed", { error: String(err) });
			send(500, { error: "internal error" });
		}
	};
}
