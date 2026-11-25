import type { QueryState } from "@tanstack/react-query";
import { ApiError } from "./api";
import { kubernetesInventoryNotReady } from "./kubernetesContinuity";

/** First wait after a read fails with a retryable answer. */
export const failureBackoffBaseMs = 2_000;
/** Longest wait between automatic retries while a read keeps failing. */
export const failureBackoffCapMs = 60_000;
/** A server's Retry-After is honoured, but never parks a read for longer than this. */
export const retryAfterCeilingMs = 10 * 60_000;

/**
 * Whether a failed read may be asked again on its own. The server says so in
 * `retryable`; without that, a network failure, a timeout, rate limiting and a
 * gateway or availability fault are worth asking again, while a refusal or a
 * bad request is answered the same way every time and waits for the reader.
 */
export function kubernetesFailureRetryable(error: unknown): boolean {
  if (!(error instanceof ApiError)) return true;
  const flag = retryableFlag(error);
  if (flag !== undefined) return flag;
  return error.status === 408 || error.status === 429 || error.status >= 500;
}

function retryableFlag(error: ApiError): boolean | undefined {
  const body = error.body;
  const flag = body && typeof body === "object" ? (body as { retryable?: unknown }).retryable : undefined;
  return typeof flag === "boolean" ? flag : undefined;
}

function serverSaysNoRetry(error: unknown): boolean {
  return error instanceof ApiError && retryableFlag(error) === false;
}

/**
 * The wait before the next automatic retry after `failures` consecutive
 * failures: 2 s, 4 s, 8 s … up to 60 s. Each wait is drawn from the last
 * quarter below its step, so open tabs do not retry in lockstep, and never
 * shorter than the server's Retry-After.
 */
export function kubernetesFailureBackoffDelay(failures: number, retryAfterMs?: number, random: () => number = Math.random): number {
  const step = Math.min(failureBackoffCapMs, failureBackoffBaseMs * 2 ** Math.max(0, failures - 1));
  const jittered = Math.round(step * (0.75 + 0.25 * random()));
  const hint = retryAfterMs !== undefined && Number.isFinite(retryAfterMs) && retryAfterMs > 0 ? Math.min(retryAfterMs, retryAfterCeilingMs) : 0;
  return Math.max(jittered, hint);
}

/** The part of a cached query the backoff reads; any query's state fits. */
interface FailingQuery {
  readonly state: QueryState<unknown, unknown>;
}

interface FailureStreak {
  /** When the failure this streak last counted was recorded. */
  errorAt: number;
  failures: number;
  delay: number;
}

// Keyed by the cached query, so every observer of one read shares one streak
// and a read that is removed from the cache takes its streak with it.
const streaks = new WeakMap<object, FailureStreak>();

function currentStreak(query: FailingQuery): FailureStreak | undefined {
  const streak = streaks.get(query);
  if (streak && query.state.dataUpdatedAt > streak.errorAt) {
    // A success since the last counted failure ends the streak.
    streaks.delete(query);
    return undefined;
  }
  return streak;
}

/**
 * The refetch interval of a Kubernetes read, given the cadence its caller
 * wants while it answers (`normal`).
 *
 * A read that fails with a retryable answer waits out an exponential backoff
 * before it asks again, instead of its normal cadence; a read the server says
 * not to retry is not asked again until the reader retries or the data
 * changes. A read that never polls keeps not polling. The warming answer of a
 * first sync is progress, not a failure, so it keeps the caller's cadence.
 */
export function kubernetesFailureRefetchInterval(query: FailingQuery, normal: number | false | undefined): number | false | undefined {
  const { status, error, errorUpdatedAt } = query.state;
  const streak = currentStreak(query);
  if (status === "success") return normal;
  if (status === "error") {
    if (kubernetesInventoryNotReady(error)) return normal;
    let next = streak;
    if (!next || next.errorAt !== errorUpdatedAt) {
      const failures = (streak?.failures ?? 0) + 1;
      next = { errorAt: errorUpdatedAt, failures, delay: kubernetesFailureBackoffDelay(failures, error instanceof ApiError ? error.retryAfterMs : undefined) };
      streaks.set(query, next);
    }
    if (!normal || !kubernetesFailureRetryable(error)) return false;
    // A backoff slows a read down; it never asks faster than its cadence.
    return Math.max(normal, next.delay);
  }
  // A read asked again after a failure is pending until it answers; it keeps
  // the step it is on rather than falling back to the normal cadence.
  return streak && normal ? Math.max(normal, streak.delay) : normal;
}

/**
 * Whether a failed read must not be asked again by a background refresh (a
 * commit notice, the fallback timer): a retryable failure until its backoff
 * ends, one the server explicitly says not to retry until the reader asks.
 * The reader's own Retry is never held back.
 */
export function kubernetesFailureBackingOff(query: FailingQuery, now = Date.now()): boolean {
  const { status, error } = query.state;
  if (status !== "error" || kubernetesInventoryNotReady(error)) return false;
  // Only the server's explicit "do not retry" holds a read for the reader. A
  // refusal (lost access, a missing object) is still asked again on a commit
  // notice, which is how access that came back is noticed.
  if (!kubernetesFailureRetryable(error)) return serverSaysNoRetry(error);
  const streak = currentStreak(query);
  return Boolean(streak && now < streak.errorAt + streak.delay);
}
