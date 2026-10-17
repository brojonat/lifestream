// Lifestream capture for pi. After every turn (and when a session shuts
// down) it runs `lifestream capture pi` with {session_id, transcript_path,
// cwd} on stdin; that command sends the session file's new lines. The
// session file is the offline buffer, so a failed send is retried on the
// next turn without keeping anything in memory.
//
// Install: copy to ~/.pi/agent/extensions/lifestream.ts and put
// `lifestream` on PATH. Nothing is captured unless LIFESTREAM_CAPTURE=on.
//
// The pi types are declared here, not imported, so this file works with
// either npm scope (@mariozechner/* or @earendil-works/*).

import { spawn } from "node:child_process";

type Ctx = {
	sessionManager: { getSessionFile(): string | undefined; getSessionId(): string; getCwd(): string };
	ui?: { notify(message: string, level: "info" | "warning" | "error"): void };
};
type Pi = { on(event: "turn_end" | "session_shutdown", handler: (event: unknown, ctx: Ctx) => Promise<void>): void };

/** Runs one capture. Resolves either way: capture must never break a turn. */
export function capture(ctx: Ctx, bin = "lifestream"): Promise<void> {
	const file = ctx.sessionManager.getSessionFile();
	if (!file) return Promise.resolve(); // an in-memory session has nothing to tail
	const hook = JSON.stringify({ session_id: ctx.sessionManager.getSessionId(), transcript_path: file, cwd: ctx.sessionManager.getCwd() });
	return new Promise((resolve) => {
		const child = spawn(bin, ["capture", "pi"], { stdio: ["pipe", "ignore", "pipe"] });
		let stderr = "";
		child.stderr.on("data", (d) => (stderr += d));
		child.on("error", (err) => {
			ctx.ui?.notify(`lifestream capture: ${err.message}`, "error");
			resolve();
		});
		// Exit 0 with a warning means "offline, will retry": stay quiet.
		child.on("close", (code) => {
			if (code !== 0) ctx.ui?.notify(stderr.trim() || `lifestream capture exited ${code}`, "error");
			resolve();
		});
		child.stdin.end(hook);
	});
}

export default function (pi: Pi) {
	if (process.env.LIFESTREAM_CAPTURE !== "on") return;
	// One capture at a time; a turn that ends mid-send queues behind it.
	let last = Promise.resolve();
	const queue = (_event: unknown, ctx: Ctx) => (last = last.then(() => capture(ctx)));
	pi.on("turn_end", queue);
	pi.on("session_shutdown", queue);
}
