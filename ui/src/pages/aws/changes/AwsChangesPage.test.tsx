// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Route, Routes, useNavigate } from "react-router-dom";
import { ToastProvider } from "@/components/Toast";
import { api, ApiError } from "@/lib/api";
import {
  changesQueryString,
  clockSkewText,
  compareToOptions,
  fmtClock,
  fmtClockOrDay,
  fmtDayClock,
  fmtSpanOrDay,
  gapReasonText,
  lateArrivalsText,
  pollInterval,
  routineText,
  topActorLabels,
  warningLine,
  type ChangesQuery,
  type ChangesView,
  type CompareQuery,
  type CompareView,
} from "@/lib/awsChanges";
import { idHintText, partialLines } from "./compareWords";
import { AwsChangesPage, PANEL_HEIGHT } from "./AwsChangesPage";
import { HATCH } from "./ActivityGrid";
import {
  ALARM,
  BURST,
  NOW,
  PROD,
  SHARED,
  STACK_GROUP,
  STAGING,
  accounts,
  amendmentStates,
  browserPassStates,
  SUSPECT,
  at,
  buckets,
  changesStates,
  item,
  usualCompare,
  usualItems,
  usualView,
  utc,
  type ChangesStateFixture,
} from "./changesFixtures";

// Every worst-case page of the Changes screen (07e-01 … 07e-17) renders from
// a fixture built in code, plus the completeness, keyset paging and layout
// rules the page owes the reader: plain numbers in one summary card, one
// warning line whose popover names every gap, and no hedged counts.

vi.mock("@/lib/api", async (importActual) => {
  const actual = await importActual<typeof import("@/lib/api")>();
  return {
    ...actual,
    api: {
      ...actual.api,
      awsChanges: vi.fn(),
      awsChangesGroup: vi.fn(),
      awsChangesCompare: vi.fn(),
      awsChangesPreferences: vi.fn(),
      saveAwsChangesPreferences: vi.fn(),
    },
  };
});

vi.mock("@/components/TopBar", () => ({
  TopBar: ({ title, subtitle, actions }: { title: string; subtitle?: string; actions?: React.ReactNode }) => (
    <header>
      <span>
        {title} {subtitle}
      </span>
      {actions}
    </header>
  ),
}));

const states = changesStates();

function serve(view: ChangesView, compare: CompareView = usualCompare()) {
  vi.mocked(api.awsChanges).mockResolvedValue(view);
  vi.mocked(api.awsChangesCompare).mockResolvedValue(compare);
}

function serveState(name: string) {
  const f = states[name];
  if (!f) throw new Error(`missing fixture ${name}`);
  serve(f.view, f.compare);
  return f;
}

function renderPage(route = "/aws/changes") {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <ToastProvider>
        <MemoryRouter initialEntries={[route]} future={{ v7_startTransition: true, v7_relativeSplatPath: true }}>
          <Routes>
            <Route path="/aws/changes" element={<AwsChangesPage />} />
          </Routes>
        </MemoryRouter>
      </ToastProvider>
    </QueryClientProvider>,
  );
}

/** The summary card, once the first page has rendered. */
async function summary(): Promise<HTMLElement> {
  return waitFor(() => {
    const el = document.querySelector("[data-summary-card]");
    if (!el) throw new Error("summary card not rendered");
    return el as HTMLElement;
  });
}

/** One number in the summary card's row, e.g. num("changes") → "37". */
function num(k: string): string | null {
  return document.querySelector(`[data-number="${k}"] dd`)?.textContent ?? null;
}

function warningText(): string | null {
  return document.querySelector("[data-warning-text]")?.textContent ?? null;
}

/** Opens the warning line's popover and returns its panel. */
function openWarning(): HTMLElement {
  const trigger = document.querySelector("[data-warning-line] button[aria-haspopup='dialog']") as HTMLElement;
  fireEvent.click(trigger);
  return document.querySelector("[data-warning-popover]") as HTMLElement;
}

function likelyText(): string | null {
  return document.querySelector("[data-likely-cause-text]")?.textContent ?? null;
}

function rowTotals(): string[] {
  return [...document.querySelectorAll("[data-row-total]")].map((e) => e.textContent ?? "");
}

async function compareBox(): Promise<HTMLElement> {
  return waitFor(() => {
    const p = document.querySelector("[data-compare]");
    if (!p) throw new Error("not yet");
    return p as HTMLElement;
  });
}

/** Opens the Compare card's "N groups not compared" popover. */
async function openNotCompared(): Promise<HTMLElement> {
  const trigger = await waitFor(() => {
    const t = document.querySelector("[data-not-compared]");
    if (!t) throw new Error("not yet");
    return t as HTMLElement;
  });
  fireEvent.click(trigger);
  return document.querySelector("[data-compare-partial]") as HTMLElement;
}

