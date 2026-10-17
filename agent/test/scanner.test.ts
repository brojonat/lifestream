import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { test } from "node:test";
import { redactStrings, scanJSON, scanText } from "../src/scanner.ts";

const corpus = JSON.parse(readFileSync(join(import.meta.dirname, "..", "..", "testdata", "secrets.json"), "utf8")) as {
	positives: { kind: string; secret: string; text: string }[];
	negatives: { name: string; text: string }[];
};

test("every fake secret in the corpus is redacted", () => {
	for (const p of corpus.positives) {
		const r = scanText(p.text);
		assert.ok(!r.text.includes(p.secret), `${p.kind}: secret survived in ${JSON.stringify(r.text)}`);
		assert.ok(r.findings.some((f) => f.kind === p.kind), `${p.kind}: reported ${JSON.stringify(r.findings)}`);
	}
});

test("ordinary text comes through unchanged", () => {
	for (const n of corpus.negatives) {
		const r = scanText(n.text);
		assert.equal(r.text, n.text, `${n.name} was changed`);
		assert.deepEqual(r.findings, []);
	}
});

test("context around a secret is kept", () => {
	assert.equal(scanText("DATABASE_URL=postgres://app:hunter2hunter2@db:5432/x").text, "DATABASE_URL=postgres://app:[REDACTED:connection_password]@db:5432/x");
	assert.equal(scanText("Authorization: Bearer abcdefghijklmnopqrstuvwxyz012345").text, "Authorization: Bearer [REDACTED:bearer]");
	assert.equal(scanText(`git clone https://ghp_${"a".repeat(36)}@github.com/me/repo`).text, "git clone https://[REDACTED:github_token]@github.com/me/repo");
	assert.equal(scanText(`?key=AIzaSy${"b".repeat(33)}&q=1`).text, "?key=[REDACTED:google_api_key]&q=1");
});

test("JSON payloads: strings redacted, structure and keys kept", () => {
	const r = scanJSON({ From: "+15555550100", Body: "key AKIAIOSFODNN7EXAMPLE", n: 3, tags: ["alice@example.com"] });
	assert.deepEqual(r.value, { From: "[REDACTED:phone]", Body: "key [REDACTED:aws_access_key]", n: 3, tags: ["[REDACTED:email]"] });
	assert.deepEqual(r.findings.map((f) => f.kind).sort(), ["aws_access_key", "email", "phone"]);
});

test("model-flagged spans are replaced everywhere, longest first", () => {
	const v = redactStrings({ a: "Acme Corp and Acme", b: ["db.acme.internal"] }, [
		{ text: "Acme", kind: "client" },
		{ text: "Acme Corp", kind: "client" },
		{ text: "db.acme.internal", kind: "hostname" },
		{ text: "x", kind: "too short to trust" },
	]);
	assert.deepEqual(v, { a: "[REDACTED:client] and [REDACTED:client]", b: ["[REDACTED:hostname]"] });
});
