// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import path from "node:path";
import { ApiError, api, type FinOpsReportView } from "@/lib/api";
import { FinOpsReportPage } from "./FinOpsReportPage";
import { finopsReportFixture } from "../../../../../tests/ui/finops/fixtures";

// The print route renders the report model built by
// versus-incident/tests/ui/finops/fixtures.ts (the 09c design).
const fixture = finopsReportFixture();

vi.mock("@/lib/api", async (importActual) => {
  const actual = await importActual<typeof import("@/lib/api")>();
  return {
    ...actual,
    api: { ...actual.api, finopsReport: vi.fn(), getSSODeployment: vi.fn() },
  };
});

function renderReport(report: FinOpsReportView, route = "/aws/finops/report?period=2026-10") {
  vi.mocked(api.finopsReport).mockResolvedValue(report);
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={[route]}>
        <Routes>
          <Route path="/aws/finops/report" element={<FinOpsReportPage />} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

const text = () => document.body.textContent ?? "";

afterEach(cleanup);
beforeEach(() => {
  vi.clearAllMocks();
  vi.mocked(api.getSSODeployment).mockRejectedValue(new ApiError(404, "not found"));
  window.print = vi.fn();
});

describe("FinOps report print route", () => {
  it("renders two pages from the report model", async () => {
    renderReport(fixture.report);
    const page1 = await screen.findByRole("region", { name: "Page 1" });
    const page2 = screen.getByRole("region", { name: "Page 2" });
    expect(document.querySelectorAll(".finops-page")).toHaveLength(2);
    expect(page1.getAttribute("data-page")).toBe("1");

    const p1 = page1.textContent ?? "";
    for (const s of [
      "AWS cost report: October 2026",
      "Frozen Nov 5",
      "Deployment: default, 3 accounts",
      "$43,520",
      "+$3,640",
      "Over budget by $2,900",
      "What moved the cost",
      "New release uses a paid gateway; a private route fixes it",
      "6 to 9 servers for a load test; no action needed",
      "Untagged reporting copy; data team to claim or remove",
      "Expected",
      "Page 1 of 2",
    ]) {
      expect(p1, s).toContain(s);
    }
    const p2 = page2.textContent ?? "";
    for (const s of [
      "Spend by service",
      "Spend by account",
      "Savings found: $1,890 a month",
      "up to $610 a month, before other savings",
      "or: Stop reporting-db outside working hours $90",
      "VPC endpoints for S3, ECR in vpc-prod",
      "Expected increases",
      "Planned load test until Nov 15",
      "Marked Oct 14",
      "About this report",
      "Net amortized, after discounts and credits",
      "To Oct 31, complete",
      "Nov 5 09:14 UTC",
      "Read only",
      "Page 2 of 2",
    ]) {
      expect(p2, s).toContain(s);
    }
    // One sentence under each of the top three drivers only.
    expect(page1.querySelectorAll("tbody tr td[colspan]")).toHaveLength(3);
  });

  it("is quiet: no ⓘ buttons, no AWS actions", async () => {
    renderReport(fixture.report);
    await screen.findByRole("region", { name: "Page 1" });
    expect(screen.queryAllByRole("button", { name: /^About / })).toHaveLength(0);
    expect(text()).not.toMatch(/\b(Approve|Run)\b/);
    expect(text()).not.toMatch(/NaN|undefined|null/);
  });

  it("prints no resource marker when the bridge has no resource note", async () => {
    renderReport({ ...fixture.report, bridge: { ...fixture.report.bridge, resource_note: undefined } });
    const card = await screen.findByRole("region", { name: "What moved the cost" });
    expect(card.textContent).toContain("+$1,980");
    expect(card.textContent).not.toContain("Resources not named");
    expect(card.textContent).not.toContain("* ");
  });

  it("prints the resource status word and the note once as a footnote when resource detail is too large", async () => {
    const note = "Resource detail is too large to read for this period, so resources are not named; the amounts are complete";
    renderReport({ ...fixture.report, bridge: { ...fixture.report.bridge, resource_note: note } });
    const card = await screen.findByRole("region", { name: "What moved the cost" });
    const t = card.textContent ?? "";
    expect(within(card).getByText("Resources not named*")).toBeTruthy();
    // On paper the note is printed, once, under the card; no ⓘ to open.
    expect(t.split(note)).toHaveLength(2);
    expect(within(card).getByText(`* ${note}`).tagName).toBe("P");
    expect(within(card).queryAllByRole("button")).toHaveLength(0);
    // The drivers and amounts still print; nothing reads as empty or zero.
    expect(t).toContain("+$1,980");
    expect(t).not.toMatch(/no resources?/i);
    expect(t).not.toMatch(/\$0(?![\d,.])/);
  });

  it("prints the resource status word on the marked row only and keeps a single footnote", async () => {
    const note = "Resource detail is too large to read for this period, so resources are not named; the amounts are complete";
    const rows = (fixture.report.bridge.rows ?? []).map((r) => (r.id === "drv-nat" ? { ...r, resources: "too_large" } : r));
    renderReport({ ...fixture.report, bridge: { ...fixture.report.bridge, rows, resource_note: note } });
    const card = await screen.findByRole("region", { name: "What moved the cost" });
    const marked = within(card).getByText("Network traffic").closest("tbody")!;
    const normal = within(card).getByText("More servers").closest("tbody")!;
    expect(marked.textContent).toContain("Resources not named*");
    expect(normal.textContent).not.toContain("Resources not named");
    // Header word plus the row word; the note itself prints once.
    expect(within(card).getAllByText("Resources not named*")).toHaveLength(2);
    expect((card.textContent ?? "").split(note)).toHaveLength(2);
    for (const tb of [marked, normal]) {
      const t = tb.textContent ?? "";
      expect(t).not.toMatch(/no resources?/i);
      expect(t).not.toMatch(/\$0(?![\d,.])/);
    }
    expect(marked.textContent).toContain("+$1,980");
    expect(normal.textContent).toContain("+$1,240");
  });

  it("is forced light", async () => {
    const { container } = renderReport(fixture.report);
    await screen.findByRole("region", { name: "Page 1" });
    expect(container.querySelector(".finops-report")?.getAttribute("data-theme")).toBe("light");
  });

  it("makes no request after it loads", async () => {
    const fetchSpy = vi.fn(() => Promise.reject(new Error("network used after load")));
    vi.stubGlobal("fetch", fetchSpy);
    renderReport(fixture.report);
    await screen.findByRole("region", { name: "Page 2" });
    await waitFor(() => expect(api.getSSODeployment).toHaveBeenCalled());
    const reportCalls = vi.mocked(api.finopsReport).mock.calls.length;
    const orgCalls = vi.mocked(api.getSSODeployment).mock.calls.length;
    window.dispatchEvent(new Event("focus"));
    window.dispatchEvent(new Event("online"));
    await new Promise((r) => setTimeout(r, 50));
    expect(vi.mocked(api.finopsReport).mock.calls.length).toBe(reportCalls);
    expect(reportCalls).toBe(1);
    expect(vi.mocked(api.getSSODeployment).mock.calls.length).toBe(orgCalls);
    expect(fetchSpy).not.toHaveBeenCalled();
    vi.unstubAllGlobals();
  });

  it("says Not final while the bill is open", async () => {
    const r = structuredClone(fixture.report);
    r.frozen = null;
    r.can_freeze = false;
    r.not_final = true;
    renderReport(r);
    const page1 = await screen.findByRole("region", { name: "Page 1" });
    expect(within(page1).getByText("Not final")).toBeTruthy();
    expect(text()).not.toContain("Frozen");
    expect(text()).toContain("To Oct 31, not final");
    expect(text()).toContain("Generated");
  });

  it("opens the print dialog once with ?print=1", async () => {
    renderReport(fixture.report, "/aws/finops/report?period=2026-10&print=1");
    await screen.findByRole("region", { name: "Page 1" });
    await waitFor(() => expect(window.print).toHaveBeenCalledOnce());
  });

  it("asks for the requested month", async () => {
    renderReport(fixture.report);
    await screen.findByRole("region", { name: "Page 1" });
    expect(vi.mocked(api.finopsReport).mock.calls[0][0]).toEqual({ period: "2026-10", compare: undefined });
    expect(window.print).not.toHaveBeenCalled();
  });

  it("names a month-to-date base by its days and the total as to date", async () => {
    const r = structuredClone(fixture.report);
    r.period = {
      key: "mtd", label: "November 2026", short: "November",
      range: { start: "2026-11-01", end: "2026-11-09" }, calendar: { start: "2026-11-01", end: "2026-11-30" },
      open: true, complete_days: 9,
    };
    r.compare = { key: "prev_period", label: "October", short: "October", range: { start: "2026-10-01", end: "2026-10-09" }, available: true };
    renderReport(r);
    const card = await screen.findByRole("region", { name: "What moved the cost" });
    const names = Array.from(card.querySelectorAll("tbody th[scope=row]")).map((th) => th.textContent);
    expect(names[0]).toBe("Oct 1–9");
    expect(names[names.length - 1]).toBe("November to date");
    expect(names).not.toContain("October");
  });

  it("prints no $0 savings total when nothing adds to it", async () => {
    const r = structuredClone(fixture.report);
    r.savings = { ...r.savings, total: { monthly_micros: 0, count: 0, commitment_monthly_micros: 0, commitment_count: 0 } };
    renderReport(r);
    const section = await screen.findByRole("region", { name: "Savings found" });
    expect(within(section).getByRole("heading").textContent).toBe("Savings found");
    expect(section.textContent).not.toMatch(/\$0 a month/);
  });

  it("prints the commitment line alone when only commitments were found", async () => {
    const r = structuredClone(fixture.report);
    r.savings = { top: [], total: { monthly_micros: 0, count: 0, commitment_monthly_micros: 441_000_000, commitment_count: 1 } };
    renderReport(r);
    const section = await screen.findByRole("region", { name: "Savings found" });
    expect(section.textContent).toContain("Up to $441 a month, before other savings");
    expect(section.textContent).not.toContain("None found");
    expect(section.textContent).not.toMatch(/\$0(?![\d,.])/);
  });

  it("fits a phone and covers the window on screen while paper keeps A4", () => {
    const css = readFileSync(path.join(path.dirname(fileURLToPath(import.meta.url)), "finops-print.css"), "utf8").replace(/\s+/g, " ");
    // Its own scroll layer over the whole window, so the light theme reaches the bottom.
    expect(css).toMatch(/\.finops-report \{ position: fixed; inset: 0; overflow: auto;/);
    // A phone gets the sheet at the screen's width.
    const phone = css.match(/@media screen and \(max-width: 840px\) \{(.*?)\} \}/)?.[1] ?? "";
    expect(phone).toMatch(/\.finops-page \{ width: auto;/);
    // Print undoes the screen layer and keeps the A4 page height.
    const print = css.slice(css.indexOf("@media print"));
    expect(print).toMatch(/\.finops-report \{ position: static; overflow: visible;/);
    expect(print).toContain("height: calc(279mm - 20mm)");
  });
});