beforeEach(() => {
  // jsdom has no layout; the Dropdown and Show › scroll into view.
  Element.prototype.scrollIntoView = vi.fn();
  vi.mocked(api.awsChangesGroup).mockResolvedValue({ items: [], next_cursor: null });
  // These pages answer for every service; the service filter has its own tests.
  vi.mocked(api.awsChangesPreferences).mockResolvedValue({ services: [] });
  vi.mocked(api.saveAwsChangesPreferences).mockResolvedValue({ services: [] });
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("Changes page — the usual day", () => {
  it("answers who changed what in plain numbers, names the likely cause, and has no Approve or Run control", async () => {
    serve(usualView());
    renderPage();
    await summary();
    expect([num("changes"), num("people"), num("pipeline"), num("aws_service"), num("errors"), num("accounts")]).toEqual(["37", "3", "20", "14", "214", "3/3"]);
    expect(likelyText()).toBe(SUSPECT.summary);
    expect(warningText()).toBeNull();
    const suspectRow = document.querySelector(`[data-item="${SUSPECT.id}"]`) as HTMLElement;
    expect(within(suspectRow).getByText("Likely cause")).toBeTruthy();
    expect(document.querySelector("[data-suspect]")?.getAttribute("aria-label")).toContain("Automation");
    expect(document.querySelector(`[data-alarm-line="${ALARM.id}"]`)).toBeTruthy();
    expect(screen.queryByRole("button", { name: /approve|run/i })).toBeNull();
  });

  it("asks the contract's route with the window, filters and page size", async () => {
    serve(usualView());
    renderPage("/aws/changes?window=1h&account=204811974821&region=eu-west-1");
    await summary();
    const q = vi.mocked(api.awsChanges).mock.calls[0][0] as ChangesQuery;
    expect(q).toMatchObject({ window: "1h", account: "204811974821", region: "eu-west-1", limit: 50, show_routine: false });
  });
});

describe("Changes page — worst cases (07e)", () => {
  it("01 first sync: the warning line says the first read is running and nothing says No changes", async () => {
    serveState("01-first-sync");
    renderPage();
    await summary();
    expect(warningText()).toBe("Reading the last 24 h…");
    expect(document.querySelector("[data-warning-line] button[aria-haspopup='dialog']")).toBeNull();
    expect(document.querySelector("[data-no-changes]")).toBeNull();
    expect(screen.queryByText(/No changes/)).toBeNull();
  });

  it("02 CloudTrail denied in one account: the warning names it; the popover carries the permission and Copy fix", async () => {
    serveState("02-cloudtrail-denied");
    renderPage();
    await summary();
    expect(num("changes")).toBe("31");
    expect(num("accounts")).toBe("2/3");
    expect(warningText()).toBe("Data not complete: Versus can't read CloudTrail in Staging.");
    const pop = openWarning();
    const row = pop.querySelector('[data-stretch="cloudtrail_denied"]') as HTMLElement;
    expect(row.textContent).toContain("Staging eu-west-1");
    expect(row.querySelector("[data-stretch-reason]")?.textContent).toBe("CloudTrail not readable, missing cloudtrail:LookupEvents");
    expect(within(row).getByRole("button", { name: /Copy fix/ })).toBeTruthy();
  });

  it("03 history gap: hatched band with its reason on hover and a warning that names the span", async () => {
    serveState("03-history-gap");
    renderPage();
    await summary();
    expect(num("changes")).toBe("35");
    expect(warningText()).toBe(`Data not complete from ${fmtClock(utc(2, 10))} to ${fmtClock(utc(3, 40))}: AWS throttled Prod in eu-west-1.`);
    const band = document.querySelector('[data-band="throttled"]') as HTMLElement;
    expect(band.getAttribute("title")).toContain("Prod eu-west-1: CloudTrail throttled the collector");
    const gapCells = document.querySelectorAll('[role="cell"][data-state="gap"]');
    expect(gapCells.length).toBeGreaterThan(0);
    gapCells.forEach((c) => {
      expect(c.textContent).toBe("");
      expect(c.getAttribute("title")).toContain("CloudTrail throttled the collector");
    });
    expect(document.querySelector("[data-gap-row]")).toBeNull();
  });

  it("04 no changes, complete: the card says so with Accounts n/m and no warning", async () => {
    serveState("04-no-changes");
    renderPage();
    await summary();
    expect(document.querySelector("[data-no-changes]")?.textContent).toBe("No changes in the last 24 h");
    expect(num("accounts")).toBe("3/3");
    expect(num("changes")).toBeNull();
    expect(warningText()).toBeNull();
    expect(document.querySelector('[data-empty="complete"]')).toBeTruthy();
    expect(await screen.findByText(/Nothing differs between/)).toBeTruthy();
  });

  it("05 only AWS-service changes: zero by people and pipelines, N by AWS service", async () => {
    serveState("05-aws-service-only");
    renderPage();
    await summary();
    expect([num("changes"), num("people"), num("pipeline"), num("aws_service")]).toEqual(["14", "0", "0", "14"]);
  });

  it("06 very busy: full numbers, busiest hour, and the list loads more instead of truncating", async () => {
    serveState("06-very-busy");
    renderPage();
    await summary();
    expect(num("changes")).toBe("5,214");
    expect(num("people")).toBe("41");
    expect(rowTotals()).toContain("4,402");
    expect(screen.getByText(/Busiest hour .*: 1,240 changes/)).toBeTruthy();
    expect(screen.getByRole("button", { name: "Load more" })).toBeTruthy();
    expect(document.querySelector("[data-list-footer]")).toBeNull();
  });

  it("07 error burst: one row with count, code, principal, first–last time, and Expand from /groups/{id}", async () => {
    serveState("07-error-burst");
    vi.mocked(api.awsChangesGroup).mockResolvedValue({
      items: [item({ id: "m1", time: utc(11, 30), summary: "Failed attempt: iam:PutRolePolicy", error_code: "AccessDenied" })],
      next_cursor: null,
    });
    renderPage();
    await summary();
    const row = document.querySelector(`[data-item="${BURST.id}"]`) as HTMLElement;
    expect(row.textContent).toContain("Error burst from role reporting-etl");
    expect(row.textContent).toContain("AccessDenied ×214");
    expect(row.textContent).toContain(`${fmtClock(utc(11, 30))}–${fmtClock(utc(11, 42))}`);
    fireEvent.click(within(row).getByRole("button", { name: /Expand/ }));
    expect(await within(row).findByText(/Failed attempt: iam:PutRolePolicy/)).toBeTruthy();
    expect(vi.mocked(api.awsChangesGroup).mock.calls[0][0]).toBe(BURST.id);
  });

  it("08 alarm with no likely cause: the slot says no change matches, nothing is forced, and says where it looked on hover", async () => {
    serveState("08-alarm-no-suspect");
    renderPage();
    await summary();
    expect(likelyText()).toBe("checkout 5xx: no change matches this alarm");
    const slot = document.querySelector('[data-likely-cause="status"]') as HTMLElement;
    expect(slot.getAttribute("title")).toBe(`Looked at changes from ${fmtClock(utc(13, 2))} to ${fmtClock(utc(14, 2))} in Prod (eu-west-1, global)`);
    expect(document.querySelector("[data-likely-tag]")).toBeNull();
    expect(document.querySelector("[data-suspect]")).toBeNull();
    expect(document.querySelector(`[data-alarm-line="${ALARM.id}"]`)).toBeTruthy();
    expect(document.querySelector("[data-alarm-panel]")).toBeNull();
    expect(document.querySelector('[data-item="ev-asg"]')?.textContent).toContain("after the alarm");
  });

  it("09 compare not available: names where history starts and offers that time, no counts", async () => {
    serveState("09-compare-unavailable");
    renderPage();
    const panel = await waitFor(() => {
      const p = document.querySelector('[data-compare="unavailable"]');
      if (!p) throw new Error("not yet");
      return p as HTMLElement;
    });
    expect(panel.textContent).toContain(`Earliest full compare: ${fmtClock(utc(2, 15))}; pick that or a later time.`);
    expect(panel.textContent).not.toContain("Changes are kept longer than snapshots");
    expect(panel.querySelector("[data-oldest-snapshot]")).toBeNull();
    expect(screen.queryByText("added")).toBeNull();
    fireEvent.click(within(panel).getByRole("button", { name: `Use ${fmtClock(utc(2, 15))}` }));
    await waitFor(() => {
      const last = vi.mocked(api.awsChangesCompare).mock.calls.at(-1)?.[0] as CompareQuery;
      expect(last.from).toBe(utc(2, 15));
    });
  });

  it("10 compare with no differences: Nothing differs only when ok, complete and empty", async () => {
    serveState("10-compare-no-diff");
    renderPage();
    expect(await screen.findByText(`Nothing differs between ${fmtClock(at(-6 * 3_600_000))} and now`)).toBeTruthy();
  });

  it("11 account disconnected: one warning with the as-of and disconnect times and Reconnect", async () => {
    serveState("11-account-disconnected");
    renderPage();
    await summary();
    expect(warningText()).toBe(`Data not complete after ${fmtClock(utc(9, 20))}: Staging was disconnected at ${fmtClock(utc(9, 25))}.`);
    const line = document.querySelector("[data-warning-line]") as HTMLElement;
    expect(within(line).getByRole("link", { name: "Reconnect ›" }).getAttribute("href")).toBe("/aws/settings");
    // The 09:14 row is inside the account's coverage, so it carries no "as of" chip.
    expect(document.querySelector('[data-item="ev-staging"] [data-as-of-chip]')).toBeNull();
    const row = openWarning().querySelector('[data-stretch="account_disconnected"]') as HTMLElement;
    expect(row.textContent).toContain("Staging eu-west-1");
    expect(row.textContent).toContain(`${fmtClock(utc(9, 20))}–now`);
    expect(row.querySelector("[data-stretch-reason]")?.textContent).toBe("account disconnected");
    expect(document.querySelector("[data-account-gap]")).toBeNull();
  });

  it("11 account disconnected on an earlier local day: every as-of and disconnect time carries its date, as Settings prints it", async () => {
    const tz = process.env.TZ;
    process.env.TZ = "America/Los_Angeles";
    try {
      // Now is 00:00 Nov 19 in Los Angeles; the account dropped at 23:50 Nov 18 and is known to 23:35.
      const now = "2026-11-19T08:00:42.000Z";
      const asOf = "2026-11-19T07:35:00.000Z";
      const base = states["11-account-disconnected"].view;
      serve({
        ...base,
        window: { since: "2026-11-18T08:00:42.000Z", until: now, settled_until: now },
        accounts: accounts({ [STAGING]: { complete: false, reason: "account_disconnected", as_of: asOf, disconnected_at: "2026-11-19T07:50:37.000Z" } }),
        gaps: [{ account: STAGING, region: "eu-west-1", from: asOf, to: now, reason: "account_disconnected" }],
        items: [item({ id: "ev-late", time: "2026-11-19T07:55:00.000Z", account: STAGING, summary: "Updated staging-api task definition :18 → :19" })],
      });
      renderPage();
      await summary();
      expect(warningText()).toBe("Data not complete after Nov 18 23:35: Staging was disconnected at Nov 18 23:50.");
      expect(document.querySelector('[data-item="ev-late"] [data-as-of-chip]')?.textContent).toBe("Staging, as of Nov 18 23:35");
      expect(document.querySelector('[data-item="ev-late"]')?.textContent).toContain("Nov 18 23:55");
    } finally {
      process.env.TZ = tz;
    }
  });

  it("12 single Region: the Regions dropdown names the one read Region and the not-enabled ones as such", async () => {
    serveState("12-single-region");
    renderPage();
    await summary();
    const button = screen.getByRole("button", { name: "Regions" });
    expect(button.textContent).toBe("1 region");
    fireEvent.click(button);
    const options = screen.getAllByRole("option").map((o) => o.textContent);
    expect(options).toEqual(["1 region", "eu-west-1 — complete", "ap-east-1 — not enabled", "me-central-1 — not enabled"]);
    expect(document.querySelector("[data-not-enabled]")).toBeNull();
  });

  it("13 unknown actor: says Unknown actor and keeps the server's sentence, no guess", async () => {
    serveState("13-unknown-actor");
    renderPage();
    await summary();
    expect(num("changes")).toBe("39");
    expect(num("unknown")).toBe("1");
    const row = document.querySelector('[data-item="ev-unknown"]') as HTMLElement;
    expect(row.textContent).toContain("Put bucket policy on reporting-exports");
    expect(row.textContent).toContain("Unknown actor");
    expect(row.textContent).toContain("s3");
    expect(document.querySelector('[data-row="unknown"]')).toBeTruthy();
  });

  it("14 7 d asked in OSS: 7d stays locked with a quiet Enterprise note, the 24 h view loads", async () => {
    serveState("14-beyond-retention");
    renderPage("/aws/changes?window=7d");
    await summary();
    expect((vi.mocked(api.awsChanges).mock.calls[0][0] as ChangesQuery).window).toBe("24h");
    expect(screen.getByRole("button", { name: "7d, Enterprise" }).getAttribute("aria-disabled")).toBe("true");
    expect(document.querySelector("[data-lock-note]")?.textContent).toContain("part of Enterprise; this page shows the last 24 h");
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("14 a server window_locked answer is a quiet note, not an error", async () => {
    vi.mocked(api.awsChanges).mockRejectedValue(new ApiError(403, "window_locked", { error: "window_locked", tier: "enterprise", max_window: "24h" }));
    vi.mocked(api.awsChangesCompare).mockResolvedValue(usualCompare());
    renderPage();
    await waitFor(() => expect(document.querySelector("[data-lock-note]")).toBeTruthy());
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("licensed limits (168h): 7d is open with no Enterprise mark, and choosing it reads 7 days", async () => {
    const limits = { max_window: "168h", max_compare_age: "168h" };
    serve(usualView({ limits }), usualCompare());
    renderPage();
    await summary();
    const seven = await screen.findByRole("button", { name: "7d" });
    expect(seven.getAttribute("aria-disabled")).toBeNull();
    expect(seven.getAttribute("title")).toBeNull();
    expect(screen.queryByRole("button", { name: "7d, Enterprise" })).toBeNull();
    fireEvent.click(seven);
    await waitFor(() => expect(vi.mocked(api.awsChanges).mock.calls.some(([q]) => (q as ChangesQuery).window === "7d")).toBe(true));
    await waitFor(() => expect(screen.getByRole("button", { name: "7d" }).getAttribute("aria-pressed")).toBe("true"));
    expect(document.querySelector("[data-lock-note]")).toBeNull();
  });

  it("licensed limits: the ?window=7d deep link opens the 7 day view without an Enterprise note", async () => {
    serve(usualView({ limits: { max_window: "168h", max_compare_age: "168h" } }), usualCompare());
    renderPage("/aws/changes?window=7d");
    await waitFor(() => expect(vi.mocked(api.awsChanges).mock.calls.at(-1)?.[0]).toMatchObject({ window: "7d" }));
    await waitFor(() => expect(screen.getByRole("button", { name: "7d" }).getAttribute("aria-pressed")).toBe("true"));
    expect(document.querySelector("[data-lock-note]")).toBeNull();
  });

  it("licensed limits: Compare offers starting points back to max_compare_age", async () => {
    serve(usualView({ limits: { max_window: "168h", max_compare_age: "100h" } }), usualCompare());
    renderPage();
    await summary();
    fireEvent.click(screen.getByRole("button", { name: /Compare from/ }));
    const labels = screen.getAllByRole("option").map((o) => o.textContent ?? "");
    expect(labels.some((l) => l.includes("(2 d ago)"))).toBe(true);
    expect(labels.some((l) => l.includes("(3 d ago)"))).toBe(true);
    expect(labels.some((l) => l.includes("(100 h ago)"))).toBe(true);
    expect(labels.some((l) => l.includes("(7 d ago)"))).toBe(false);
  });

  it("the Compare from list is as wide as its longest option and wraps past that, never cutting off the (2 d ago) suffix", async () => {
    serve(usualView({ limits: { max_window: "168h", max_compare_age: "168h" } }), usualCompare());
    renderPage();
    await summary();
    fireEvent.click(screen.getByRole("button", { name: /Compare from/ }));
    const list = screen.getByRole("listbox", { name: "Compare from" });
    expect(list.className).toContain("w-max");
    expect(list.className).toContain("min-w-full");
    expect(list.className).not.toMatch(/(^|\s)w-full(\s|$)/);
    const twoDays = screen.getAllByRole("option").find((o) => (o.textContent ?? "").includes("(2 d ago)"));
    expect(twoDays).toBeTruthy();
    const label = twoDays?.querySelector("span") as HTMLElement;
    expect(label.className).not.toContain("truncate");
    expect(label.className).toContain("break-words");
  });

  it("an older server without limits falls back to 24 h: 7d locked and Compare stops at 24 h", async () => {
    serve(usualView(), usualCompare());
    renderPage();
    await summary();
    expect(screen.getByRole("button", { name: "7d, Enterprise" }).getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(screen.getByRole("button", { name: /Compare from/ }));
    const labels = screen.getAllByRole("option").map((o) => o.textContent ?? "");
    expect(labels.at(-1)).toContain("(24 h ago)");
    expect(labels.some((l) => l.includes("d ago"))).toBe(false);
  });

  it("OSS limits (24h): the 7d deep link reads 24 h and never sends window=7d", async () => {
    serve(usualView({ limits: { max_window: "24h", max_compare_age: "24h" } }), usualCompare());
    renderPage("/aws/changes?window=7d");
    await waitFor(() => expect(document.querySelector("[data-lock-note]")).toBeTruthy());
    expect(vi.mocked(api.awsChanges).mock.calls.every(([q]) => (q as ChangesQuery).window !== "7d")).toBe(true);
  });

  it("a 403 permission_required is a plain denial: no Retry, no alert, no poll", async () => {
    vi.mocked(api.awsChanges).mockRejectedValue(
      new ApiError(403, "infrastructure:view permission is required", { error: "forbidden", code: "permission_required", permission: "infrastructure:view" }),
    );
    vi.mocked(api.awsChangesCompare).mockResolvedValue(usualCompare());
    renderPage();
    expect(await screen.findByText("You don't have access to AWS Changes")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Retry" })).toBeNull();
    expect(screen.queryByRole("alert")).toBeNull();
    expect(document.body.textContent).not.toContain("infrastructure:view");
    expect(vi.mocked(api.awsChanges)).toHaveBeenCalledTimes(1);
  });

  it("15 late delivery: the unsettled tail is only the dashed outline on the grid, with no label and no warning", async () => {
    serveState("15-late-delivery");
    renderPage();
    await summary();
    expect(num("changes")).toBe("34");
    expect(warningText()).toBeNull();
    expect(document.querySelector("[data-arriving-row]")).toBeNull();
    expect(document.querySelector("[data-arriving-outline]")).toBeTruthy();
    const arriving = document.querySelectorAll('[role="cell"][data-state="arriving"]');
    expect(arriving.length).toBeGreaterThan(0);
    arriving.forEach((c) => expect(c.getAttribute("aria-label")).toMatch(/not settled yet/));
    await compareBox();
    expect(document.body.textContent).not.toMatch(/arriving|may still/i);
  });

  it("16 hidden secret fields: value hidden pill, the fact it changed stays", async () => {
    serveState("16-hidden-secret");
    renderPage();
    await summary();
    const row = document.querySelector('[data-item="ev-secret"]') as HTMLElement;
    expect(row.textContent).toContain("Changed PAYMENTS_API_KEY on Lambda billing-worker");
    expect(row.querySelectorAll("[data-value-hidden]").length).toBeGreaterThan(0);
    await waitFor(() => expect(document.querySelector('[data-compare="ok"] [data-value-hidden]')).toBeTruthy());
    expect(document.body.textContent).not.toContain("alice@example.com");
  });

  it("17 multi-account: account chip on every row and completeness per account in the filter", async () => {
    serveState("17-multi-account");
    renderPage();
    await summary();
    expect(num("changes")).toBe("35");
    expect(warningText()).toBe(`Data not complete from ${fmtClock(utc(2, 10))} to ${fmtClock(utc(3, 40))}: AWS throttled Staging in eu-west-1.`);
    const rows = document.querySelectorAll("[data-item]");
    rows.forEach((r) => expect(r.querySelector("[data-account-chip]")).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: "Accounts" }));
    expect(screen.getByRole("option", { name: /Staging \(559210038466\) — gap/ })).toBeTruthy();
    expect(screen.getByRole("option", { name: /Prod \(204811974821\) — complete/ })).toBeTruthy();
  });
});

describe("Changes page — completeness rules", () => {
  it("H1: everything read with an unsettled tail says No changes, with the failed calls, and no warning", async () => {
    serve(
      usualView({
        window: { since: at(-24 * 3_600_000), until: NOW, settled_until: at(-15 * 60_000) },
        summary: { ...usualView().summary, total: 0, by_actor_class: { pipeline: 0, person: 0, aws_service: 0, unknown: 0 }, people: 0 },
        items: [],
        suspect: { status: "no_alarm" },
        alarms: [],
      }),
    );
    renderPage();
    await summary();
    expect(document.querySelector("[data-no-changes]")?.textContent).toBe("No changes in the last 24 h");
    expect(num("errors")).toBe("214");
    expect(warningText()).toBeNull();
  });

  it("H1: a zero total with a real gap never reads No changes; the warning names the gap", async () => {
    const base = usualView();
    serve({
      ...base,
      complete: false,
      summary: { ...base.summary, total: 0, total_is_lower_bound: true, people: 0, by_actor_class: { pipeline: 0, person: 0, aws_service: 0, unknown: 0 } },
      gaps: [{ account: STAGING, region: "eu-west-1", from: utc(2, 0), to: utc(3, 0), reason: "collector_down" }],
      items: [],
      alarms: [],
      suspect: { status: "no_alarm" },
    });
    renderPage();
    await summary();
    expect(warningText()).toBe(`Data not complete from ${fmtClock(utc(2, 0))} to ${fmtClock(utc(3, 0))}: the collector was not running for Staging in eu-west-1.`);
    expect(document.querySelector("[data-no-changes]")).toBeNull();
    expect(screen.queryByText(/No changes/)).toBeNull();
    expect(num("changes")).toBe("0");
    expect(rowTotals().every((t) => /^\d[\d,]*$/.test(t))).toBe(true);
    expect(document.querySelector('[data-empty="incomplete"]')).toBeTruthy();
  });

  it("H7: a read that hit its cap raises the warning", () => {
    const view = usualView({ summary: { ...usualView().summary, total: 214, total_is_lower_bound: true, truncated: true } });
    expect(warningLine(view, "24h")?.text).toBe("Data not complete: the collector hit its cap, so more changes exist.");
  });

  it("a lower-bound total with no named cause still raises the warning, never No changes", () => {
    const base = usualView();
    const view = usualView({ summary: { ...base.summary, total: 0, total_is_lower_bound: true, people: 0, by_actor_class: { pipeline: 0, person: 0, aws_service: 0, unknown: 0 } } });
    expect(warningLine(view, "1h")?.text).toBe("Data not complete: part of this window is missing.");
  });

  it("H7: compare left-out groups never yield Nothing differs", async () => {
    serve(
      usualView(),
      usualCompare({
        added: [],
        removed: [],
        changed: [],
        complete: false,
        partial: [{ account: STAGING, region: "eu-west-1", service: "autoscaling", history_starts: utc(8, 0) }],
      }),
    );
    renderPage();
    const pop = await openNotCompared();
    expect(screen.queryByText(/Nothing differs/)).toBeNull();
    expect(pop.textContent).toContain(`not available before ${fmtClock(utc(8, 0))}`);
  });

  it("H13: Load more walks the keyset cursor; a group never repeats across pages", async () => {
    const page1 = usualView({ next_cursor: "cur-2" });
    const page2 = usualView({
      items: [STACK_GROUP, item({ id: "ev-old", time: utc(7, 2), summary: "Renewed certificate for CloudFront checkout-cdn" })],
      next_cursor: null,
    });
    vi.mocked(api.awsChanges).mockImplementation(async (q: ChangesQuery = {}) => (q.cursor === "cur-2" ? page2 : page1));
    vi.mocked(api.awsChangesCompare).mockResolvedValue(usualCompare());
    renderPage();
    await summary();
    const more = screen.getByRole("button", { name: "Load more" });
    expect((document.querySelector("[data-list-scroll]") as HTMLElement).contains(more)).toBe(true);
    fireEvent.click(more);
    await screen.findByText("Renewed certificate for CloudFront checkout-cdn");
    const calls = vi.mocked(api.awsChanges).mock.calls.map((c) => (c[0] as ChangesQuery).cursor);
    expect(calls).toEqual([undefined, "cur-2"]);
    expect(document.querySelectorAll(`[data-item="${STACK_GROUP.id}"]`)).toHaveLength(1);
    expect(screen.queryByRole("button", { name: "Load more" })).toBeNull();
    expect(screen.queryByRole("navigation", { name: /page/i })).toBeNull();
  });

  it("routine updates are counts only and hidden until asked for", async () => {
    const routine = item({ id: "routine-ssm", kind: "routine", time: utc(10, 0), summary: "SSM agent UpdateInstanceInformation", count: 1240 });
    serve(usualView({ items: [...usualItems(), routine] }));
    renderPage();
    await summary();
    expect(document.querySelector('[data-item="routine-ssm"]')).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Show 1,240 routine agent updates" }));
    await waitFor(() => expect((vi.mocked(api.awsChanges).mock.calls.at(-1)?.[0] as ChangesQuery).show_routine).toBe(true));
    await waitFor(() => expect(document.querySelector('[data-item="routine-ssm"]')?.textContent).toContain("1,240 routine updates"));
  });

  it("buckets keep the server's count and order across a DST change", async () => {
    serve(usualView({ buckets: buckets() }));
    renderPage();
    await summary();
    const pipeline = document.querySelector('[data-row="pipeline"]') as HTMLElement;
    expect(pipeline.querySelectorAll('[role="cell"]')).toHaveLength(24);
  });
});

describe("Changes page — contract amendments", () => {
  const amend = amendmentStates();
  function serveAmend(name: string) {
    const f = amend[name];
    if (!f) throw new Error(`missing fixture ${name}`);
    serve(f.view, f.compare);
    return f;
  }

  it("resource_unknown: says it can't tell which resource the alarm watches, no likely-cause cell or tag", async () => {
    serveAmend("resource-unknown");
    renderPage();
    await summary();
    expect(likelyText()).toBe("checkout 5xx: can't tell which resource this alarm watches");
    expect(document.querySelector("[data-suspect]")).toBeNull();
    expect(document.querySelector("[data-likely-tag]")).toBeNull();
    expect(screen.queryByText(/no change matches/i)).toBeNull();
  });

  it("too_busy gap: names the event rate against the collector's budget on hover and in the popover", async () => {
    serveAmend("too-busy");
    renderPage();
    await summary();
    expect(warningText()).toBe(`Data not complete from ${fmtClock(utc(9, 0))} to ${fmtClock(utc(10, 0))}: Prod in eu-west-1 had too many events to fetch in time.`);
    const band = document.querySelector('[data-band="too_busy"]') as HTMLElement;
    expect(band.getAttribute("title")).toContain("(~3,880 events/hour, above the collector's budget)");
    expect(openWarning().querySelector('[data-stretch="too_busy"] [data-stretch-reason]')?.textContent).toBe(
      "too many events to fetch in time (~3,880 events/hour, above the collector's budget)",
    );
  });

  it("dropped rows: the warning counts the malformed events skipped and the popover lists them; none when zero", async () => {
    serveAmend("dropped");
    renderPage();
    await summary();
    expect(warningText()).toBe("Data not complete: 3 malformed events skipped.");
    expect(openWarning().querySelector("[data-dropped]")?.textContent).toBe("3 malformed events skipped");
    cleanup();
    serve(usualView());
    renderPage();
    await summary();
    expect(warningText()).toBeNull();
  });

  it("compare unavailable: names the oldest snapshot and links the rest of the changes in the span", async () => {
    const f = serveAmend("compare-unavailable-with-changes");
    renderPage("/aws/changes?account=204811974821");
    const panel = await waitFor(() => {
      const p = document.querySelector('[data-compare="unavailable"]');
      if (!p) throw new Error("not yet");
      return p as HTMLElement;
    });
    expect(panel.querySelector("[data-oldest-snapshot]")?.textContent).toBe(`Oldest snapshot: ${fmtClockOrDay(utc(2, 15), NOW)}`);
    const between = panel.querySelector("[data-changes-between]") as HTMLElement;
    expect(between.querySelectorAll("li")).toHaveLength(20);
    const more = within(between).getByRole("link", { name: /and 37 more/ });
    const href = new URL(more.getAttribute("href") ?? "", "http://x");
    expect(href.pathname).toBe("/aws/changes");
    expect(href.searchParams.get("since")).toBe(f.compare.from);
    expect(href.searchParams.get("until")).toBe(f.compare.to);
    expect(href.searchParams.get("account")).toBe("204811974821");
    expect(href.searchParams.get("window")).toBeNull();
  });

  it("tied rows keep the server's newest-first order and only the named one is tagged Likely cause", async () => {
    serveAmend("tied-suspects");
    renderPage();
    await summary();
    expect(likelyText()).toBe(SUSPECT.summary);
    const ids = [...document.querySelectorAll("[data-item]")].map((e) => e.getAttribute("data-item"));
    expect(ids.indexOf(SUSPECT.id)).toBe(ids.indexOf("ev-deploy-tie") - 1);
    expect(document.querySelectorAll('[data-likely-tag="likely"]')).toHaveLength(1);
    expect(document.querySelector(`[data-item="${SUSPECT.id}"] [data-likely-tag="likely"]`)?.textContent).toBe("Likely cause");
  });
});

describe("Changes page — Amendment 1 fields and the exact span", () => {
  const amend = amendmentStates();
  function serveAmend(name: string) {
    const f = amend[name];
    if (!f) throw new Error(`missing fixture ${name}`);
    serve(f.view, f.compare);
    return f;
  }

  it("suspect.change: the likely-cause slot and the grid cell work when the change is not on the loaded page", async () => {
    serveAmend("suspect-off-page");
    renderPage();
    await summary();
    expect(likelyText()).toBe(SUSPECT.summary);
    expect(document.querySelector(`[data-item="${SUSPECT.id}"]`)).toBeNull();
    const cell = document.querySelector("[data-suspect]") as HTMLElement;
    expect(cell.getAttribute("aria-label")).toBe(`Automation ${fmtClock(utc(13, 0))}: 2, likely cause`);
    fireEvent.click(screen.getByRole("button", { name: "Show ›" }));
    expect(cell.scrollIntoView).toHaveBeenCalled();
    await waitFor(() => expect(document.querySelector("[data-suspect]")?.getAttribute("data-shown")).toBe("true"));
  });

  it("regions[]: no per-Region status line; each Region's state sits in its dropdown option, global and not-enabled last", async () => {
    serveAmend("regions-in-scope");
    renderPage();
    await summary();
    expect(document.querySelector("[data-regions]")).toBeNull();
    expect(document.querySelector("[data-region-row]")).toBeNull();
    const button = screen.getByRole("button", { name: "Regions" });
    expect(button.textContent).toBe("3 regions");
    fireEvent.click(button);
    expect(screen.getAllByRole("option").map((o) => o.textContent)).toEqual([
      "3 regions",
      "eu-west-1 — complete",
      "us-east-1 — partly unknown",
      "Global (IAM, …) — complete",
      "ap-east-1 — not enabled",
    ]);
  });

  it("top_actors shows the top five on their own line; last_change_before sits with No changes", async () => {
    serveAmend("top-actors");
    renderPage();
    await summary();
    const actors = [...document.querySelectorAll("[data-summary-card] [data-top-actor]")].map((e) => e.textContent ?? "");
    expect(actors).toHaveLength(5);
    expect(actors[0]).toBe("GitHub Actions deploy.yml 3,880");
    expect(actors[2]).toBe("a…@example.com 2");
    expect(actors[4]).toBe("Unknown actor 1");
    cleanup();
    serveAmend("empty-with-last-change");
    renderPage();
    await summary();
    expect(document.querySelector("[data-no-changes] p")?.textContent).toBe("No changes in the last 24 h");
    expect(document.querySelector("[data-last-change-before]")?.textContent).toBe("Last change before this window: 3 d ago");
  });

  it("event_name: shown raw in the expanded item and searchable through q", async () => {
    serveAmend("event-name");
    renderPage();
    await summary();
    const row = document.querySelector('[data-item="ev-bucket-policy"]') as HTMLElement;
    expect(row.querySelector("[data-event-name]")).toBeNull();
    fireEvent.click(within(row).getByRole("button", { name: /Details/ }));
    expect(row.querySelector("[data-event-name]")?.textContent).toBe("s3:PutBucketPolicy");
    const box = screen.getByRole("searchbox", { name: "Search changes" });
    fireEvent.change(box, { target: { value: "s3:PutBucketPolicy" } });
    fireEvent.submit(box.closest("form") as HTMLFormElement);
    await waitFor(() => expect((vi.mocked(api.awsChanges).mock.calls.at(-1)?.[0] as ChangesQuery).q).toBe("s3:PutBucketPolicy"));
  });

  it("groups expand from /groups/{id} and Show more walks its next_cursor", async () => {
    serve(usualView());
    vi.mocked(api.awsChangesGroup)
      .mockResolvedValueOnce({ items: [item({ id: "g1", time: utc(9, 10), summary: "Updated subnet-a route table" })], next_cursor: "g-page-2" })
      .mockResolvedValueOnce({ items: [item({ id: "g2", time: utc(9, 12), summary: "Updated subnet-b route table" })], next_cursor: null });
    renderPage();
    await summary();
    const row = document.querySelector(`[data-item="${STACK_GROUP.id}"]`) as HTMLElement;
    expect("group_cursor" in STACK_GROUP).toBe(false);
    fireEvent.click(within(row).getByRole("button", { name: /Expand/ }));
    expect(await within(row).findByText(/subnet-a route table/)).toBeTruthy();
    fireEvent.click(within(row).getByRole("button", { name: "Show more in this group" }));
    expect(await within(row).findByText(/subnet-b route table/)).toBeTruthy();
    const calls = vi.mocked(api.awsChangesGroup).mock.calls;
    expect(calls[0][0]).toBe(STACK_GROUP.id);
    expect(calls[1]).toEqual([STACK_GROUP.id, "g-page-2"]);
    expect(within(row).queryByRole("button", { name: "Show more in this group" })).toBeNull();
  });

  it("since: the page reads exactly [since, until] and words the span, not a preset window", async () => {
    const since = utc(8, 10);
    serve(usualView({ window: { since, until: NOW, settled_until: NOW } }));
    renderPage(`/aws/changes?since=${encodeURIComponent(since)}&until=${encodeURIComponent(NOW)}`);
    await summary();
    const q = vi.mocked(api.awsChanges).mock.calls[0][0] as ChangesQuery;
    expect(q).toMatchObject({ since, until: NOW });
    expect(changesQueryString(q)).not.toContain("window=");
    expect(document.querySelector("[data-span]")?.textContent).toContain(fmtClock(since));
    expect(screen.getByRole("button", { name: "24h" }).getAttribute("aria-pressed")).toBe("false");
  });
});

describe("Changes page — docs-pass fixes", () => {
  it("compare partial with no history yet says so instead of a start time", async () => {
    serve(
      usualView(),
      usualCompare({
        complete: false,
        partial: [{ account: STAGING, region: "eu-west-1", service: "ec2", history_starts: null }],
      }),
    );
    renderPage();
    const box = await openNotCompared();
    expect(box.textContent).toContain("Staging eu-west-1 EC2: no history yet");
    expect(box.textContent).not.toContain("not available before");
  });

  const amend = amendmentStates();
  function serveAmend(name: string) {
    const f = amend[name];
    if (!f) throw new Error(`missing fixture ${name}`);
    serve(f.view, f.compare);
    return f;
  }

  it("late arrivals and clock skew sit in the summary card; small skew and zero late stay quiet", async () => {
    serveAmend("sync-health");
    renderPage();
    await summary();
    expect(document.querySelector('[data-summary-card] [data-sync-note="late"]')?.textContent).toBe(
      "12 late events caught; raise connectors.aws.collect.changes.overlap if this keeps growing",
    );
    expect(document.querySelector('[data-sync-note="skew"]')?.textContent).toBe("This server's clock is 75 s off AWS time");
    cleanup();
    serve(usualView({ sync: { ...usualView().sync, clock_skew_seconds: 59 } }));
    renderPage();
    await summary();
    expect(document.querySelector("[data-sync-notes]")).toBeNull();
  });

  it("the burst caption names no fixed minute gap", async () => {
    serveState("07-error-burst");
    renderPage();
    await summary();
    const row = document.querySelector(`[data-item="${BURST.id}"]`) as HTMLElement;
    expect(row.textContent).toContain("Rule: same principal, code and service, close together in time");
    expect(row.textContent).not.toMatch(/\d+ min/);
  });

  it("tied candidates read Equally likely on their own rows; the named one reads Likely cause; others carry no tag", async () => {
    serveAmend("candidates");
    renderPage();
    await summary();
    expect(document.querySelector(`[data-item="${SUSPECT.id}"] [data-likely-tag]`)?.textContent).toBe("Likely cause");
    expect(document.querySelector('[data-item="ev-deploy-tie"] [data-likely-tag]')?.textContent).toBe("Equally likely");
    expect(document.querySelector('[data-item="ev-lambda"] [data-likely-tag]')).toBeNull();
    expect(document.querySelector("[data-candidates]")).toBeNull();
    cleanup();
    serve(usualView());
    renderPage();
    await summary();
    expect(screen.queryByText("Equally likely")).toBeNull();
  });

  it("status none says no change matches this alarm once, in the likely-cause slot", async () => {
    serveState("08-alarm-no-suspect");
    renderPage();
    await summary();
    expect(screen.getAllByText(/no change matches this alarm/i)).toHaveLength(1);
    expect(document.querySelector('[data-summary-card] [data-likely-cause="status"]')?.textContent).toContain("no change matches this alarm");
  });

  it("incomplete and start_unknown statuses say there is not enough data to tell, never an empty slot", async () => {
    serve(usualView({ suspect: { status: "incomplete", alarm_id: ALARM.id } }));
    renderPage();
    await summary();
    expect(likelyText()).toBe("checkout 5xx: not enough data to tell");
    cleanup();
    serve(usualView({ suspect: { status: "start_unknown", alarm_id: ALARM.id } }));
    renderPage();
    await summary();
    expect(likelyText()).toBe("checkout 5xx: start time unknown, so not enough data to tell");
    cleanup();
    serve(usualView({ alarms: [], suspect: { status: "no_alarm" } }));
    renderPage();
    await summary();
    expect(document.querySelector("[data-likely-cause]")).toBeNull();
  });

  it("compare history start carries the date when it is not today", async () => {
    serveAmend("compare-history-yesterday");
    renderPage();
    const panel = await waitFor(() => {
      const p = document.querySelector('[data-compare="unavailable"]');
      if (!p) throw new Error("not yet");
      return p as HTMLElement;
    });
    const start = at(-26 * 3_600_000); // 26 h back is a different calendar day in every time zone
    expect(panel.textContent).toContain(`Earliest full compare: ${fmtDayClock(start)}; pick that or a later time.`);
    expect(within(panel).getByRole("button", { name: `Use ${fmtDayClock(start)}` })).toBeTruthy();
  });
});

describe("Changes page — browser-pass repairs", () => {
  const bp = browserPassStates();
  function serveBp(name: string) {
    const f = bp[name];
    if (!f) throw new Error(`missing fixture ${name}`);
    serve(f.view, f.compare);
    return f;
  }

  it("not-compared groups name their reason; Use HH:MM only when the compare is not already full", async () => {
    serveBp("compare-partial-reason");
    renderPage();
    const box = await openNotCompared();
    const start = at(-4 * 3_600_000);
    expect(document.querySelector("[data-not-compared]")?.textContent).toBe("1 group not compared ▾");
    expect(box.textContent).toContain(`Staging eu-west-1 EC2: CloudTrail throttled the collector; not available before ${fmtClock(start)}`);
    expect(within(box).getByRole("button", { name: `Use ${fmtClock(start)} for a full compare` })).toBeTruthy();
    cleanup();

    serveBp("compare-full-with-start");
    renderPage();
    expect(await screen.findByText(/^Added$/)).toBeTruthy();
    expect(screen.queryByRole("button", { name: /^Use / })).toBeNull();
    expect(document.querySelector("[data-not-compared]")).toBeNull();
  });

  it("partial buckets keep their plain counts, lightly hatched, name the gap accounts on hover and keep the likely-cause cell", async () => {
    serveBp("partial-buckets");
    renderPage();
    await summary();
    const pipeline = document.querySelector('[data-row="pipeline"]') as HTMLElement;
    const cells = [...pipeline.querySelectorAll('[role="cell"]')] as HTMLElement[];
    expect(cells.every((c) => c.getAttribute("data-state") === "partial")).toBe(true);
    const busy = cells.find((c) => c.textContent === "2") as HTMLElement;
    expect(busy).toBeTruthy();
    expect(cells.some((c) => (c.textContent ?? "").includes("+"))).toBe(false);
    expect(busy.getAttribute("title")).toBe("Unknown in Staging");
    expect(busy.getAttribute("aria-label")).toContain("2, unknown in Staging");
    expect(document.querySelector("[data-suspect]")?.getAttribute("data-state")).toBe("partial");
    expect(document.querySelectorAll('[role="cell"][data-state="gap"]')).toHaveLength(0);
    expect(document.querySelector("[data-partial-note]")).toBeNull();
    expect(warningText()).toBe("Data not complete: AWS throttled Staging.");
  });

  it("a gap over the whole window names its cause; the band and popover carry both days", async () => {
    serveBp("gap-whole-day");
    renderPage();
    await summary();
    const [from, to] = [fmtDayClock(at(-24 * 3_600_000)), fmtDayClock(NOW)];
    expect(warningText()).toBe("Data not complete: AWS throttled Prod in eu-west-1.");
    expect(document.querySelector('[data-band="throttled"]')?.getAttribute("title")).toContain(`Prod eu-west-1: CloudTrail throttled the collector ${from}–${to}`);
    expect(openWarning().querySelector('[data-stretch="throttled"]')?.textContent).toContain(`${from}–now`);
    fireEvent.keyDown(document, { key: "Escape" });
    fireEvent.click(screen.getByRole("button", { name: "Accounts" }));
    expect(screen.getByRole("option", { name: "Prod (204811974821) — CloudTrail throttled the collector" })).toBeTruthy();
  });

  it("the hatch never sets the text colour", () => {
    expect(HATCH).not.toMatch(/\btext-/);
  });

  it("the Accounts filter is wide enough to read each account's state in full", async () => {
    serveState("17-multi-account");
    renderPage();
    await summary();
    const button = screen.getByRole("button", { name: "Accounts" });
    expect(button.parentElement?.className).toContain("w-[26rem]");
    fireEvent.click(button);
    expect(screen.getByRole("option", { name: /Staging \(559210038466\) — gap/ }).textContent).toMatch(/— gap .+–.+$/);
  });

  it("unsettled cells show the counts the list shows, and the likely-cause cell outlines inside the tail", async () => {
    serveBp("arriving-suspect");
    renderPage();
    await summary();
    const tail = document.querySelector('[data-row="aws_service"] [role="cell"][data-state="arriving"]') as HTMLElement;
    expect(tail.textContent).toBe("1");
    expect(tail.getAttribute("data-suspect")).toBe("true");
    expect(tail.getAttribute("aria-label")).toContain("1, not settled yet, likely cause");
  });

  it("polls while open: the first read clears on its own and expanded rows stay open", async () => {
    expect(pollInterval(bp["first-read"].view)).toBe(10_000);
    expect(pollInterval(bp["arriving-suspect"].view)).toBe(10_000);
    expect(pollInterval(usualView())).toBe(30_000);
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      vi.mocked(api.awsChanges).mockResolvedValueOnce(bp["first-read"].view).mockResolvedValue(usualView());
      vi.mocked(api.awsChangesCompare).mockResolvedValue(usualCompare());
      renderPage();
      await summary();
      expect(warningText()).toBe("Reading the last 24 h…");
      const row = document.querySelector('[data-item="ev-lambda"]') as HTMLElement;
      fireEvent.click(within(row).getByRole("button", { name: /Details/ }));
      await vi.advanceTimersByTimeAsync(10_500);
      await waitFor(() => expect(warningText()).toBeNull());
      expect(num("changes")).toBe("37");
      expect(document.querySelector('[data-item="ev-lambda"] [data-item-detail]')).toBeTruthy();
    } finally {
      vi.useRealTimers();
    }
  });

  it("copy: plain zero counts, failed calls in red, one alarms chip with each alarm and the history start in its popover", async () => {
    serveBp("errors-only");
    renderPage();
    await summary();
    expect(num("changes")).toBe("0");
    expect(num("errors")).toBe("36");
    expect(document.querySelector('[data-number="errors"] dd')?.className).toContain("text-sev-critical");
    expect(rowTotals()).toContain("0");
    expect(document.querySelector("[data-list-footer]")).toBeNull();
    const chip = document.querySelector("[data-alarms-chip]") as HTMLElement;
    expect(chip.textContent).toBe("2 alarms ▾");
    expect(document.querySelector("[data-alarm-label]")).toBeNull();
    fireEvent.click(chip);
    const pop = document.querySelector("[data-alarms-popover]") as HTMLElement;
    const entries = [...pop.querySelectorAll("[data-alarm-entry]")].map((e) => e.textContent);
    expect(entries).toEqual([`checkout 5xxfrom ${fmtClock(ALARM.start)}Prod eu-west-1`, `checkout 5xxfrom ${fmtClock(ALARM.start)}Prod us-east-1`]);
    expect(pop.querySelector("[data-alarm-history]")?.textContent).toBe(`Alarm history starts ${fmtClock(utc(1, 0))}`);
    const field = document.querySelector('[data-item="ev-arn"] [data-field-line]') as HTMLElement;
    expect(field.children[0].className).toContain("shrink-0");
    expect(field.children[1].className).toContain("break-all");
    cleanup();

    serveBp("quiet-no-alarm");
    renderPage();
    await summary();
    expect(document.querySelector("[data-alarms-chip]")).toBeNull();
    expect(screen.queryByText(/Alarm history starts/)).toBeNull();
  });

  it("compare look-alikes carry their id_hint and Region", async () => {
    serveBp("compare-look-alikes");
    renderPage();
    await waitFor(() => {
      if (document.querySelectorAll("[data-compare-line]").length < 2) throw new Error("not yet");
    });
    const lines = [...document.querySelectorAll("[data-compare-line]")].map((e) => e.textContent ?? "");
    expect(lines[0]).toContain("versus-web");
    expect(lines[0]).toContain("i-0a1b");
    expect(lines[1]).toContain("i-9f8e");
    expect(lines[0]).toContain("eu-west-1");
    expect(lines[0]).toContain("Prod");
  });
});

describe("Changes page — docs-review fixes", () => {
  it("there is no Read time anywhere on the page", async () => {
    serve(usualView());
    renderPage();
    await summary();
    await compareBox();
    expect(document.querySelector("[data-read-at]")).toBeNull();
    expect(document.body.textContent).not.toMatch(/Read \d/);
  });

  it("clock skew shows from 60 s either way, not below", () => {
    expect(clockSkewText(59)).toBeNull();
    expect(clockSkewText(-59)).toBeNull();
    expect(clockSkewText(60)).toBe("This server's clock is 60 s off AWS time");
    expect(clockSkewText(-90)).toBe("This server's clock is 90 s off AWS time");
  });

  it("the late-events note names the full config key", () => {
    expect(lateArrivalsText(1)).toBe("1 late event caught; raise connectors.aws.collect.changes.overlap if this keeps growing");
    expect(lateArrivalsText(0)).toBeNull();
  });
});

describe("Changes page — expired cursors", () => {
  it("Load more or a group expansion with an expired cursor quietly reloads the first page with a note", async () => {
    const busy = states["06-very-busy"];
    const expired = new ApiError(400, "cursor_expired", { error: "cursor_expired" });
    vi.mocked(api.awsChanges).mockImplementation(async (q: ChangesQuery) => {
      if (q.cursor) throw expired;
      return busy.view;
    });
    vi.mocked(api.awsChangesCompare).mockResolvedValue(busy.compare);
    renderPage();
    await summary();
    fireEvent.click(screen.getByRole("button", { name: "Load more" }));
    expect(await screen.findByText("The list moved on; showing the latest changes.")).toBeTruthy();
    await waitFor(() => expect((vi.mocked(api.awsChanges).mock.calls.at(-1)?.[0] as ChangesQuery).cursor).toBeUndefined());
    expect(screen.queryByText("Couldn't load changes")).toBeNull();
    expect(num("changes")).toBe("5,214");
    cleanup();

    vi.mocked(api.awsChanges).mockReset();
    serve(usualView());
    vi.mocked(api.awsChangesGroup).mockRejectedValue(expired);
    renderPage();
    await summary();
    const callsBefore = vi.mocked(api.awsChanges).mock.calls.length;
    const row = document.querySelector(`[data-item="${STACK_GROUP.id}"]`) as HTMLElement;
    fireEvent.click(within(row).getByRole("button", { name: /Expand/ }));
    expect(await screen.findByText("The list moved on; showing the latest changes.")).toBeTruthy();
    await waitFor(() => expect(vi.mocked(api.awsChanges).mock.calls.length).toBe(callsBefore + 1));
    expect(screen.queryByText("Couldn't load this group.")).toBeNull();
  });
});

describe("Changes page — expired cursors while polling", () => {
  const expired = new ApiError(400, "cursor_expired", { error: "cursor_expired" });
  const page2 = () => ({ ...states["06-very-busy"].view, items: [item({ id: "p2-1", time: utc(3, 0), summary: "Older change on page two" })], next_cursor: null });

  async function openTwoPages() {
    let expire = false;
    vi.mocked(api.awsChanges).mockImplementation(async (q: ChangesQuery) => {
      if (q.cursor) {
        if (expire) throw expired;
        return page2();
      }
      return states["06-very-busy"].view;
    });
    vi.mocked(api.awsChangesCompare).mockResolvedValue(usualCompare());
    renderPage();
    await summary();
    fireEvent.click(screen.getByRole("button", { name: "Load more" }));
    await waitFor(() => expect(document.querySelector('[data-item="p2-1"]')).toBeTruthy());
    return () => {
      expire = true;
    };
  }

  it("a poll that refetches an expired later page reloads the first page quietly, never the retry screen", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      const expireNow = await openTwoPages();
      expireNow();
      await vi.advanceTimersByTimeAsync(30_500);
      expect(await screen.findByText("The list moved on; showing the latest changes.")).toBeTruthy();
      expect(screen.queryByText("Couldn't load changes")).toBeNull();
      await waitFor(() => expect(document.querySelector('[data-item="p2-1"]')).toBeNull());
      expect(num("changes")).toBe("5,214");
    } finally {
      vi.useRealTimers();
    }
  });

  it("the moved-on note clears after a later successful read about 60 s on, and when the window changes", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      const expireNow = await openTwoPages();
      expireNow();
      await vi.advanceTimersByTimeAsync(30_500);
      expect(await screen.findByText("The list moved on; showing the latest changes.")).toBeTruthy();
      await vi.advanceTimersByTimeAsync(30_500);
      expect(screen.queryByText("The list moved on; showing the latest changes.")).toBeTruthy();
      await vi.advanceTimersByTimeAsync(31_000);
      await waitFor(() => expect(screen.queryByText("The list moved on; showing the latest changes.")).toBeNull());
    } finally {
      vi.useRealTimers();
    }
    cleanup();

    vi.mocked(api.awsChanges).mockImplementation(async (q: ChangesQuery) => {
      if (q.cursor) throw expired;
      return states["06-very-busy"].view;
    });
    renderPage();
    await summary();
    fireEvent.click(screen.getByRole("button", { name: "Load more" }));
    expect(await screen.findByText("The list moved on; showing the latest changes.")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "1h" }));
    await waitFor(() => expect((vi.mocked(api.awsChanges).mock.calls.at(-1)?.[0] as ChangesQuery).window).toBe("1h"));
    await summary();
    expect(screen.queryByText("The list moved on; showing the latest changes.")).toBeNull();
  });
});

