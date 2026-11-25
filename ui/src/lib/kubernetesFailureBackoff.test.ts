import { describe, expect, it } from "vitest";
import type { Query } from "@tanstack/react-query";
import { ApiError, retryAfterMilliseconds } from "./api";
import {
  failureBackoffCapMs,
  kubernetesFailureBackingOff,
  kubernetesFailureBackoffDelay,
  kubernetesFailureRefetchInterval,
  kubernetesFailureRetryable,
} from "./kubernetesFailureBackoff";

type State = { status: "pending" | "error" | "success"; error: unknown; errorUpdatedAt: number; dataUpdatedAt: number };

// Only the state the interval reads; the cache owns everything else.
function fakeQuery(state: State) {
  return { state } as unknown as Query & { state: State };
}

const unavailable = (retryable?: boolean, retryAfterMs?: number) => {
  const error = new ApiError(503, "unavailable", retryable === undefined ? {} : { code: "connector_credential_unresolved", retryable });
  if (retryAfterMs !== undefined) error.retryAfterMs = retryAfterMs;
  return error;
};

describe("kubernetesFailureBackoffDelay", () => {
  it("doubles from two seconds up to the one-minute cap, with jitter below each step", () => {
    const top = () => 1;
    expect([1, 2, 3, 4, 5, 6, 7, 12].map((failures) => kubernetesFailureBackoffDelay(failures, undefined, top)))
      .toEqual([2_000, 4_000, 8_000, 16_000, 32_000, 60_000, 60_000, 60_000]);
    const bottom = () => 0;
    expect(kubernetesFailureBackoffDelay(1, undefined, bottom)).toBe(1_500);
    expect(kubernetesFailureBackoffDelay(20, undefined, bottom)).toBe(45_000);
  });

  it("never waits less than the server's Retry-After, and caps an absurd one", () => {
    expect(kubernetesFailureBackoffDelay(1, 30_000, () => 1)).toBe(30_000);
    expect(kubernetesFailureBackoffDelay(6, 1_000, () => 1)).toBe(failureBackoffCapMs);
    expect(kubernetesFailureBackoffDelay(1, 24 * 3_600_000, () => 1)).toBe(600_000);
  });
});

describe("kubernetesFailureRetryable", () => {
  it("follows the server's retryable flag before the status", () => {
    expect(kubernetesFailureRetryable(unavailable(true))).toBe(true);
    expect(kubernetesFailureRetryable(unavailable(false))).toBe(false);
    expect(kubernetesFailureRetryable(new ApiError(400, "bad", { retryable: true }))).toBe(true);
  });

  it("retries gateway, availability, timeout and network failures but not refusals", () => {
    expect([502, 503, 504, 429, 408].map((status) => kubernetesFailureRetryable(new ApiError(status, "x")))).toEqual([true, true, true, true, true]);
    expect([400, 401, 403, 404, 409].map((status) => kubernetesFailureRetryable(new ApiError(status, "x")))).toEqual([false, false, false, false, false]);
    expect(kubernetesFailureRetryable(new TypeError("Failed to fetch"))).toBe(true);
  });
});

