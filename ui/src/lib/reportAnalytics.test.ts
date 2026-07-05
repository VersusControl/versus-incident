import { describe, it, expect } from "vitest";
import type { Capabilities, ReportSendResult } from "@/lib/api";
import {
  canReport,
  defaultReportChannel,
  defaultReportWindow,
  hasReportChannel,
  isReportWindow,
  reportChannels,
  summarizeReportOutcome,
} from "@/lib/reportAnalytics";

const report = (over?: Partial<NonNullable<Capabilities["report"]>>) => ({
  enable: true,
  default_channel: "",
  default_window: "today",
  include_chart: true,
  channels: [] as string[],
  public_host_set: false,
  ...over,
});

const cap = (r?: Capabilities["report"]): Capabilities => ({
  search: false,
  report: r,
});

describe("canReport", () => {
  it("is false when the server omits the report block (older server)", () => {
    expect(canReport(cap(undefined))).toBe(false);
    expect(canReport(undefined)).toBe(false);
  });
  it("is false when the feature is disabled", () => {
    expect(canReport(cap(report({ enable: false })))).toBe(false);
  });
  it("is true when the feature is enabled", () => {
    expect(canReport(cap(report({ enable: true, channels: ["slack"] })))).toBe(
      true,
    );
  });
});

describe("isReportWindow", () => {
  it("accepts the closed set", () => {
    expect(isReportWindow("today")).toBe(true);
    expect(isReportWindow("24h")).toBe(true);
    expect(isReportWindow("7d")).toBe(true);
  });
  it("rejects anything else", () => {
    expect(isReportWindow("year")).toBe(false);
    expect(isReportWindow(undefined)).toBe(false);
  });
});

describe("defaultReportWindow", () => {
  it("uses the runtime default_window when valid", () => {
    expect(defaultReportWindow(cap(report({ default_window: "7d" })))).toBe(
      "7d",
    );
  });
  it("falls back to today for an invalid/absent default", () => {
    expect(defaultReportWindow(cap(report({ default_window: "year" })))).toBe(
      "today",
    );
    expect(defaultReportWindow(undefined)).toBe("today");
  });
});

describe("no-channel degrade", () => {
  it("reports no channel when the enabled list is empty", () => {
    const c = cap(report({ channels: [] }));
    expect(hasReportChannel(c)).toBe(false);
    expect(reportChannels(c)).toEqual([]);
  });
  it("reports channels when configured", () => {
    const c = cap(
      report({ default_channel: "telegram", channels: ["slack", "telegram"] }),
    );
    expect(hasReportChannel(c)).toBe(true);
    expect(reportChannels(c)).toEqual(["slack", "telegram"]);
  });
});

describe("defaultReportChannel", () => {
  it("uses the configured default when it is enabled", () => {
    expect(
      defaultReportChannel(
        cap(
          report({
            default_channel: "telegram",
            channels: ["slack", "telegram"],
          }),
        ),
      ),
    ).toBe("telegram");
  });
  it("falls back to the first enabled channel when default is not enabled", () => {
    expect(
      defaultReportChannel(
        cap(
          report({ default_channel: "email", channels: ["slack", "telegram"] }),
        ),
      ),
    ).toBe("slack");
  });
  it("is empty when no channel is enabled", () => {
    expect(defaultReportChannel(cap(report({ channels: [] })))).toBe("");
  });
});

describe("summarizeReportOutcome", () => {
  const res = (o: Partial<ReportSendResult>): ReportSendResult => ({
    window: "today",
    sent: [],
    fallback: [],
    failed: {},
    bytes: 0,
    ...o,
  });

  it("ok when the image was sent", () => {
    const s = summarizeReportOutcome(res({ sent: ["slack"], bytes: 42 }));
    expect(s.tone).toBe("ok");
    expect(s.description).toContain("image sent to slack");
  });

  it("warn(info) when only a text fallback was delivered", () => {
    const s = summarizeReportOutcome(res({ fallback: ["msteams"] }));
    expect(s.tone).toBe("info");
    expect(s.description).toContain("text summary to msteams");
  });

  it("error when any channel failed, but still notes what got through", () => {
    const s = summarizeReportOutcome(
      res({ sent: ["slack"], failed: { telegram: "boom" } }),
    );
    expect(s.tone).toBe("error");
    expect(s.description).toContain("image sent to slack");
    expect(s.description).toContain("failed: telegram");
  });
});
