import { describe, expect, it } from "vitest";
import { ApiError, finopsErrorFrom } from "@/lib/api";
import { finopsStates, type FinOpsStateFixture } from "../../../tests/ui/finops/fixtures";
import {
  bridgeGeometry,
  commitmentOnlyLine,
  compareBaseName,
  compareRowName,
  isZeroAmount,
  partialMonthLabel,
  periodYear,
  fmtSince,
  staleNeedsAccessFix,
  staleReasonText,
  statedAmountSpan,
  usageSignalLabel,
  fmtChangePct,
  fmtMoney,
  fmtMoneyAxis,
  fmtShare,
  focusRowId,
  forecastDisplay,
  isNotFinal,
  ownerColumn,
  signalRatio,
  skippedCallsText,
  spendByLayout,
} from "@/lib/finops";

// One fixture per design page (named after the 09e slugs), built in code by
// versus-incident/tests/ui/finops/fixtures.ts.
type StateFixture = FinOpsStateFixture;
const fixtures = finopsStates();
const fx = (name: string): StateFixture => {
  const f = fixtures.get(name);
  if (!f) throw new Error(`missing fixture ${name}`);
  return f;
};

describe("finops fixtures", () => {
  it("has one fixture per design state", () => {
    const names = [...fixtures.keys()].sort();
    expect(names).toContain("09-finops");
    expect(names).toContain("09b-finops-why");
    for (let i = 1; i <= 21; i++) {
      const prefix = `09e-${String(i).padStart(2, "0")}-`;
      expect(names.some((n) => n.startsWith(prefix)), prefix).toBe(true);
    }
  });
});

describe("money and percent", () => {
  it("formats whole units with a real minus and an explicit plus", () => {
    expect(fmtMoney(43_520_000_000)).toBe("$43,520");
    expect(fmtMoney(1_980_000_000, { signed: true })).toBe("+$1,980");
    expect(fmtMoney(-380_000_000, { signed: true })).toBe("−$380");
    expect(fmtMoney(200_000)).toBe("$0.20");
    expect(fmtMoney(0)).toBe("$0");
  });
  it("formats axis, change and share", () => {
    expect(fmtMoneyAxis(36_000_000_000)).toBe("$36k");
    expect(fmtMoneyAxis(1_200_000_000)).toBe("$1,200");
    expect(fmtChangePct(913)).toBe("+9.1%");
    expect(fmtChangePct(-580)).toBe("−5.8%");
    expect(fmtShare(3470)).toBe("35%");
    expect(fmtShare(-110)).toBe("−1%");
  });
  it("words a usage ratio", () => {
    expect(signalRatio(600_000, 2_500_000)).toBe("4×");
    expect(signalRatio(1_000_000, 1_500_000)).toBe("1.5×");
    expect(signalRatio(0, 5)).toBe("");
  });
});

describe("bridge geometry", () => {
  for (const f of fixtures.values()) {
    if (!f.bridge.available || !(f.bridge.rows ?? []).length) continue;
    it(`drivers sum from base to total (${f.state})`, () => {
      const g = bridgeGeometry(f.bridge);
      expect(g.end).toBe(f.bridge.total_micros);
      expect(g.bars[0].kind).toBe("base");
      expect(g.bars[g.bars.length - 1].kind).toBe("total");
      for (const bar of g.bars) {
        if (bar.kind === "driver" || bar.kind === "other") {
          expect(Math.sign(bar.to - bar.from)).toBe(Math.sign(bar.amount));
        }
      }
      expect(g.lo).toBeLessThanOrEqual(Math.min(...g.bars.map((b) => Math.min(b.from === 0 ? b.to : b.from, b.to))));
      expect(g.hi).toBeGreaterThanOrEqual(Math.max(...g.bars.map((b) => Math.max(b.from, b.to))));
    });
  }

  it("marks the largest open increase as the one that matters", () => {
    expect(focusRowId(fx("09-finops").bridge.rows)).toBe("drv-nat");
    expect(focusRowId(fx("09e-06-cost-down").bridge.rows)).toBeNull();
    expect(focusRowId(fx("09e-13-cost-flat").bridge.rows)).toBeNull();
  });
});