describe("Changes page — second browser pass", () => {
  it("compare names services Settings knows it can't read and says Nothing differs in the compared services", async () => {
    serve(
      usualView(),
      usualCompare({
        complete: true,
        added: [],
        changed: [],
        removed: [],
        partial: [
          { account: STAGING, region: "global", service: "aws-health", history_starts: null, reason: "not_on_plan", excluded: true },
          { account: STAGING, region: "us-east-1", service: "elasticache", history_starts: null, reason: "service_denied", excluded: true },
          { account: STAGING, region: "eu-west-1", service: "elasticache", history_starts: null, reason: "service_denied", excluded: true },
        ],
      }),
    );
    renderPage();
    const box = await compareBox();
    expect(box.getAttribute("data-compare")).toBe("nothing");
    expect(box.textContent).toContain("Nothing differs in the compared services");
    expect(box.querySelector("[data-not-compared]")?.textContent).toBe("2 groups not compared ▾");
    const pop = await openNotCompared();
    const lines = [...pop.querySelectorAll("[data-gap-text]")].map((e) => e.textContent);
    expect(lines).toEqual(["Staging AWS Health: not on this support plan", "Staging ElastiCache: not readable (permission)"]);
    expect(box.textContent).not.toMatch(/not checked yet|no history yet/);
    expect(box.textContent).not.toContain("+");
  });

  it("compare exclusions drop the account name when only one account is in scope", () => {
    const one = usualView({ accounts: [{ account: STAGING, name: "Staging", complete: true, reason: null, as_of: null }] });
    const lines = partialLines(
      {
        partial: [
          { account: STAGING, region: "us-east-1", service: "elasticache", history_starts: null, reason: "service_denied", excluded: true },
          { account: STAGING, region: "global", service: "aws-health", history_starts: null, reason: "not_on_plan", excluded: true },
          { account: STAGING, region: "eu-west-1", service: "ec2", history_starts: null, reason: "not_checked" },
        ],
      },
      one,
    ).map((l) => l.text);
    expect(lines).toEqual([
      "ElastiCache: not readable (permission)",
      "AWS Health: not on this support plan",
      "Staging eu-west-1 EC2: not checked yet; no history yet",
    ]);
  });

  it("compare not available names the earliest full compare and no second oldest-snapshot time", async () => {
    serve(
      usualView(),
      usualCompare({ status: "unavailable", from: utc(1, 0), history_starts: utc(2, 15), snapshots: { oldest: utc(2, 10), count: 3 } }),
    );
    renderPage();
    const box = await compareBox();
    expect(box.textContent).toContain(`Earliest full compare: ${fmtClock(utc(2, 15))}; pick that or a later time.`);
    expect(box.textContent).not.toContain("History starts");
    expect(box.querySelector("[data-oldest-snapshot]")).toBeNull();
  });

  it("compare ID hints drop what the line already prints, and added and removed lines both name the account", async () => {
    expect(idHintText({ resource_arn: "a", type: "stack", name: "versus-web", account: PROD, region: "eu-west-1", id_hint: `${PROD} eu-west-1 versus-web` })).toBe("");
    expect(idHintText({ resource_arn: "a", type: "instance", name: "versus-web", account: PROD, region: "eu-west-1", id_hint: `${PROD} eu-west-1 i-0a1b` })).toBe("i-0a1b");
    serve(
      usualView({ accounts: [{ account: PROD, name: "Prod", complete: true, reason: null, as_of: null }] }),
      usualCompare({
        changed: [],
        added: [{ resource_arn: "arn:s-a", type: "stack", name: "versus-web", account: PROD, region: "eu-west-1", id_hint: `${PROD} eu-west-1 versus-web` }],
        removed: [{ resource_arn: "arn:s-b", type: "stack", name: "versus-web", account: PROD, region: "us-east-1", id_hint: `${PROD} us-east-1 versus-web` }],
      }),
    );
    renderPage();
    await waitFor(() => {
      if (document.querySelectorAll("[data-compare-line]").length < 2) throw new Error("not yet");
    });
    const lines = [...document.querySelectorAll("[data-compare-line]")];
    expect(lines.map((l) => l.querySelector("[data-id-hint]"))).toEqual([null, null]);
    expect(lines[0].textContent).toBe("Added stack versus-web eu-west-1 · Prod");
    expect(lines[1].textContent).toBe("Removed stack versus-web us-east-1 · Prod");
  });

  it("an account disconnected in Settings tells one story: disconnected, with Reconnect; only rows past its coverage carry the as-of chip", async () => {
    serve(
      usualView({
        complete: false,
        accounts: accounts3({ complete: false, reason: "account_disconnected", as_of: utc(9, 20), disconnected_at: utc(9, 25) }),
        gaps: [{ account: STAGING, region: "eu-west-1", from: utc(9, 20), to: NOW, reason: "assume_role_failed" }],
        items: [
          ...usualItems(),
          item({ id: "in-coverage", time: utc(9, 14), account: STAGING, summary: "Inside coverage" }),
          item({ id: "past-coverage", time: utc(9, 22), account: STAGING, summary: "Past coverage" }),
        ],
      }),
    );
    renderPage();
    await summary();
    expect(warningText()).toBe(`Data not complete after ${fmtClock(utc(9, 20))}: Staging was disconnected at ${fmtClock(utc(9, 25))}.`);
    const pop = openWarning();
    expect(pop.textContent).not.toContain("could not assume the reader role");
    expect(within(pop).getByRole("link", { name: "Reconnect ›" }).getAttribute("href")).toBe("/aws/settings");
    expect(document.querySelector('[data-item="in-coverage"] [data-as-of-chip]')).toBeNull();
    expect(document.querySelector('[data-item="past-coverage"] [data-as-of-chip]')?.textContent).toBe(`Staging, as of ${fmtClock(utc(9, 20))}`);
  });

  it("Reconnect sits in the summary card's warning line, never inside the sideways-scrolling grid", async () => {
    serve(
      usualView({
        complete: false,
        accounts: accounts3({ complete: false, reason: "account_disconnected", as_of: utc(9, 20), disconnected_at: utc(9, 25) }),
        gaps: [{ account: STAGING, region: "eu-west-1", from: utc(9, 20), to: NOW, reason: "assume_role_failed" }],
      }),
    );
    renderPage();
    await summary();
    const scroll = document.querySelector("[data-grid-scroll]") as HTMLElement;
    const links = [...document.querySelectorAll("[data-reconnect]")];
    expect(links).toHaveLength(1);
    expect(scroll.contains(links[0])).toBe(false);
    expect((document.querySelector("[data-warning-line]") as HTMLElement).contains(links[0])).toBe(true);
    expect(document.querySelector("[data-reconnect-outside]")).toBeNull();
    expect(document.querySelector("[data-account-gap]")).toBeNull();
  });

  it("the Accounts filter holds to the screen width below sm and wraps its options", async () => {
    serveState("17-multi-account");
    renderPage();
    await summary();
    const button = screen.getByRole("button", { name: "Accounts" });
    const control = button.parentElement as HTMLElement;
    expect(control.className).toContain("max-w-full");
    expect(control.className).toContain("min-w-0");
    expect(control.className).toContain("sm:w-[26rem]");
    expect(control.className).not.toMatch(/(^|\s)w-\[26rem\]/);
    expect(document.querySelector("[data-accounts-filter]")?.className).toContain("max-w-full");
    fireEvent.click(button);
    const option = screen.getByRole("option", { name: /Staging \(559210038466\)/ });
    expect(option.querySelector("span")?.className).toContain("break-words");
    expect(option.querySelector("span")?.className).not.toContain("truncate");
  });

  it("routine rows say 1 routine update in the singular", async () => {
    expect(routineText(1)).toBe("1 routine update");
    expect(routineText(24)).toBe("24 routine updates");
    const routine = item({ id: "routine-one", kind: "routine", time: utc(10, 0), summary: "Routine ecs:SubmitTaskStateChange events", count: 1 });
    serve(usualView({ items: [...usualItems(), routine] }));
    renderPage("/aws/changes?routine=1");
    await summary();
    await waitFor(() => expect(document.querySelector('[data-item="routine-one"]')?.textContent).toContain("1 routine update: Routine"));
  });

  it("rows name their Region only when the scope reads more than one", async () => {
    const twoRegions = usualView({
      regions: [
        { region: "eu-west-1", state: "selected", complete: true },
        { region: "us-east-1", state: "selected", complete: true },
        { region: "global", state: "global", complete: true },
      ],
    });
    serve(twoRegions);
    renderPage();
    await summary();
    expect(document.querySelector(`[data-item="${SUSPECT.id}"] [data-region-chip]`)?.textContent).toBe(SUSPECT.region);
    cleanup();
    serve(usualView());
    renderPage();
    await summary();
    expect(document.querySelector("[data-region-chip]")).toBeNull();
  });

  it("the warning names each account once, merging its gaps across Regions, and counts the other causes", () => {
    const view = usualView({
      complete: false,
      gaps: [
        { account: PROD, region: "eu-west-1", from: utc(2, 26), to: utc(12, 1), reason: "too_busy" },
        { account: PROD, region: "us-east-1", from: utc(2, 26), to: utc(12, 2), reason: "too_busy" },
        { account: STAGING, region: "eu-west-1", from: utc(2, 26), to: utc(12, 3), reason: "too_busy" },
        { account: STAGING, region: "us-east-1", from: utc(2, 26), to: utc(12, 3), reason: "too_busy" },
      ],
    });
    const span = fmtSpanOrDay(utc(2, 26), utc(12, 2), view.window.until, " to ");
    expect(warningLine(view, "24h")?.text).toBe(`Data not complete from ${span}: Prod in eu-west-1 and us-east-1 had too many events to fetch in time, and 1 more.`);
  });

  it("Top actors keep the server's one entry per actor; a label that still repeats names its account", async () => {
    const top = [
      { actor: "github-actions-deploy", actor_class: "pipeline" as const, count: 4, account: PROD },
      { actor: "Admin (SSO)", actor_class: "person" as const, count: 2, account: PROD },
      { actor: "terraform-apply", actor_class: "pipeline" as const, count: 2 },
      { actor: "github-actions-deploy", actor_class: "pipeline" as const, count: 2, account: STAGING },
      { actor: "Admin (SSO)", actor_class: "person" as const, count: 1, account: STAGING },
    ];
    serve(usualView({ summary: { ...usualView().summary, top_actors: top } }));
    renderPage();
    await summary();
    const shown = [...document.querySelectorAll("[data-top-actor]")].map((e) => e.textContent);
    expect(shown).toEqual(["github-actions-deploy 6", "Admin (SSO) 3", "terraform-apply 2"]);
    const lookAlike = topActorLabels(
      usualView({
        summary: {
          ...usualView().summary,
          top_actors: [
            { actor: "alice@example.com", actor_class: "person", count: 3, account: PROD },
            { actor: "anna@example.com", actor_class: "person", count: 2, account: STAGING },
          ],
        },
      }),
    ).map((a) => a.label);
    expect(lookAlike).toEqual(["a…@example.com · Prod", "a…@example.com · Staging"]);
  });
});

