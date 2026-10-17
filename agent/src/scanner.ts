// Deterministic secret scanner: the first redaction layer, before the
// model's review. Each match becomes [REDACTED:<kind>]. Patterns favor
// catching too much over too little; the test corpus in
// testdata/secrets.json (shared with the capture adapters) pins both sides.

export type Finding = { kind: string; count: number };

type Pattern = {
	kind: string;
	re: RegExp; // global
	/** Index of the capture group holding the secret; 0 masks the whole match. */
	group?: number;
};

const PATTERNS: Pattern[] = [
	// Multi-line and prefixed tokens first, so later generic rules see the mask.
	{ kind: "private_key", re: /-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----/g },
	{ kind: "aws_access_key", re: /\b(?:AKIA|ASIA)[0-9A-Z]{16}\b/g },
	{ kind: "github_token", re: /\b(?:gh[pousr]_[A-Za-z0-9]{36,}|github_pat_[A-Za-z0-9_]{60,})/g },
	{ kind: "anthropic_key", re: /\bsk-ant-[A-Za-z0-9_-]{20,}/g },
	{ kind: "openai_key", re: /\bsk-(?:proj-|svcacct-|admin-)?[A-Za-z0-9_-]{20,}/g },
	{ kind: "stripe_key", re: /\b(?:sk|rk|pk)_(?:live|test)_[A-Za-z0-9]{16,}/g },
	{ kind: "slack_token", re: /\bxox[abprs]-[A-Za-z0-9-]{10,}/g },
	{ kind: "google_api_key", re: /\bAIza[0-9A-Za-z_-]{35}/g },
	{ kind: "twilio_sid", re: /\b(?:AC|SK)[0-9a-f]{32}\b/g },
	{ kind: "jwt", re: /\beyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{10,}/g },
	{ kind: "bearer", re: /\b[Bb]earer\s+([A-Za-z0-9\-._~+/]{20,}=*)/g, group: 1 },
	{ kind: "connection_password", re: /\b[a-z][a-z0-9+.-]*:\/\/[^\s:/@]+:([^\s@/]+)@/g, group: 1 },
	{
		kind: "assigned_secret",
		re: /\b(?:[A-Za-z0-9]+[_-])?(?:api[_-]?key|secret|token|passw(?:or)?d|pwd)["']?\s*[:=]\s*["']?([^\s"',;]{8,})/gi,
		group: 1,
	},
	{ kind: "email", re: /\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}\b/g },
	{ kind: "phone", re: /(?<![\w+])\+[1-9]\d{7,14}\b/g },
];

const mask = (kind: string) => `[REDACTED:${kind}]`;

/** Redacts one string. */
export function scanText(text: string): { text: string; findings: Finding[] } {
	const counts = new Map<string, number>();
	let out = text;
	for (const p of PATTERNS) {
		out = out.replace(p.re, (match, ...groups) => {
			const secret = p.group ? (groups[p.group - 1] as string | undefined) : match;
			// Never match across an earlier mask (a token inside a URL, say).
			if (!secret || match.includes("[REDACTED:")) return match;
			counts.set(p.kind, (counts.get(p.kind) ?? 0) + 1);
			return p.group ? match.replace(secret, mask(p.kind)) : mask(p.kind);
		});
	}
	return { text: out, findings: [...counts].map(([kind, count]) => ({ kind, count })) };
}

/** Redacts every string in a JSON value (keys stay), returning a copy. */
export function scanJSON(value: unknown): { value: unknown; findings: Finding[] } {
	const all = new Map<string, number>();
	const walk = (v: unknown): unknown => {
		if (typeof v === "string") {
			const r = scanText(v);
			for (const f of r.findings) all.set(f.kind, (all.get(f.kind) ?? 0) + f.count);
			return r.text;
		}
		if (Array.isArray(v)) return v.map(walk);
		if (v && typeof v === "object") return Object.fromEntries(Object.entries(v).map(([k, x]) => [k, walk(x)]));
		return v;
	};
	const out = walk(value);
	return { value: out, findings: [...all].map(([kind, count]) => ({ kind, count })) };
}

/** Replaces each exact string the model flagged, everywhere in a JSON value. */
export function redactStrings(value: unknown, spans: { text: string; kind: string }[]): unknown {
	const usable = spans.filter((s) => s.text.length >= 3).sort((a, b) => b.text.length - a.text.length);
	const walk = (v: unknown): unknown => {
		if (typeof v === "string") return usable.reduce((acc, s) => acc.split(s.text).join(mask(s.kind)), v);
		if (Array.isArray(v)) return v.map(walk);
		if (v && typeof v === "object") return Object.fromEntries(Object.entries(v).map(([k, x]) => [k, walk(x)]));
		return v;
	};
	return walk(value);
}