describe("breakdown rule", () => {
  const layoutOf = (name: string) => {
    const b = fx(name).breakdown.service;
    return spendByLayout(b.options, b.default_group_by);
  };
  it("removes the card when nothing splits the cost", () => {
    expect(layoutOf("09e-18-single-region")).toEqual({ kind: "none" });
  });
  it("titles the card with the only option, no switch", () => {
    expect(layoutOf("09e-19-single-region-tagged")).toEqual({ kind: "single", groupBy: "team" });
  });
  it("removes Account with one account and opens on Region", () => {
    const l = layoutOf("09e-16-single-account");
    expect(l.kind).toBe("switch");
    if (l.kind !== "switch") return;
    expect(l.defaultGroupBy).toBe("region");
    expect(l.choices.map((c) => c.groupBy)).toEqual(["region", "team"]);
  });
  it("keeps Team in the switch, disabled with its reason, when tags are missing or uniform", () => {
    for (const name of ["09e-05-no-team-tags", "09e-14-same-tag"]) {
      const l = layoutOf(name);
      expect(l.kind).toBe("switch");
      if (l.kind !== "switch") return;
      const team = l.choices.find((c) => c.groupBy === "team");
      expect(team?.disabledReason, name).toBeTruthy();
      expect(l.defaultGroupBy).toBe("account");
    }
  });
  it("offers Group first and by default in the organization view", () => {
    const l = layoutOf("09e-20-organization");
    expect(l.kind === "switch" && l.defaultGroupBy).toBe("account_group");
    expect(l.kind === "switch" && l.choices.map((c) => c.label)).toEqual(["Group", "Account", "Region", "Team"]);
  });
  it("never offers a dominant or single value", () => {
    expect(
      spendByLayout(
        [
          { group_by: "account", available: false, reason: "dominant_value" },
          { group_by: "region", available: false, reason: "single_value" },
          { group_by: "team", available: true },
        ],
        "account",
      ),
    ).toEqual({ kind: "single", groupBy: "team" });
  });
});

describe("owner column", () => {
  it("shows when tags name two or more owners", () => {
    expect(ownerColumn(fx("09-finops").bridge, true)).toEqual({ show: true });
  });
  it("hides with one line when the tag is not active", () => {
    const o = ownerColumn(fx("09e-05-no-team-tags").bridge, false);
    expect(o).toEqual({ show: false, note: "Owner: no team tag", setUpTags: true });
  });
  it("hides when every row has the same tag", () => {
    const o = ownerColumn(fx("09e-14-same-tag").bridge, true);
    expect(o.show).toBe(false);
    expect(!o.show && o.note).toBe("Owner hidden: all cost is team=platform");
  });
  it("hides when no row has an owner", () => {
    expect(ownerColumn(fx("09e-18-single-region").bridge, true).show).toBe(false);
  });
  it("is never a column of Unallocated, even when the server allows it", () => {
    const b = structuredClone(fx("09-finops").bridge);
    for (const r of b.rows ?? []) r.owner = { name: "Unallocated", rule: "unallocated", rule_text: "" };
    const o = ownerColumn(b, true);
    expect(o.show).toBe(false);
    expect(!o.show && o.note).toBe("Owner not shown: no team tag on this cost");
  });
  it("hides when one named owner covers every row", () => {
    const b = structuredClone(fx("09-finops").bridge);
    b.owner_column = { show: true };
    for (const r of b.rows ?? []) r.owner = { name: "platform", rule: "tag", rule_text: "" };
    const o = ownerColumn(b, true);
    expect(!o.show && o.note).toBe("All cost has team=platform");
  });
});

describe("states", () => {
  it("knows when the bill is not final", () => {
    expect(isNotFinal(fx("09e-11-bill-not-closed").summary)).toBe(true);
    expect(isNotFinal(fx("09-finops").summary)).toBe(false);
    expect(isNotFinal(fx("09e-12-partial-day").summary)).toBe(false);
  });
  it("words the forecast without guessing", () => {
    expect(forecastDisplay(fx("09e-04-request-cap").summary.forecast)).toEqual({ kind: "skipped" });
    expect(forecastDisplay(fx("09e-15-forecast-unavailable").summary.forecast)).toMatchObject({ kind: "amount", estimate: true });
    expect(forecastDisplay({ month: "2026-11", amount_micros: 0, source: "none", reason: "not_enough_history" })).toEqual({ kind: "no_history" });
  });
  it("names skipped calls", () => {
    expect(skippedCallsText(["forecast", "coverage"])).toBe("Request cap reached: forecast and coverage skipped");
  });
});