function accounts3(staging: Partial<ChangesView["accounts"][number]>): ChangesView["accounts"] {
  return usualView().accounts.map((a) => (a.account === STAGING ? { ...a, ...staging } : a));
}

// The Changes page prints every past instant against one reference day: the
// local day of the window end. Run in Los Angeles just after local midnight,
// so "yesterday" in local time is still "today" in UTC.
function inZone<T>(zone: string, fn: () => T): T {
  const tz = process.env.TZ;
  process.env.TZ = zone;
  try {
    return fn();
  } finally {
    process.env.TZ = tz;
  }
}

async function inZoneAsync(zone: string, fn: () => Promise<void>): Promise<void> {
  const tz = process.env.TZ;
  process.env.TZ = zone;
  try {
    await fn();
  } finally {
    process.env.TZ = tz;
  }
}

describe("Changes page — one reference day for every time", () => {
  // 01:10 Nov 19 in Los Angeles.
  const UNTIL = "2026-11-19T09:10:00.000Z";
  // 01:10 to 17:57 Nov 18 in Los Angeles: wholly on the previous local day.
  const GAP = { from: "2026-11-18T09:10:00.000Z", to: "2026-11-19T01:57:00.000Z" };

  it.each([
    { name: "instant on the reference day", got: () => fmtClockOrDay("2026-11-19T08:40:00.000Z", UNTIL), want: "00:40" },
    { name: "instant on the earlier day", got: () => fmtClockOrDay("2026-11-19T07:30:00.000Z", UNTIL), want: "Nov 18 23:30" },
    { name: "span on the reference day", got: () => fmtSpanOrDay("2026-11-19T08:10:00.000Z", "2026-11-19T09:00:00.000Z", UNTIL, " to "), want: "00:10 to 01:00" },
    { name: "span crossing local midnight", got: () => fmtSpanOrDay("2026-11-19T07:30:00.000Z", "2026-11-19T08:00:00.000Z", UNTIL, " to "), want: "Nov 18 23:30 to Nov 19 00:00" },
    { name: "span wholly on the earlier day", got: () => fmtSpanOrDay(GAP.from, GAP.to, UNTIL, " to "), want: "Nov 18 01:10 to 17:57" },
    { name: "span joined with the default dash", got: () => fmtSpanOrDay(GAP.from, GAP.to, UNTIL), want: "Nov 18 01:10–17:57" },
    { name: "span with no end", got: () => fmtSpanOrDay(GAP.from, null, UNTIL), want: "Nov 18 01:10" },
  ])("helpers in America/Los_Angeles: $name", ({ got, want }) => {
    expect(inZone("America/Los_Angeles", got)).toBe(want);
  });

  function serveEarlierDayGap() {
    serve(
      usualView({
        window: { since: "2026-11-18T09:10:00.000Z", until: UNTIL, settled_until: UNTIL },
        complete: false,
        gaps: [{ account: PROD, region: "us-east-1", from: GAP.from, to: GAP.to, reason: "too_busy" }],
      }),
      usualCompare({
        status: "unavailable",
        from: "2026-11-19T08:40:00.000Z",
        history_starts: GAP.to,
        complete: false,
        added: [],
        removed: [],
        changed: [],
        changes_between: [
          item({ id: "cb-today", time: "2026-11-19T08:50:00.000Z", summary: "Updated prod-api task definition" }),
          item({ id: "cb-yesterday", time: "2026-11-19T07:59:00.000Z", summary: "Updated prod-web task definition" }),
        ],
        changes_between_total: 2,
      }),
    );
  }

  async function unavailablePanel(): Promise<HTMLElement> {
    return waitFor(() => {
      const p = document.querySelector('[data-compare="unavailable"]');
      if (!p) throw new Error("not yet");
      return p as HTMLElement;
    });
  }

  it("a gap wholly on the previous local day is dated in the warning and the band; same-day compare times are not", async () => {
    await inZoneAsync("America/Los_Angeles", async () => {
      serveEarlierDayGap();
      renderPage();
      await summary();
      expect(warningText()).toBe("Data not complete before Nov 18 17:57: Prod in us-east-1 had too many events to fetch in time.");
      expect(document.querySelector('[data-band="too_busy"]')?.getAttribute("title")).toContain("Nov 18 01:10–17:57");
      expect(openWarning().querySelector('[data-stretch="too_busy"]')?.textContent).toContain("Nov 18 01:10–17:57");
      const panel = await unavailablePanel();
      expect(panel.textContent).toContain("Compare not available for 00:40");
      expect(panel.textContent).toContain("Earliest full compare: Nov 18 17:57; pick that or a later time.");
      const times = [...panel.querySelectorAll("[data-compare-time]")].map((e) => e.textContent);
      expect(times).toEqual(["00:50", "Nov 18 23:59"]);
    });
  });

  it("compare change rows share one fixed-width time column wide enough for a date", async () => {
    await inZoneAsync("America/Los_Angeles", async () => {
      serveEarlierDayGap();
      renderPage();
      const panel = await unavailablePanel();
      const cells = [...panel.querySelectorAll<HTMLElement>("[data-compare-time]")];
      expect(cells).toHaveLength(2);
      for (const c of cells) {
        // 5.25rem = 84 px; "Nov 18 23:59" in 11 px tabular figures measures about 72 px.
        expect(c.className.split(" ")).toEqual(expect.arrayContaining(["w-[5.25rem]", "shrink-0", "whitespace-nowrap"]));
        expect(c.className).not.toMatch(/\bmin-w-/);
      }
      expect(new Set(cells.map((c) => c.className)).size).toBe(1);
    });
  });
});

