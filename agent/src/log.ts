// Structured logs on stderr: JSON when LOG_FORMAT=json, else key=value.
// Level from LOG_LEVEL (debug, info, warn, error).

const LEVELS = { debug: 10, info: 20, warn: 30, error: 40 } as const;
type Level = keyof typeof LEVELS;

export type Logger = Record<Level, (msg: string, attrs?: Record<string, unknown>) => void>;

export function newLogger(env: Record<string, string | undefined> = process.env, out: { write(s: string): unknown } = process.stderr): Logger {
	const min = LEVELS[(env.LOG_LEVEL?.toLowerCase() as Level) ?? "info"] ?? LEVELS.info;
	const json = env.LOG_FORMAT?.toLowerCase() === "json";
	const emit = (level: Level) => (msg: string, attrs: Record<string, unknown> = {}) => {
		if (LEVELS[level] < min) return;
		const time = new Date().toISOString();
		if (json) {
			out.write(JSON.stringify({ time, level: level.toUpperCase(), msg, ...attrs }) + "\n");
		} else {
			const kv = Object.entries(attrs).map(([k, v]) => `${k}=${typeof v === "string" ? JSON.stringify(v) : JSON.stringify(v)}`);
			out.write([`time=${time}`, `level=${level.toUpperCase()}`, `msg=${JSON.stringify(msg)}`, ...kv].join(" ") + "\n");
		}
	};
	return { debug: emit("debug"), info: emit("info"), warn: emit("warn"), error: emit("error") };
}
