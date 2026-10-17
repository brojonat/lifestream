// The pi capture extension (capture/pi/lifestream.ts) against a fake pi and
// a stub `lifestream` on PATH.

import assert from "node:assert/strict";
import { chmod, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";
import extension, { capture } from "../../capture/pi/lifestream.ts";

const ctx = (file: string | undefined, notes: string[] = []) => ({
	sessionManager: { getSessionFile: () => file, getSessionId: () => "019e-fixture", getCwd: () => "/home/me/src/pgmig" },
	ui: { notify: (m: string) => void notes.push(m) },
});

test("pi capture: hands the session file to lifestream capture pi, only when capture is on", async () => {
	const dir = await mkdtemp(join(tmpdir(), "lifestream-pi-"));
	const out = join(dir, "calls");
	const stub = join(dir, "lifestream");
	await writeFile(stub, `#!/bin/sh\necho "$@" >> ${out}\ncat >> ${out}\necho >> ${out}\n`);
	await chmod(stub, 0o755);
	const path = process.env.PATH;
	process.env.PATH = `${dir}:${path}`;
	try {
		const handlers = new Map<string, (e: unknown, c: ReturnType<typeof ctx>) => Promise<void>>();
		const pi = { on: (name: string, h: (e: unknown, c: ReturnType<typeof ctx>) => Promise<void>) => void handlers.set(name, h) };

		delete process.env.LIFESTREAM_CAPTURE;
		extension(pi);
		assert.equal(handlers.size, 0, "capture off: no handlers");

		process.env.LIFESTREAM_CAPTURE = "on";
		extension(pi);
		assert.deepEqual([...handlers.keys()].sort(), ["session_shutdown", "turn_end"]);
		await handlers.get("turn_end")!({}, ctx("/s/session.jsonl"));
		await handlers.get("turn_end")!({}, ctx(undefined)); // in-memory session: nothing to run
		const [args, hook] = (await readFile(out, "utf8")).trim().split("\n");
		assert.equal(args, "capture pi");
		assert.deepEqual(JSON.parse(hook), { session_id: "019e-fixture", transcript_path: "/s/session.jsonl", cwd: "/home/me/src/pgmig" });

		const notes: string[] = [];
		await capture(ctx("/s/session.jsonl", notes), join(dir, "missing"));
		assert.match(notes[0], /ENOENT/, "a missing binary is reported, not thrown");
	} finally {
		process.env.PATH = path;
		delete process.env.LIFESTREAM_CAPTURE;
		await rm(dir, { recursive: true, force: true });
	}
});