describe("Changes page — layout redesign", () => {
  it("summary card: one row of numbers in order with Accounts n/m and the window switch at its end; no Read time", async () => {
    serveState("11-account-disconnected");
    renderPage();
    const card = await summary();
    const labels = [...card.querySelectorAll("[data-number] dt")].map((e) => e.textContent);
    expect(labels).toEqual(["Changes", "By people", "Automation", "AWS service", "Failed calls", "Accounts"]);
    expect(num("accounts")).toBe("2/3");
    expect(within(card).getByRole("group", { name: "Window" })).toBeTruthy();
    expect(card.textContent).not.toMatch(/Read \d|accounts complete| of 3 accounts/);
  });

  it("likely-cause row: label, time, sentence, account and Region chips, and Show › scrolls to the row and outlines it and its cell", async () => {
    serve(usualView());
    renderPage();
    await summary();
    const row = document.querySelector('[data-likely-cause="found"]') as HTMLElement;
    expect(row.textContent).toBe(`Likely cause${fmtClock(SUSPECT.time)}${SUSPECT.summary}Prodeu-west-1Show ›`);
    expect(row.querySelector("[data-likely-tag]")).toBeNull();
    expect(row.querySelector(".pill")).toBeNull();
    const listRow = document.querySelector(`[data-item="${SUSPECT.id}"]`) as HTMLElement;
    fireEvent.click(within(row).getByRole("button", { name: "Show ›" }));
    expect(listRow.scrollIntoView).toHaveBeenCalled();
    await waitFor(() => expect(listRow.getAttribute("data-shown")).toBe("true"));
    expect(document.querySelector("[data-suspect]")?.getAttribute("data-shown")).toBe("true");
  });

  it("warning line: plain sentences with no separator dot, the first cause and a count of the rest; its popover lists gaps, not-enabled Regions and dropped rows", async () => {
    serve(
      usualView({
        complete: false,
        accounts: accounts3({ complete: false, reason: "account_disconnected", as_of: utc(9, 20), disconnected_at: utc(9, 25) }),
        regions: [
          { region: "eu-west-1", state: "selected", complete: false },
          { region: "af-south-1", state: "not_enabled", complete: false },
          { region: "global", state: "global", complete: true },
        ],
        gaps: [
          { account: STAGING, region: "eu-west-1", from: utc(9, 20), to: NOW, reason: "account_disconnected" },
          { account: STAGING, region: "global", from: utc(9, 20), to: NOW, reason: "account_disconnected" },
          { account: PROD, region: "eu-west-1", from: utc(2, 10), to: utc(3, 40), reason: "throttled" },
        ],
        sync: { ...usualView().sync, dropped: 2 },
      }),
    );
    renderPage();
    await summary();
    const text = warningText() ?? "";
    expect(text).toBe(`Data not complete after ${fmtClock(utc(9, 20))}: Staging was disconnected at ${fmtClock(utc(9, 25))}, and 2 more.`);
    expect(document.querySelector("[data-warning-line]")?.textContent).not.toContain("·");
    const pop = openWarning();
    expect(pop.getAttribute("role")).toBe("dialog");
    expect(pop.querySelectorAll("[data-stretch]")).toHaveLength(3);
    expect(pop.querySelector('[data-not-enabled-region="af-south-1"]')?.textContent).toBe("af-south-1not enabled");
    expect(pop.querySelector("[data-dropped]")?.textContent).toBe("2 malformed events skipped");
    fireEvent.keyDown(document, { key: "Escape" });
    expect(document.querySelector("[data-warning-popover]")).toBeNull();
    expect(document.activeElement).toBe(document.querySelector("[data-warning-line] button"));
  });

  it("No changes shows only when there is no warning line", async () => {
    const zero = { pipeline: 0, person: 0, aws_service: 0, unknown: 0 };
    serve(usualView({ summary: { ...usualView().summary, total: 0, people: 0, errors: 0, by_actor_class: zero }, items: [], alarms: [], suspect: { status: "no_alarm" } }));
    renderPage();
    await summary();
    expect(document.querySelector("[data-no-changes]")).toBeTruthy();
    expect(warningText()).toBeNull();
    cleanup();
    serve(
      usualView({
        complete: false,
        summary: { ...usualView().summary, total: 0, people: 0, errors: 0, by_actor_class: zero },
        gaps: [{ account: PROD, region: "eu-west-1", from: utc(2, 0), to: utc(3, 0), reason: "throttled" }],
        items: [],
        alarms: [],
        suspect: { status: "no_alarm" },
      }),
    );
    renderPage();
    await summary();
    expect(warningText()).not.toBeNull();
    expect(document.querySelector("[data-no-changes]")).toBeNull();
  });

  it("Activity card: row labels are name and number, no status text, top actors or account rows under the grid; gap bands name their reason on hover", async () => {
    serveState("11-account-disconnected");
    renderPage();
    await summary();
    const grid = document.querySelector('section[aria-labelledby="changes-grid-title"]') as HTMLElement;
    const headers = [...grid.querySelectorAll('[role="rowheader"]')].map((e) => e.textContent);
    expect(headers).toEqual(["Automation20", "Person3", "AWS service14", "Errors214"]);
    expect(grid.textContent).not.toMatch(/Complete:|Still|Top actors|Disconnected|Cells marked/);
    expect(grid.querySelector("[data-top-actors]")).toBeNull();
    expect(grid.querySelector("[data-band-label]")).toBeNull();
    expect(grid.querySelector("[data-arriving-label]")).toBeNull();
  });

  it("Activity card: alarms collapse into one chip; each alarm line names its alarm on hover", async () => {
    serve(usualView());
    renderPage();
    await summary();
    expect(document.querySelector("[data-alarms-chip]")?.textContent).toBe("1 alarm ▾");
    expect(document.querySelector(`[data-alarm-line="${ALARM.id}"]`)?.getAttribute("title")).toBe(`checkout 5xx from ${fmtClock(ALARM.start)}`);
  });

  it("Changes list: no arriving banner, gap rows or footer", async () => {
    serveBp("errors-only");
    renderPage();
    await summary();
    const list = document.querySelector('section[aria-labelledby="changes-list-title"]') as HTMLElement;
    expect(list.querySelector("[data-arriving-row]")).toBeNull();
    expect(list.querySelector("[data-gap-row]")).toBeNull();
    expect(list.querySelector("[data-list-footer]")).toBeNull();
    expect(list.querySelector("[data-alarm-panel]")).toBeNull();
    expect(list.textContent).not.toMatch(/Showing|newest first|unknown in/);
  });

  it("Compare: no repeated notes; the to picker offers Now or a time between from and now, sent as to", async () => {
    serve(states["15-late-delivery"].view, usualCompare({ changes_between: [item({ id: "cb1", time: utc(12, 0), summary: "Between" })], changes_between_total: 1 }));
    renderPage();
    const box = await compareBox();
    expect(box.textContent).not.toMatch(/latest snapshot|kept longer/);
    const from = at(-6 * 3_600_000);
    expect((vi.mocked(api.awsChangesCompare).mock.calls[0][0] as CompareQuery).to).toBeUndefined();
    const button = screen.getByRole("button", { name: "Compare to" });
    expect(button.textContent).toBe("Now");
    fireEvent.click(button);
    const options = screen.getAllByRole("option").map((o) => o.textContent);
    const want = compareToOptions(from, "", NOW).map((o) => o.label);
    expect(options).toEqual(want);
    expect(options[0]).toBe("Now");
    expect(options.length).toBe(7);
    fireEvent.mouseDown(screen.getByRole("option", { name: fmtClock(utc(12, 0)) }));
    await waitFor(() => expect((vi.mocked(api.awsChangesCompare).mock.calls.at(-1)?.[0] as CompareQuery).to).toBe(utc(12, 0)));
    expect(await screen.findByText(`Changes between ${fmtClock(from)} and ${fmtClock(utc(12, 0))} (1)`)).toBeTruthy();
  });

  it("compareToOptions: Now first, whole hours after from and before now, newest first; a picked time off the hour keeps its option", () => {
    const opts = compareToOptions(utc(11, 30), utc(12, 45), utc(14, 10));
    expect(opts.map((o) => o.value)).toEqual(["", utc(14, 0), utc(13, 0), utc(12, 45), utc(12, 0)]);
  });

  it("Changes and Compare share one fixed, capped height and scroll inside, even with 200 rows loaded", async () => {
    const many = Array.from({ length: 200 }, (_, i) => item({ id: `bulk-${i}`, time: at(-(i + 1) * 60_000), summary: `Bulk change ${i}` }));
    serve(usualView({ items: many, suspect: { status: "no_alarm" }, alarms: [] }));
    renderPage();
    await summary();
    const changes = document.querySelector('[data-panel="changes"]') as HTMLElement;
    const compare = document.querySelector('[data-panel="compare"]') as HTMLElement;
    expect(PANEL_HEIGHT).toBe("h-[clamp(480px,calc(100dvh_-_6rem),720px)]");
    expect(changes.className).toContain(PANEL_HEIGHT);
    expect(compare.className).toContain(PANEL_HEIGHT);
    const listScroll = changes.querySelector("[data-list-scroll]") as HTMLElement;
    expect(listScroll.className).toContain("overflow-y-auto");
    expect(listScroll.className).toContain("min-h-0");
    expect(listScroll.querySelectorAll("[data-item]")).toHaveLength(200);
    expect((compare.querySelector("[data-compare-scroll]") as HTMLElement).className).toContain("overflow-y-auto");
    // Stacked below lg, each card keeps the same cap.
    expect((document.querySelector("[data-panels]") as HTMLElement).className).toMatch(/(^|\s)grid(\s|$)/);
  });
});