describe("FinOps action failure wording", () => {
  const failed = (code: string) => new ApiError(400, code, { error: code });
  const words = (code: string, kind: "aws" | "local") => {
    const info = finopsErrorFrom(failed(code), kind);
    return [info.title, info.detail ?? ""];
  };

  it("words every drill-down code, falling back to the AWS table", () => {
    expect(words("paid_request_cap_reached", "aws")).toEqual(["Daily AWS request limit reached", "Try again tomorrow."]);
    expect(words("reserved_for_scheduled_refresh", "aws")).toEqual(["Requests kept for the daily refresh", "Try again after it runs."]);
    expect(words("store_unavailable", "aws")).toEqual(["Storage is unavailable", "Try again shortly."]);
    expect(words("cost_data_off", "aws")).toEqual(["Cost data is off", "Turn it on in AWS settings."]);
    expect(words("cost_period_too_large", "aws")).toEqual(["Period too large", "Choose a shorter period."]);
    expect(words("refresh_in_progress", "aws")).toEqual(["A cost refresh is running", "Try again in a few seconds."]);
    expect(words("throttled", "aws")[0]).toBe("AWS throttled the request");
  });

  it("words every mark-expected code", () => {
    expect(words("reason_required", "local")).toEqual(["Add a reason", ""]);
    expect(words("until_invalid", "local")).toEqual(["Pick a valid end date", ""]);
    expect(words("status_change_not_allowed", "local")).toEqual(["This status can't be changed", ""]);
  });

  it("words every save-report code", () => {
    expect(words("already_frozen", "local")).toEqual(["This month's report is already saved", ""]);
    expect(words("bill_not_closed", "local")).toEqual(["The bill isn't final yet", "Save the report after the 5th."]);
    expect(words("report_not_month", "local")).toEqual(["Pick a whole month", ""]);
  });

  it("words a used manual refresh", () => {
    expect(words("manual_refresh_used", "aws")).toEqual(["Today's manual refresh is used", "Try again tomorrow."]);
  });

  it("words an account missing from cost data without blaming AWS", () => {
    const [title, detail] = words("account_not_covered", "aws");
    expect([title, detail]).toEqual(["This account isn't in cost data", "Check the account in AWS settings."]);
    expect(title).not.toMatch(/AWS request failed/);
  });

  it("words an amount that is too large", () => {
    expect(words("amount_overflow", "local")).toEqual(["Amount too large", "Enter a smaller amount."]);
  });

  it("words an invalid budget", () => {
    expect(words("invalid_budget", "local")).toEqual(["Check the budget", "Enter a positive amount."]);
  });

  it("words a query the server could not read", () => {
    expect(words("bad_query", "aws")).toEqual(["Can't show this view", "Reload the page."]);
  });

  it("words a missing item", () => {
    expect(words("not_found", "local")).toEqual(["Not found", "It may have been removed."]);
  });

  it("falls back to Could not save with no mention of AWS for actions that stay in Versus", () => {
    for (const e of [failed("something_new"), failed("throttled"), new Error("boom"), "boom"]) {
      const info = finopsErrorFrom(e, "local");
      expect(info).toEqual({ title: "Could not save", detail: "Try again." });
      expect(`${info.title} ${info.detail}`).not.toMatch(/AWS/);
    }
  });
});

