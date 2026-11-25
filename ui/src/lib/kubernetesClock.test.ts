import { afterAll, afterEach, beforeAll, describe, expect, it, vi } from "vitest";
import { ApiError } from "./api";
import { clockLabel, datedClock, notToday } from "./kubernetesClock";
import { refreshFailureLabel } from "./kubernetesLiveFailure";

// The reader is in UTC+7, so a UTC timestamp from late afternoon is already
// tomorrow for them: the day boundary is the reader's, not the server's.
const zone = process.env.TZ;
beforeAll(() => { process.env.TZ = "Asia/Ho_Chi_Minh"; });
afterAll(() => { process.env.TZ = zone; });
afterEach(() => { vi.useRealTimers(); });

const now = Date.parse("2026-11-24T19:45:00Z"); // 02:45 on Nov 25 in UTC+7
// Expected dates come from the same locale the code formats with, so the
// tests hold under any LANG.
const day = (iso: string, year = false) => new Date(iso).toLocaleDateString([], { month: "short", day: "numeric", ...(year ? { year: "numeric" } : {}) });
const hm = (iso: string) => new Date(iso).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });

describe("dated clock times", () => {
  it("runs in the reader's time zone", () => {
    expect(new Date(now).getDate()).toBe(25);
    expect(new Date(now).getHours()).toBe(2);
  });

  it("shows only the time for today and adds the date for an earlier day", () => {
    expect(datedClock("2026-11-24T19:00:00Z", undefined, now)).toBe(hm("2026-11-24T19:00:00Z"));
    // 16:59 UTC on Nov 24 is 23:59 on Nov 24 for the reader: yesterday.
    expect(datedClock("2026-11-24T16:59:00Z", undefined, now)).toBe(`${day("2026-11-24T16:59:00Z")}, ${hm("2026-11-24T16:59:00Z")}`);
    expect(datedClock("2025-11-21T08:36:45Z", { hour12: false }, now)).toBe(`${day("2025-11-21T08:36:45Z", true)}, ${new Date("2025-11-21T08:36:45Z").toLocaleTimeString([], { hour12: false })}`);
  });

  it("answers no time for a value that is not one", () => {
    expect(datedClock(undefined)).toBeUndefined();
    expect(datedClock("not a time")).toBeUndefined();
    expect(clockLabel("", undefined, "Time unavailable")).toBe("Time unavailable");
  });

  it("tells a time from another day apart from today's", () => {
    expect(notToday("2026-11-24T16:59:00Z", now)).toBe(true);
    expect(notToday("2026-11-24T17:00:00Z", now)).toBe(false);
  });
});

describe("refresh failure wording", () => {
  const earlier = "2026-11-24T16:59:00Z";
  it("names the history database and dates the data shown", () => {
    const error = new ApiError(503, "The history database read timed out.", { code: "history_store_timeout", retryable: true });
    expect(refreshFailureLabel(error, earlier, 30_000, now)).toBe(`The history database is not responding — showing data as of ${day(earlier)}, ${hm(earlier)}`);
    const unavailable = new ApiError(503, "down", { code: "history_store_unavailable" });
    expect(refreshFailureLabel(unavailable, earlier, 30_000, now)).toMatch(/^The history database is not responding/);
  });

  it("tells the reader to try again after a server timeout, with the server's retry time first", () => {
    const error = new ApiError(504, "timed out", { code: "server_timeout" });
    expect(refreshFailureLabel(error, now - 60_000, 30_000, now)).toBe(`The server timed out — try again in 30s; showing data as of ${new Date(now - 60_000).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })}`);
    error.retryAfterMs = 5_000;
    expect(refreshFailureLabel(error, now - 60_000, 30_000, now)).toMatch(/^The server timed out — try again in 5s; showing data as of /);
  });

  it("keeps the general wording for any other failure", () => {
    expect(refreshFailureLabel(new ApiError(500, "boom"), earlier, 30_000, now)).toBe(`Couldn't refresh — showing data as of ${day(earlier)}, ${hm(earlier)}`);
  });
});