function serveBp(name: string) {
  const f = browserPassStates()[name];
  if (!f) throw new Error(`missing fixture ${name}`);
  serve(f.view, f.compare);
  return f;
}

describe("Changes page — scope pickers, card counts and warning causes", () => {
  const regionsOf = (...names: string[]): ChangesView["regions"] =>
    names.map((r) => ({ region: r, state: r === "global" ? "global" : "selected", complete: true }));

  it("the Accounts and Regions pickers stay after a choice, offer All to clear it, and keep focus on their button", async () => {
    const full = usualView({ regions: regionsOf("eu-west-1", "us-east-1", "global") });
    const byAccount = usualView({ accounts: accounts().filter((a) => a.account === PROD), regions: regionsOf("eu-west-1", "global") });
    const byRegion = usualView({ accounts: accounts().filter((a) => a.account === PROD), regions: regionsOf("eu-west-1") });
    vi.mocked(api.awsChanges).mockImplementation(async (q: ChangesQuery = {}) => (q.account ? byAccount : q.region ? byRegion : full));
    vi.mocked(api.awsChangesCompare).mockResolvedValue(usualCompare());
    renderPage();
    await summary();
    const pick = async (name: string, steps: number) => {
      const button = screen.getByRole("button", { name });
      fireEvent.keyDown(button, { key: "Enter" });
      const list = await screen.findByRole("listbox", { name });
      fireEvent.keyDown(list, { key: "Home" });
      for (let i = 0; i < steps; i++) fireEvent.keyDown(list, { key: "ArrowDown" });
      fireEvent.keyDown(list, { key: "Enter" });
    };
    // The page's own reads; the pickers' unfiltered read asks for one row.
    const lastQuery = () => vi.mocked(api.awsChanges).mock.calls.map((c) => c[0] as ChangesQuery).filter((c) => c.limit !== 1).at(-1) as ChangesQuery;
    const options = async (name: string) => {
      fireEvent.click(screen.getByRole("button", { name }));
      const texts = (await screen.findAllByRole("option")).map((o) => o.textContent);
      fireEvent.keyDown(screen.getByRole("listbox", { name }), { key: "Escape" });
      return texts;
    };

    // Accounts, by keyboard: Prod is the second option.
    await pick("Accounts", 1);
    await waitFor(() => expect(lastQuery()).toMatchObject({ account: PROD }));
    await waitFor(() => expect(num("accounts")).toBe("1/1"));
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Accounts" }));
    expect(screen.getByRole("button", { name: "Accounts" }).textContent).toContain("Prod (204811974821)");
    expect(await options("Accounts")).toEqual(expect.arrayContaining(["All 3", expect.stringContaining("Staging")]));
    expect(screen.getByRole("button", { name: "Regions" }).textContent).toBe("3 regions");

    // Regions, by keyboard, while an account is chosen: eu-west-1 is the second option.
    await pick("Regions", 1);
    await waitFor(() => expect(lastQuery()).toMatchObject({ account: PROD, region: "eu-west-1" }));
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Regions" }));
    expect(await options("Regions")).toEqual(["3 regions", "eu-west-1 — complete", "us-east-1 — complete", "Global (IAM, …) — complete"]);
    expect(await options("Accounts")).toContain("All 3");

    // All clears each filter in turn.
    await pick("Accounts", 0);
    await waitFor(() => expect(lastQuery().account).toBeUndefined());
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Accounts" }));
    await pick("Regions", 0);
    await waitFor(() => expect(lastQuery().region).toBeUndefined());
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Regions" }));
    expect(screen.getByRole("button", { name: "Regions" }).textContent).toBe("3 regions");
  });

  it("the Regions options read in full: they wrap in a list that may grow wider than the button", async () => {
    serve(
      usualView({
        regions: [
          { region: "us-east-1", state: "selected", complete: false },
          { region: "global", state: "global", complete: false },
          { region: "af-south-1", state: "not_enabled", complete: false },
        ],
      }),
    );
    renderPage();
    await summary();
    fireEvent.click(screen.getByRole("button", { name: "Regions" }));
    const list = screen.getByRole("listbox", { name: "Regions" });
    expect(list.className).toContain("w-max");
    expect(list.classList.contains("w-full")).toBe(false);
    const labels = [...list.querySelectorAll("[role=option] > span")];
    expect(labels.map((l) => l.textContent)).toEqual(["2 regions", "us-east-1 — partly unknown", "Global (IAM, …) — partly unknown", "af-south-1 — not enabled"]);
    labels.forEach((l) => expect(l.className).not.toContain("truncate"));
  });

  it("By people counts changes by people, the same figure as the grid's Person row, not distinct people", async () => {
    serve(usualView({ summary: { ...usualView().summary, total: 12, people: 2, by_actor_class: { pipeline: 9, person: 3, aws_service: 0, unknown: 0 } } }));
    renderPage();
    await summary();
    expect([num("changes"), num("people"), num("pipeline"), num("aws_service")]).toEqual(["12", "3", "9", "0"]);
    const personRow = document.querySelector('[data-row="person"] [data-row-total]')?.textContent;
    expect(personRow).toBe(num("people"));
  });

  it("one CloudTrail denial in one account is one cause, even with its global stretch unknown too", () => {
    const view = usualView({
      complete: false,
      accounts: accounts({ [STAGING]: { complete: false, reason: "cloudtrail_denied" } }),
      gaps: [
        { account: STAGING, region: "us-east-1", from: at(-24 * 3_600_000), to: NOW, reason: "cloudtrail_denied", missing_permission: "cloudtrail:LookupEvents" },
        { account: STAGING, region: "global", from: at(-24 * 3_600_000), to: NOW, reason: "global_unknown" },
      ],
    });
    expect(warningLine(view, "24h")?.text).toBe("Data not complete: Versus can't read CloudTrail in Staging.");
    // A second account's problem is still a second cause.
    const two = { ...view, gaps: [...view.gaps, { account: PROD, region: "eu-west-1", from: at(-24 * 3_600_000), to: NOW, reason: "throttled" }] };
    expect(warningLine(two, "24h")?.text).toBe("Data not complete: Versus can't read CloudTrail in Staging, and 1 more.");
  });

  it("during the first read an account whose credentials are not valid is named with Reconnect, not as reading", () => {
    const view = usualView({
      complete: false,
      sync: { ...usualView().sync, first_read_pending: true },
      accounts: accounts({ [PROD]: { complete: false, reason: "not_checked" }, [STAGING]: { complete: false, reason: "credentials_invalid", as_of: null } }),
      gaps: [{ account: PROD, region: "ap-southeast-5", from: at(-24 * 3_600_000), to: NOW, reason: "not_checked" }],
    });
    const line = warningLine(view, "24h");
    expect(line?.text).toBe("Data not complete: the credentials for Staging are not valid.");
    expect(line?.reconnect).toBe(true);
    expect(line?.reading).toBe(false);
  });

  it("with nothing read anywhere in the window every number reads a dash and the warning says why; anything read shows real numbers", async () => {
    const zero = { pipeline: 0, person: 0, aws_service: 0, unknown: 0 };
    const allGaps = (fill: ChangesView["buckets"][number]["state"]) =>
      usualView({
        complete: false,
        summary: { ...usualView().summary, total: 0, people: 0, errors: 0, top_actors: [], by_actor_class: zero },
        buckets: buckets(() => ({ state: fill })),
        accounts: accounts({
          [PROD]: { complete: false, reason: "account_disconnected" },
          [SHARED]: { complete: false, reason: "account_disconnected" },
          [STAGING]: { complete: false, reason: "account_disconnected" },
        }),
        items: [],
        alarms: [],
        suspect: { status: "no_alarm" },
      });
    serve(allGaps("gap"));
    renderPage();
    await summary();
    expect([num("changes"), num("people"), num("pipeline"), num("aws_service"), num("errors"), num("accounts")]).toEqual(["—", "—", "—", "—", "—", "—"]);
    expect(document.querySelector("[data-no-changes]")).toBeNull();
    expect(warningText()).toMatch(/^Data not complete/);
    cleanup();
    serve(allGaps("partial"));
    renderPage();
    await summary();
    expect([num("changes"), num("people"), num("errors"), num("accounts")]).toEqual(["0", "0", "0", "0/3"]);
  });

  it("with nothing read anywhere in the window the grid's row totals read a dash; anything read shows the real totals", async () => {
    const zero = { pipeline: 0, person: 0, aws_service: 0, unknown: 0 };
    const allGaps = (fill: ChangesView["buckets"][number]["state"]) =>
      usualView({
        complete: false,
        summary: { ...usualView().summary, total: 0, people: 0, errors: 0, top_actors: [], by_actor_class: zero },
        buckets: buckets(() => ({ state: fill })),
        accounts: accounts({
          [PROD]: { complete: false, reason: "throttled" },
          [SHARED]: { complete: false, reason: "throttled" },
          [STAGING]: { complete: false, reason: "throttled" },
        }),
        items: [],
        alarms: [],
        suspect: { status: "no_alarm" },
      });
    serve(allGaps("gap"));
    renderPage();
    await summary();
    expect(num("people")).toBe("—");
    expect(rowTotals()).toEqual(["—", "—", "—", "—"]);
    expect(document.querySelectorAll('[data-row] [role="cell"][data-state="gap"]').length).toBeGreaterThan(0);
    cleanup();
    serve(allGaps("partial"));
    renderPage();
    await summary();
    expect(rowTotals()).toEqual(["0", "0", "0", "0"]);
  });
});