describe("partial periods and quiet wording", () => {
  it("names part of a month by its days and leaves a whole month to the server's name", () => {
    expect(partialMonthLabel({ start: "2026-10-01", end: "2026-10-09" })).toBe("Oct 1–9");
    expect(partialMonthLabel({ start: "2026-10-01", end: "2026-10-01" })).toBe("Oct 1");
    expect(partialMonthLabel({ start: "2026-10-01", end: "2026-10-31" })).toBe("");
    expect(partialMonthLabel({ start: "2024-02-01", end: "2024-02-29" })).toBe("");
    expect(partialMonthLabel({ start: "2026-09-11", end: "2026-10-10" })).toBe("");
    expect(partialMonthLabel(undefined)).toBe("");
  });

  it("names the year of a base that sits in another year than the period", () => {
    const lastYear = { short: "November", range: { start: "2025-11-01", end: "2025-11-09" } };
    expect(partialMonthLabel(lastYear.range, 2026)).toBe("Nov 1–9, 2025");
    expect(partialMonthLabel(lastYear.range, 2025)).toBe("Nov 1–9");
    expect(compareBaseName(lastYear, 2026)).toBe("Nov 1–9, 2025");
    expect(compareRowName(lastYear, 2026)).toBe("Nov 1–9, 2025");
    const wholeLastYear = { short: "November", range: { start: "2025-11-01", end: "2025-11-30" } };
    expect(compareBaseName(wholeLastYear, 2026)).toBe("November 2025 total");
    expect(compareRowName(wholeLastYear, 2026)).toBe("November 2025");
    // Across the new year the previous month is named with its year too.
    const december = { short: "December", range: { start: "2025-12-01", end: "2025-12-31" } };
    expect(compareBaseName(december, 2026)).toBe("December 2025 total");
    // Same year: no year added.
    expect(compareBaseName({ short: "October", range: { start: "2026-10-01", end: "2026-10-31" } }, 2026)).toBe("October total");
    expect(periodYear({ range: { start: "2026-11-01", end: "2026-11-09" } })).toBe(2026);
    expect(periodYear(undefined)).toBeUndefined();
  });

  it("calls a partial base by its days, never the month's total", () => {
    const oct = { short: "October", range: { start: "2026-10-01", end: "2026-10-09" } };
    expect(compareBaseName(oct)).toBe("Oct 1–9");
    expect(compareRowName(oct)).toBe("Oct 1–9");
    const full = { short: "October", range: { start: "2026-10-01", end: "2026-10-31" } };
    expect(compareBaseName(full)).toBe("October total");
    expect(compareRowName(full)).toBe("October");
  });

  it("puts the usage unit in the label so the label and the figures agree", () => {
    expect(usageSignalLabel("instances billed per day", "Hrs/day")).toBe("instances, hours per day");
    expect(usageSignalLabel("NAT bytes processed", "TB/day")).toBe("NAT bytes processed, TB per day");
    expect(usageSignalLabel("storage billed per day", "GB-Mo/day")).toBe("storage, GB-months per day");
  });

  it("words a commitment-only savings total, and nothing when a saving is counted", () => {
    expect(commitmentOnlyLine({ count: 0, commitment_monthly_micros: 441_000_000, commitment_count: 1 })).toBe(
      "Up to $441 a month, before other savings",
    );
    expect(commitmentOnlyLine({ count: 1, commitment_monthly_micros: 441_000_000, commitment_count: 1 })).toBe("");
    expect(commitmentOnlyLine({ count: 0, commitment_monthly_micros: 0, commitment_count: 0 })).toBe("");
  });

  it("finds the stated amount as the sentence writes it, cents included", () => {
    const sentence = "Virtual machines in Harness account went up $69.33 from Oct 1–9";
    const span = statedAmountSpan(sentence, 69_330_000);
    expect(span && sentence.slice(span[0], span[1])).toBe("$69.33");
    const big = "Network traffic in Production went up $1,980 in October";
    const s2 = statedAmountSpan(big, 1_980_000_000);
    expect(s2 && big.slice(s2[0], s2[1])).toBe("$1,980");
    expect(statedAmountSpan(sentence, 0)).toBeNull();
    expect(statedAmountSpan("went up from Oct 20 to Oct 27", 44_000_000)).toBeNull();
  });

  it("words a stale cause, and says when it needs a fix in AWS settings", () => {
    expect(staleReasonText("no_server_identity")).toBe("The Versus server has no AWS identity");
    expect(staleReasonText("account_disconnected")).toBe("The account is disconnected");
    expect(staleReasonText("throttled")).toBe("AWS throttled the refresh");
    expect(staleNeedsAccessFix("no_server_identity")).toBe(true);
    expect(staleNeedsAccessFix("throttled")).toBe(false);
    expect(staleNeedsAccessFix(undefined)).toBe(false);
    expect(fmtSince("2026-11-11T05:18:00Z")).toBe("Nov 11 05:18 UTC");
    expect(fmtSince("2026-11-09")).toBe("Nov 9");
  });

  it("treats amounts that print as $0.00 as nothing", () => {
    expect(isZeroAmount(4_999)).toBe(true);
    expect(isZeroAmount(-4_999)).toBe(true);
    expect(isZeroAmount(5_000)).toBe(false);
    expect(fmtMoney(4_999)).toBe("$0.00");
  });
});