describe("kubernetesFailureRefetchInterval", () => {
  it("backs off one step per new failure, never faster than the normal cadence, and resets on success", () => {
    const query = fakeQuery({ status: "error", error: unavailable(true), errorUpdatedAt: 1_000, dataUpdatedAt: 0 });
    const first = kubernetesFailureRefetchInterval(query, 1_000) as number;
    expect(first).toBeGreaterThanOrEqual(1_500);
    expect(first).toBeLessThanOrEqual(2_000);
    // The same failure asked twice is one step, not two.
    expect(kubernetesFailureRefetchInterval(query, 1_000)).toBe(first);
    // Asked again: pending keeps the step it is on.
    query.state = { ...query.state, status: "pending", error: null };
    expect(kubernetesFailureRefetchInterval(query, 1_000)).toBe(first);
    query.state = { ...query.state, status: "error", error: unavailable(true), errorUpdatedAt: 5_000 };
    const second = kubernetesFailureRefetchInterval(query, 1_000) as number;
    expect(second).toBeGreaterThanOrEqual(3_000);
    expect(second).toBeLessThanOrEqual(4_000);
    // Never faster than the cadence the caller asked for.
    query.state = { ...query.state, errorUpdatedAt: 9_000 };
    expect(kubernetesFailureRefetchInterval(query, 15_000)).toBe(15_000);
    // A success ends the streak.
    query.state = { status: "success", error: null, errorUpdatedAt: 9_000, dataUpdatedAt: 10_000 };
    expect(kubernetesFailureRefetchInterval(query, 1_000)).toBe(1_000);
    query.state = { status: "error", error: unavailable(true), errorUpdatedAt: 11_000, dataUpdatedAt: 10_000 };
    expect(kubernetesFailureRefetchInterval(query, 1_000)).toBeLessThanOrEqual(2_000);
  });

  it("does not poll a non-retryable failure or a read that never polls", () => {
    expect(kubernetesFailureRefetchInterval(fakeQuery({ status: "error", error: unavailable(false), errorUpdatedAt: 1, dataUpdatedAt: 0 }), 5_000)).toBe(false);
    expect(kubernetesFailureRefetchInterval(fakeQuery({ status: "error", error: unavailable(true), errorUpdatedAt: 1, dataUpdatedAt: 0 }), false)).toBe(false);
  });

  it("honours Retry-After", () => {
    const query = fakeQuery({ status: "error", error: unavailable(true, 45_000), errorUpdatedAt: 1, dataUpdatedAt: 0 });
    expect(kubernetesFailureRefetchInterval(query, 5_000)).toBe(45_000);
  });

  it("keeps the warming answer of a first sync on the caller's cadence", () => {
    const warming = new ApiError(503, "warming", { code: "inventory_not_ready", retryable: true });
    expect(kubernetesFailureRefetchInterval(fakeQuery({ status: "error", error: warming, errorUpdatedAt: 1, dataUpdatedAt: 0 }), 5_000)).toBe(5_000);
  });
});

describe("kubernetesFailureBackingOff", () => {
  it("holds a background refresh until the backoff ends, and a non-retryable failure until the reader asks", () => {
    const query = fakeQuery({ status: "error", error: unavailable(true), errorUpdatedAt: 1_000, dataUpdatedAt: 0 });
    kubernetesFailureRefetchInterval(query, 5_000);
    expect(kubernetesFailureBackingOff(query, 1_000 + 100)).toBe(true);
    expect(kubernetesFailureBackingOff(query, 1_000 + 1_499)).toBe(true);
    expect(kubernetesFailureBackingOff(query, 1_000 + 2_000)).toBe(false);
    expect(kubernetesFailureBackingOff(fakeQuery({ status: "error", error: unavailable(false), errorUpdatedAt: 1, dataUpdatedAt: 0 }), 10 ** 9)).toBe(true);
    expect(kubernetesFailureBackingOff(fakeQuery({ status: "success", error: null, errorUpdatedAt: 0, dataUpdatedAt: 1 }))).toBe(false);
    // A refusal without the server's word is still refreshed by a commit notice.
    expect(kubernetesFailureBackingOff(fakeQuery({ status: "error", error: new ApiError(403, "denied", { code: "history_authority_denied" }), errorUpdatedAt: 1, dataUpdatedAt: 0 }), 10 ** 9)).toBe(false);
  });
});

describe("retryAfterMilliseconds", () => {
  it("reads delta seconds and HTTP dates, and ignores anything else", () => {
    const now = Date.parse("2026-11-23T15:50:00Z");
    expect(retryAfterMilliseconds("30", now)).toBe(30_000);
    expect(retryAfterMilliseconds("Mon, 23 Nov 2026 15:51:00 GMT", now)).toBe(60_000);
    expect(retryAfterMilliseconds("Mon, 23 Nov 2026 15:49:00 GMT", now)).toBeUndefined();
    expect(retryAfterMilliseconds("soon", now)).toBeUndefined();
    expect(retryAfterMilliseconds(null, now)).toBeUndefined();
  });
});