describe("Changes page — real-AWS run repairs", () => {
  it("compare names not_on_plan and not_available_in_region as Settings does and never as Not enabled", () => {
    const one = usualView({ accounts: [{ account: STAGING, name: "Staging", complete: true, reason: null, as_of: null }] });
    const lines = partialLines(
      {
        partial: [
          { account: STAGING, region: "global", service: "aws-health", history_starts: null, reason: "not_on_plan", excluded: true },
          { account: STAGING, region: "eu-west-1", service: "eks", history_starts: null, reason: "not_available_in_region", excluded: true },
          { account: STAGING, region: "us-east-1", service: "ec2", history_starts: null, reason: "not_on_plan" },
          { account: STAGING, region: "us-east-1", service: "ecs", history_starts: null, reason: "some_future_reason", excluded: true },
        ],
      },
      one,
    ).map((l) => l.text);
    expect(lines).toEqual([
      "AWS Health: not on this support plan",
      "EKS: not offered in this Region",
      "EC2: not on this support plan",
      "ECS: reason not given",
    ]);
    expect(lines.join(" ")).not.toMatch(/not enabled/i);
    expect(gapReasonText("not_on_plan")).toBe("not on this support plan");
    expect(gapReasonText("not_available_in_region")).toBe("not offered in this Region");
  });

  it("the pipeline actor class reads Automation on the card, the grid row and the list icon, and the API value stays pipeline", async () => {
    serve(usualView());
    renderPage();
    const card = await summary();
    expect(card.querySelector('[data-number="pipeline"] dt')?.textContent).toBe("Automation");
    expect(document.querySelector('[data-row="pipeline"] [role="rowheader"] span')?.textContent).toBe("Automation");
    const icon = document.querySelector('[data-kind="change"] [data-actor-icon="pipeline"]') as HTMLElement;
    expect(icon.getAttribute("title")).toBe("Automation");
    expect(document.body.textContent).not.toMatch(/\bPipeline\b/);
    const q = vi.mocked(api.awsChanges).mock.calls[0][0] as ChangesQuery;
    expect(changesQueryString({ ...q, actor_class: "pipeline" })).toContain("actor_class=pipeline");
  });

  it("top actors and list rows show the principal name whatever the class, and Unknown actor only without one", async () => {
    const top = [
      { actor: "AWSServiceRoleForConfig", actor_class: "aws_service" as const, count: 1756, account: PROD },
      { actor: "KarpenterNodeRole-prod", actor_class: "unknown" as const, count: 1019, account: PROD },
      { actor: "", actor_class: "unknown" as const, count: 375, account: PROD },
      { actor: "KarpenterControllerRole-prod", actor_class: "unknown" as const, count: 371, account: PROD },
    ];
    const named = item({ id: "ev-athena", time: utc(13, 30), actor: "athena-irsa-role", actor_class: "unknown", summary: "Start query execution" });
    const nameless = item({ id: "ev-token", time: utc(13, 20), actor: "", actor_class: "unknown", summary: "Token request" });
    serve(usualView({ summary: { ...usualView().summary, top_actors: top }, items: [named, nameless, ...usualItems()] }));
    renderPage();
    await summary();
    const shown = [...document.querySelectorAll("[data-top-actor]")].map((e) => e.textContent);
    expect(shown).toEqual(["AWSServiceRoleForConfig 1,756", "KarpenterNodeRole-prod 1,019", "Unknown actor 375", "KarpenterControllerRole-prod 371"]);
    expect(new Set(shown.map((t) => t?.replace(/ [\d,]+$/, ""))).size).toBe(shown.length);
    expect(document.querySelector('[data-item="ev-athena"]')?.textContent).toContain("athena-irsa-role");
    expect(document.querySelector('[data-item="ev-athena"]')?.textContent).not.toContain("Unknown actor");
    expect(document.querySelector('[data-item="ev-token"]')?.textContent).toContain("Unknown actor");
    const twoNameless = topActorLabels(
      usualView({
        summary: {
          ...usualView().summary,
          top_actors: [
            { actor: "", actor_class: "unknown", count: 3, account: PROD },
            { actor: "", actor_class: "person", count: 2, account: PROD },
            { actor: "a1@example.com", actor_class: "person", count: 2, account: PROD },
            { actor: "a2@example.com", actor_class: "person", count: 1, account: PROD },
          ],
        },
      }),
    ).map((a) => a.label);
    expect(new Set(twoNameless).size).toBe(twoNameless.length);
  });

  it("during the first read every card number and row total reads a dash and a hatched cell names what is not read yet", async () => {
    const zero = { pipeline: 0, person: 0, aws_service: 0, unknown: 0 };
    serve(
      usualView({
        complete: false,
        summary: { ...usualView().summary, total: 0, people: 0, errors: 0, top_actors: [], by_actor_class: zero },
        sync: { ...usualView().sync, first_read_pending: true },
        buckets: buckets(() => ({ state: "gap" })),
        gaps: [{ account: PROD, region: "ap-southeast-5", from: at(-24 * 3_600_000), to: NOW, reason: "not_checked" }],
        accounts: accounts({ [PROD]: { complete: false, reason: "not_checked" } }),
        items: [],
        alarms: [],
        suspect: { status: "no_alarm" },
      }),
    );
    renderPage();
    await summary();
    expect([num("changes"), num("people"), num("pipeline"), num("aws_service"), num("errors"), num("accounts")]).toEqual(["—", "—", "—", "—", "—", "—"]);
    expect(rowTotals()).toEqual(["—", "—", "—", "—"]);
    expect(warningText()).toBe("Reading the last 24 h…");
    const cell = document.querySelector('[data-row="pipeline"] [role="cell"][data-state="gap"]') as HTMLElement;
    expect(cell.getAttribute("title")).toBe("Prod ap-southeast-5: not checked yet");
  });

  it("a new span names itself on the chip from the first frame and shows dashes, not the previous span's numbers, until its answer lands", async () => {
    const since = utc(13, 37);
    const until = utc(13, 51);
    let land: (v: ChangesView) => void = () => {};
    vi.mocked(api.awsChanges)
      .mockResolvedValueOnce(usualView())
      .mockImplementationOnce(() => new Promise<ChangesView>((r) => (land = r)));
    vi.mocked(api.awsChangesCompare).mockResolvedValue(usualCompare());
    function Go() {
      const nav = useNavigate();
      return (
        <button type="button" onClick={() => nav(`/aws/changes?since=${encodeURIComponent(since)}&until=${encodeURIComponent(until)}`)}>
          go to span
        </button>
      );
    }
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={qc}>
        <ToastProvider>
          <MemoryRouter initialEntries={["/aws/changes"]} future={{ v7_startTransition: true, v7_relativeSplatPath: true }}>
            <Go />
            <Routes>
              <Route path="/aws/changes" element={<AwsChangesPage />} />
            </Routes>
          </MemoryRouter>
        </ToastProvider>
      </QueryClientProvider>,
    );
    await summary();
    expect(num("changes")).toBe("37");
    fireEvent.click(screen.getByRole("button", { name: "go to span" }));
    const chip = await waitFor(() => {
      const el = document.querySelector("[data-span]");
      if (!el) throw new Error("no chip yet");
      return el as HTMLElement;
    });
    expect(chip.textContent).toBe(fmtSpanOrDay(since, until, until, " – "));
    expect([num("changes"), num("people"), num("pipeline"), num("aws_service")]).toEqual(["—", "—", "—", "—"]);
    expect(rowTotals().every((t) => t === "—")).toBe(true);
    expect(document.querySelector("[data-top-actors]")).toBeNull();
    await act(async () => {
      land(usualView({ window: { since, until, settled_until: until }, summary: { ...usualView().summary, total: 12, by_actor_class: { pipeline: 9, person: 3, aws_service: 0, unknown: 0 } } }));
    });
    await waitFor(() => expect(num("changes")).toBe("12"));
    expect(num("pipeline")).toBe("9");
  });
});

