import { ApiError } from "./api";
import { kubernetesFailureCode } from "./kubernetesContinuity";
import { datedClock } from "./kubernetesClock";

/**
 * The server is rebuilding its connection to the cluster. It answers 503 with
 * a short Retry-After and recovers on its own: nothing is broken that an
 * operator has to fix, so the page keeps what it shows and says it is
 * reconnecting.
 */
export function kubernetesReconnecting(error: unknown): boolean {
  return error instanceof ApiError && kubernetesFailureCode(error) === "connector_reconnecting";
}

/**
 * The database Versus keeps cluster history in did not answer. Live cluster
 * reads are unaffected, so only saved history is missing for now.
 */
export function kubernetesHistoryStoreDown(error: unknown): boolean {
  return error instanceof ApiError && (kubernetesFailureCode(error) ?? "").startsWith("history_store_");
}

/** The server gave up on the request before it finished; asking again is expected to work. */
export function kubernetesServerTimeout(error: unknown): boolean {
  return error instanceof ApiError && kubernetesFailureCode(error) === "server_timeout";
}

export const reconnectingMessage = "Reconnecting to the cluster…";
export const historyStoreDownMessage = "Saved history is temporarily unavailable";

/**
 * The one line for a refresh that failed while earlier data stays on screen.
 * It names what failed when the server said so, and always dates the data
 * shown. `retryInMs` is when the view asks again by itself; a Retry-After the
 * server sent outranks it.
 */
export function refreshFailureLabel(error: unknown, dataAt: number | string | undefined, retryInMs: number, now: number = Date.now()): string {
  const time = datedClock(dataAt, { hour: "2-digit", minute: "2-digit" }, now);
  const shown = time ? `showing data as of ${time}` : "showing earlier data";
  if (kubernetesHistoryStoreDown(error)) return `The history database is not responding — ${shown}`;
  if (kubernetesServerTimeout(error)) {
    const wait = error instanceof ApiError && error.retryAfterMs !== undefined ? error.retryAfterMs : retryInMs;
    return `The server timed out — try again in ${Math.max(1, Math.ceil(wait / 1000))}s; ${shown}`;
  }
  return `Couldn't refresh — ${shown}`;
}

/**
 * The sync status read itself did not answer, for a reason that says nothing
 * about the cluster sync: the server gave up on the read, or the history
 * database did not respond. The last known sync state stays on screen and
 * this neutral line says why it is not newer; `retries` says the line counts
 * down to the next read. Any other failure is not passing and gets `undefined`.
 */
export function syncStatusRefreshNote(error: unknown): { text: string; retries: boolean } | undefined {
  if (kubernetesHistoryStoreDown(error)) return { text: "The history database is not responding", retries: false };
  if (kubernetesServerTimeout(error)) return { text: "Couldn't refresh status", retries: true };
  return undefined;
}

/** The countdown to the next read, from the reader's clock. */
export function retryCountdown(retryAt: number, now: number): string {
  const seconds = Math.ceil((retryAt - now) / 1000);
  return seconds > 0 ? `try again in ${seconds}s` : "retrying…";
}

/**
 * The usage card's line when its refresh failed: the cause by code, then what
 * the card still shows.
 */
export function usageRefreshNote(error: unknown, shown: { kept: boolean; fromOverview: boolean }): string {
  const cause = kubernetesHistoryStoreDown(error) ? "The history database is not responding"
    : kubernetesServerTimeout(error) ? "Couldn't refresh — try again"
      : "Usage snapshot unavailable";
  return shown.kept ? `${cause}; showing the last usage figures` : shown.fromOverview ? `${cause}; using overview snapshot` : cause;
}

/**
 * The short, calm line for a list whose refresh failed while its last rows
 * stay on screen.
 */
export function listRefreshNote(error: unknown): string {
  if (kubernetesHistoryStoreDown(error)) return "The history database is not responding";
  if (kubernetesServerTimeout(error)) return "Couldn't refresh — try again";
  return "Couldn't refresh — showing earlier data";
}
