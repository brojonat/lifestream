// The `rules` extension: the owner's standing instructions, stored in Go so
// background tasks and future conversations follow them too. A section
// lists them before every request; add_rule writes a new one.

import { Type } from "@earendil-works/pi-ai";
import { defineExtension, defineTool, section } from "@earendil-works/pi-durable";
import { fail, ownerOf, text } from "./collections.ts";
import { ApiError, type GoApi } from "./goapi.ts";

export function rulesExtension(api: GoApi) {
	const addRule = defineTool({
		name: "add_rule",
		description: "Write down something the owner taught you, as one short standing instruction.",
		parameters: Type.Object({
			event_id: Type.String({ description: "the event where they taught it" }),
			text: Type.String(),
		}),
		replay: "safe",
		execute: async (a, tapi, ctx) => {
			try {
				const owner = await ownerOf(tapi, tapi.conversationId, ctx);
				const e = await api.append(owner, "rule.added", { text: a.text }, { parentId: a.event_id, idempotencyKey: `tool:${String(tapi.taskId)}` });
				return text(`ok: rule ${e.id}`);
			} catch (err) {
				if (err instanceof ApiError && err.status < 500) return fail(`rejected: ${err.message}`);
				throw err;
			}
		},
	});

	return defineExtension({
		name: "rules",
		tools: [addRule],
		sections: [
			section("owner_rules", async (input, ctx) => {
				const rules = await api.rules(await ownerOf(input.read, input.conversationId, ctx));
				return rules.length ? rules.map((r) => `- ${r.text}`).join("\n") : "None yet.";
			}),
		],
	});
}