describe("Changes page — banned wording guard", () => {
  const BANNED: Array<[string, RegExp]> = [
    ["Partial", /partial/i],
    ["at least", /at least/i],
    // "can't read CloudTrail" is the approved denial wording; any other " read" is not.
    [" read", /(^|\s)(?<!can't\s)read\b/i],
    ["N+", /\d\+/],
    ["still arriving", /still arriving/i],
    ["Suspect", /suspect/i],
    ["Read ", /Read /],
    // The `pipeline` actor class reads Automation on the page.
    ["Pipeline", /\bPipelines?\b/],
  ];
  const all: Array<[string, ChangesStateFixture]> = [
    ...Object.entries(changesStates()),
    ...Object.entries(amendmentStates()),
    ...Object.entries(browserPassStates()),
  ];

  it.each(all)("no banned wording renders in state %s, popovers and dropdowns opened", async (_name, f) => {
    serve(f.view, f.compare);
    renderPage();
    await summary();
    await compareBox();
    await waitFor(() => expect(document.querySelector("[data-compare] .sk")).toBeNull());
    const seen: string[] = [document.body.textContent ?? ""];
    const triggers = [...document.querySelectorAll<HTMLElement>("button[aria-haspopup]")];
    for (const t of triggers) {
      fireEvent.click(t);
      seen.push(document.body.textContent ?? "");
      await act(async () => {
        fireEvent.keyDown(document.activeElement ?? document, { key: "Escape" });
      });
    }
    const text = seen.join("\n");
    for (const [label, re] of BANNED) {
      expect(text.match(re), `"${label}" in ${_name}`).toBeNull();
    }
  });
});

describe("AwsChangesPage — the shared coverage block", () => {
  it("counts accounts whose change history was read, takes the Regions count from the coverage read, and names a never-read account's reason", async () => {
    const base = usualView();
    serve({
      ...base,
      coverage: {
        // The coverage read counts inventory; Changes keeps its change-history count.
        accounts_read: 2,
        accounts_total: 3,
        regions: ["eu-west-1", "us-east-1", "ap-south-1", "eu-central-1"],
        accounts: base.accounts.map((a, i) => (i === 2 ? { account: a.account, read: "not_read", reason: "assume_role_failed" } : { account: a.account, read: "read" })),
        warning: { not_read: 1 },
      },
    });
    renderPage();
    await summary();
    expect(num("accounts")).toBe("3/3");
    expect(screen.getByRole("button", { name: "Regions" }).textContent).toContain("4 regions");
    fireEvent.click(screen.getByRole("button", { name: "Accounts" }));
    const options = await screen.findAllByRole("option");
    const third = options.find((o) => o.textContent?.includes(base.accounts[2].account)) as HTMLElement;
    expect(third.textContent).toContain("Read role could not be assumed");
    expect(third.textContent).not.toMatch(/assume_role_failed|disconnected/);
  });

  it("names a never-read account's reason in the warning line, never 'disconnected', and keeps the others' numbers", async () => {
    const base = usualView();
    const never = base.accounts[2].account;
    serve({
      ...base,
      complete: false,
      sync: { ...base.sync, first_read_pending: true },
      accounts: base.accounts.map((a) => (a.account === never ? { ...a, complete: false, reason: "account_disconnected", as_of: null, disconnected_at: null } : a)),
      coverage: {
        accounts_read: 2,
        accounts_total: 3,
        regions: ["eu-west-1"],
        accounts: base.accounts.map((a) => (a.account === never ? { account: a.account, read: "not_read", reason: "assume_role_failed" } : { account: a.account, read: "read" })),
        warning: { not_read: 1 },
      },
    });
    renderPage();
    await summary();
    expect(warningText()).toContain("not read: read role could not be assumed");
    expect(warningText()).not.toMatch(/disconnected/i);
    expect(num("accounts")).toBe("2/3");
    expect(num("changes")).not.toBe("—");
  });

  it("keeps its own counts on a server without the coverage block", async () => {
    serve(usualView());
    renderPage();
    await summary();
    expect(num("accounts")).toBe("3/3");
  });
});
