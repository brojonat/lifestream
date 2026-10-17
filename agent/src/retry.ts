// Pi Durable faults a task for good when a phase throws. These helpers turn
// a throw into a checkpointed wait and a retry with backoff; the deadline
// lives in the checkpoint, so a restart keeps waiting for the same moment.

import type { Context } from "@earendil-works/chord/context";
import type { PhaseHandler } from "@earendil-works/pi-durable";

export const MAX_ATTEMPTS = 6;
export const backoffMs = (attempt: number, baseMs = 5000) => Math.min(baseMs * 2 ** attempt, 5 * 60_000);

/** Checkpoint of the retry phase: when to wake, and the checkpoint to go back to. */
export type Retry<S> = { phase: "retry"; until: number; resume: S };

/**
 * Wraps a phase: on a throw, park in "retry" with attempt+1, or give up after
 * MAX_ATTEMPTS. P is the phase's checkpoint; S, the task's whole state union,
 * must include Retry<P>.
 */
export function retrying<I, P extends { phase: string }, S extends { phase: string }, R>(
	baseMs: number | undefined,
	fn: PhaseHandler<I, P, S, R, object>,
): PhaseHandler<I, P, S, R, object> {
	return async (task, runtime, ctx: Context) => {
		const st = task.state.checkpoint as P & { attempt?: number };
		try {
			await fn(task, runtime, ctx);
		} catch (err) {
			const attempt = (st.attempt ?? 0) + 1;
			if (attempt >= MAX_ATTEMPTS) throw err; // the task ends faulted; onReport logs it
			runtime.report(err);
			const retry: Retry<P> = { phase: "retry", until: runtime.now() + backoffMs(attempt - 1, baseMs), resume: { ...st, attempt } };
			await runtime.commit(() => ({ status: "running", checkpoint: retry as unknown as S }), ctx);
		}
	};
}

/** The "retry" phase: sleep until the deadline, then resume. */
export function retryPhase<I, P extends { phase: string }, S extends { phase: string }, R>(): PhaseHandler<I, Retry<P>, S, R, object> {
	return async (task, runtime, ctx: Context) => {
		const st = task.state.checkpoint as Retry<P>;
		await runtime.sleep(st.until, ctx);
		await runtime.commit(() => ({ status: "running", checkpoint: st.resume as unknown as S }), ctx);
	};
}
