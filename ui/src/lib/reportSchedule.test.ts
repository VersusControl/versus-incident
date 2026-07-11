import { afterEach, describe, expect, it, vi } from "vitest";
import {
  detectLocalZone,
  resolveTimezone,
  scheduleSummary,
  timezoneKind,
  windowClause,
} from "@/lib/reportSchedule";

describe("timezoneKind", () => {
  it("maps the literal 'UTC' to the UTC option", () => {
    expect(timezoneKind("UTC")).toBe("utc");
  });
  it("maps any concrete IANA name to the Local option", () => {
    expect(timezoneKind("Asia/Ho_Chi_Minh")).toBe("local");
    expect(timezoneKind("America/New_York")).toBe("local");
  });
});

describe("resolveTimezone", () => {
  it("stores 'UTC' when the UTC option is picked (ignoring the local zone)", () => {
    expect(resolveTimezone("utc", "Asia/Ho_Chi_Minh")).toBe("UTC");
  });
  it("stores the detected IANA zone when the Local option is picked", () => {
    expect(resolveTimezone("local", "Asia/Ho_Chi_Minh")).toBe(
      "Asia/Ho_Chi_Minh",
    );
  });
});

describe("detectLocalZone", () => {
  afterEach(() => {
    vi.restoreAllMocks();
  });

  it("returns the browser's resolved IANA zone", () => {
    vi.spyOn(Intl, "DateTimeFormat").mockReturnValue({
      resolvedOptions: () => ({ timeZone: "Asia/Ho_Chi_Minh" }),
    } as unknown as Intl.DateTimeFormat);
    expect(detectLocalZone()).toBe("Asia/Ho_Chi_Minh");
  });

  it("falls back to UTC when the runtime cannot resolve a zone", () => {
    vi.spyOn(Intl, "DateTimeFormat").mockImplementation(() => {
      throw new Error("no Intl");
    });
    expect(detectLocalZone()).toBe("UTC");
  });
});

describe("scheduleSummary", () => {
  it("renders the daily line with time, zone and the window clause", () => {
    expect(scheduleSummary("17:00", "Asia/Ho_Chi_Minh", "24h")).toBe(
      "Daily at 17:00 (Asia/Ho_Chi_Minh) — sends the last 24h",
    );
    expect(scheduleSummary("09:30", "UTC", "7d")).toBe(
      "Daily at 09:30 (UTC) — sends the last 7 days",
    );
  });
});

describe("windowClause", () => {
  it("has a phrase per known window", () => {
    expect(windowClause("today")).toBe("sends today's incidents");
    expect(windowClause("24h")).toBe("sends the last 24h");
    expect(windowClause("7d")).toBe("sends the last 7 days");
  });
  it("falls back generically for an unknown window", () => {
    expect(windowClause("30d")).toBe("sends the 30d window");
  });
});
