// @vitest-environment jsdom
import { describe, it, expect, afterEach, beforeEach, vi } from "vitest";
import {
  render,
  screen,
  cleanup,
  fireEvent,
  waitFor,
  within,
} from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";
import { AlertFatiguePage } from "./AlertFatiguePage";
import {
  ApiError,
  api,
  getSsoSession,
  type AlertFatigueConfig,
  type AlertFatigueFinding,
  type AlertFatigueFindingsResponse,
} from "@/lib/api";

// AlertFatiguePage is gated on the caller's effective RBAC role (the SSO
// deployment probe + session whoami), then reads/writes the enterprise
// alert-fatigue config + fingerprint review API. The mock defaults every gate
// to "closed" (no session, community deployment) so a test opts INTO the admin
// surface explicitly.
vi.mock("@/lib/api", async (importActual) => {
  const actual = await importActual<typeof import("@/lib/api")>();
  return {
    ...actual,
    // getSsoSession is a module-level export useEffectiveRole reads directly —
    // default to "no session" (fail closed) unless a test overrides it.
    getSsoSession: vi
      .fn()
      .mockRejectedValue(new actual.ApiError(401, "no session")),
    api: {
      ...actual.api,
      // Deployment probe defaults to community (403) → the "locked" gate.
      getSSODeployment: vi
        .fn()
        .mockRejectedValue(new actual.ApiError(403, "community")),
      getAlertFatigueConfig: vi.fn(),
      setAlertFatigueConfig: vi.fn(),
      listAlertFatigueFingerprints: vi.fn(),
      confirmAlertFatigueFingerprint: vi.fn(),
      reclaimAlertFatigueFingerprint: vi.fn(),
      // The analytics strip defaults to empty/inert so an enabled config renders
      // without a network call; tests override per-case.
      getAlertFatigueAnalytics: vi.fn().mockResolvedValue({
        window: "7d",
        total: 0,
        by_status: {},
        noise_ratio: 0,
        diverted: 0,
        reclaim_count: 0,
        reclaim_rate: 0,
        top_noisy: [],
        trend: [],
      }),
    },
  };
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

const NOW = new Date().toISOString();

function cfg(over: Partial<AlertFatigueConfig> = {}): AlertFatigueConfig {
  return { enabled: false, pending_review: false, ...over };
}

function finding(over: Partial<AlertFatigueFinding> = {}): AlertFatigueFinding {
  return {
    id: "fp1",
    fingerprint: "abc123",
    alert_content: { title: "disk full" },
    source: "agent:prometheus:prod",
    service: "checkout",
    severity: "warn",
    repeat_count: 3,
    first_seen: NOW,
    last_seen: NOW,
    status: "fatigued",
    routed_channel: "slack",
    ...over,
  };
}

function page(
  items: AlertFatigueFinding[],
  over: Partial<AlertFatigueFindingsResponse> = {},
): AlertFatigueFindingsResponse {
  return {
    fingerprints: items,
    total: items.length,
    page: 1,
    page_size: 50,
    ...over,
  };
}

// signInAs makes useEffectiveRole resolve a licensed deployment + a live session
// with the given role (admin/owner unlock the controls; viewer is read-only).
function signInAs(role: string) {
  vi.mocked(api.getSSODeployment).mockResolvedValue({ org: "acme" });
  vi.mocked(getSsoSession).mockResolvedValue({
    org: "acme",
    email: "a@acme.test",
    subject: "sub-1",
    mfa: false,
    role,
    issued_at: NOW,
    expires_at: NOW,
  });
}

function renderPage() {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter
        initialEntries={["/agent/alert-fatigue"]}
        future={{ v7_startTransition: true, v7_relativeSplatPath: true }}
      >
        <AlertFatiguePage />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

describe("AlertFatiguePage — license/role gating (fail closed)", () => {
  it("shows the Enterprise upsell and issues no config read on a community binary", async () => {
    // Default mocks: getSSODeployment 403 → gate "locked".
    renderPage();
    expect(await screen.findByTestId("enterprise-locked")).toBeTruthy();
    expect(api.getAlertFatigueConfig).not.toHaveBeenCalled();
  });

  it("shows the read-only admin notice for a signed-in viewer and reads no config", async () => {
    signInAs("viewer");
    renderPage();
    const notice = await screen.findByTestId("admin-access-notice");
    expect(notice.getAttribute("data-reason")).toBe("role");
    expect(api.getAlertFatigueConfig).not.toHaveBeenCalled();
  });
});

describe("AlertFatiguePage — enable + require-review toggles (read-modify-write)", () => {
  beforeEach(() => {
    signInAs("admin");
    vi.mocked(api.setAlertFatigueConfig).mockImplementation((c) =>
      Promise.resolve(c),
    );
    vi.mocked(api.listAlertFatigueFingerprints).mockResolvedValue(page([]));
  });

  it("PUTs enabled=true when the master switch is turned on", async () => {
    vi.mocked(api.getAlertFatigueConfig).mockResolvedValue(cfg());
    renderPage();

    const toggle = await screen.findByTestId("alert-fatigue-enable-toggle");
    expect(toggle.getAttribute("aria-checked")).toBe("false");
    fireEvent.click(toggle);

    await waitFor(() =>
      expect(api.setAlertFatigueConfig).toHaveBeenCalledWith(
        expect.objectContaining({ enabled: true }),
      ),
    );
  });

  it("read-modify-writes: toggling enabled preserves the sibling pending_review field", async () => {
    // A pending_review already set must NOT be clobbered when this page flips the
    // master enable — the whole merged object is PUT.
    vi.mocked(api.getAlertFatigueConfig).mockResolvedValue(
      cfg({ pending_review: true }),
    );
    renderPage();

    fireEvent.click(await screen.findByTestId("alert-fatigue-enable-toggle"));
    await waitFor(() =>
      expect(api.setAlertFatigueConfig).toHaveBeenCalledWith(
        expect.objectContaining({
          enabled: true,
          pending_review: true,
        }),
      ),
    );
  });

  it("hides the pending-review switch and note until the feature is enabled", async () => {
    vi.mocked(api.getAlertFatigueConfig).mockResolvedValue(cfg());
    renderPage();

    await screen.findByTestId("alert-fatigue-enable-toggle");
    expect(screen.queryByTestId("alert-fatigue-pending-toggle")).toBeNull();
    expect(screen.queryByTestId("alert-fatigue-pending-note")).toBeNull();
  });

  it("shows the pending-review switch with the exact auto-spam note when enabled, and PUTs pending_review", async () => {
    vi.mocked(api.getAlertFatigueConfig).mockResolvedValue(cfg({ enabled: true }));
    renderPage();

    const note = await screen.findByTestId("alert-fatigue-pending-note");
    expect(note.textContent).toContain(
      "Alerts are auto-marked as spam by default — some alerts may stop " +
        "being sent. If you notice alerts missing and want to approve them " +
        "before they're marked as spam, enable pending review.",
    );

    fireEvent.click(screen.getByTestId("alert-fatigue-pending-toggle"));
    await waitFor(() =>
      expect(api.setAlertFatigueConfig).toHaveBeenCalledWith(
        expect.objectContaining({ pending_review: true, enabled: true }),
      ),
    );
  });
});

describe("AlertFatiguePage — config surfaces moved to Admin", () => {
  beforeEach(() => {
    signInAs("admin");
    vi.mocked(api.listAlertFatigueFingerprints).mockResolvedValue(page([]));
  });

  it("no longer renders the channel picker, custom-channel form, correlation, or dependency config", async () => {
    vi.mocked(api.getAlertFatigueConfig).mockResolvedValue(cfg({ enabled: true }));
    renderPage();

    // The Enable toggle proves the admin surface mounted.
    await screen.findByTestId("alert-fatigue-enable-toggle");
    // …but none of the moved CONFIG surfaces are present on this page.
    expect(screen.queryByTestId("alert-fatigue-channel-select")).toBeNull();
    expect(screen.queryByTestId("alert-fatigue-custom-channel")).toBeNull();
    expect(screen.queryByTestId("alert-fatigue-correlation")).toBeNull();
    expect(screen.queryByTestId("alert-fatigue-dependency")).toBeNull();
    // The analytics strip (kept) still renders.
    expect(screen.getByTestId("alert-fatigue-analytics")).toBeTruthy();
  });
});

describe("AlertFatiguePage — fingerprint review table", () => {
  beforeEach(() => {
    signInAs("admin");
    vi.mocked(api.getAlertFatigueConfig).mockResolvedValue(cfg({ enabled: true }));
  });

  it("renders rows and calls confirm/reclaim then refreshes", async () => {
    vi.mocked(api.listAlertFatigueFingerprints).mockResolvedValue(
      page([
        finding({ id: "f-fat", status: "fatigued" }),
        finding({ id: "f-pend", status: "pending_review", service: "payments" }),
      ]),
    );
    vi.mocked(api.confirmAlertFatigueFingerprint).mockResolvedValue(
      finding({ id: "f-pend", status: "fatigued" }),
    );
    vi.mocked(api.reclaimAlertFatigueFingerprint).mockResolvedValue(
      finding({ id: "f-fat", status: "reclaimed" }),
    );
    renderPage();

    // Both service cells render.
    expect(await screen.findByText("checkout")).toBeTruthy();
    expect(screen.getByText("payments")).toBeTruthy();

    // A fatigued row offers only "Not spam"; a pending row offers both.
    expect(screen.getByRole("button", { name: "Confirm spam" })).toBeTruthy();
    const reclaimButtons = screen.getAllByRole("button", { name: "Not spam" });
    expect(reclaimButtons.length).toBe(2);

    fireEvent.click(screen.getByRole("button", { name: "Confirm spam" }));
    await waitFor(() =>
      expect(api.confirmAlertFatigueFingerprint).toHaveBeenCalledWith("f-pend"),
    );

    fireEvent.click(reclaimButtons[0]);
    await waitFor(() =>
      expect(api.reclaimAlertFatigueFingerprint).toHaveBeenCalledWith("f-fat"),
    );

    // The list is re-read after an action (invalidate → refetch).
    await waitFor(() =>
      expect(
        vi.mocked(api.listAlertFatigueFingerprints).mock.calls.length,
      ).toBeGreaterThan(1),
    );
  });

  it("offers a re-fatigue action on a reclaimed row, and no reclaim/confirm-spam control", async () => {
    vi.mocked(api.listAlertFatigueFingerprints).mockResolvedValue(
      page([finding({ id: "f-recl", status: "reclaimed" })]),
    );
    renderPage();

    await screen.findByText("checkout");
    // A reclaimed row is not a dead end: it exposes "Mark as spam"…
    expect(screen.getByRole("button", { name: "Mark as spam" })).toBeTruthy();
    // …and does not offer the reclaim ("Not spam") or pending ("Confirm
    // spam") controls, which don't apply to an already-reclaimed row.
    expect(screen.queryByRole("button", { name: "Not spam" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Confirm spam" })).toBeNull();
  });

  it("re-fatigues a reclaimed row via confirm(id) and refreshes the list", async () => {
    vi.mocked(api.listAlertFatigueFingerprints).mockResolvedValue(
      page([finding({ id: "f-recl", status: "reclaimed" })]),
    );
    vi.mocked(api.confirmAlertFatigueFingerprint).mockResolvedValue(
      finding({ id: "f-recl", status: "fatigued" }),
    );
    renderPage();

    await screen.findByText("checkout");
    fireEvent.click(screen.getByRole("button", { name: "Mark as spam" }));

    // Same mutation the pending-review "Confirm spam" uses: re-fatigue by id.
    await waitFor(() =>
      expect(api.confirmAlertFatigueFingerprint).toHaveBeenCalledWith("f-recl"),
    );
    // The list is re-read so the row flips out of "reclaimed".
    await waitFor(() =>
      expect(
        vi.mocked(api.listAlertFatigueFingerprints).mock.calls.length,
      ).toBeGreaterThan(1),
    );
  });

  it("shows no re-fatigue control (read-only) for a signed-in viewer", async () => {
    // A viewer never reaches the review table — the whole admin body is
    // replaced by the read-only access notice, so the row action is absent.
    signInAs("viewer");
    vi.mocked(api.listAlertFatigueFingerprints).mockResolvedValue(
      page([finding({ id: "f-recl", status: "reclaimed" })]),
    );
    renderPage();

    await screen.findByTestId("admin-access-notice");
    expect(screen.queryByRole("button", { name: "Mark as spam" })).toBeNull();
    expect(api.listAlertFatigueFingerprints).not.toHaveBeenCalled();
  });

  it("filters by status via segmented tabs, including the Tracking state", async () => {
    vi.mocked(api.listAlertFatigueFingerprints).mockResolvedValue(page([finding()]));
    renderPage();

    const tablist = await screen.findByRole("tablist", { name: "Status filter" });
    const tabNames = within(tablist)
      .getAllByRole("tab")
      .map((t) => t.textContent);
    // The segmented filter offers the public statuses plus the Tracking tab
    // (still-paging rows the operator can suppress) and the Unreachable tab
    // (dead keys every other view hides). Each reviewable tab carries its
    // per-status count as a badge (0 when empty); "All" has none, and
    // "Unreachable" has none until its own list answers (by_status can't
    // count a pseudo-status).
    expect(tabNames).toEqual([
      "All",
      "Tracking0",
      "Fatigued0",
      "Pending review0",
      "Reclaimed0",
      "Unreachable",
    ]);

    fireEvent.click(
      within(tablist).getByRole("tab", { name: /^Pending review/ }),
    );
    await waitFor(() =>
      expect(api.listAlertFatigueFingerprints).toHaveBeenCalledWith(
        expect.objectContaining({ status: "pending_review" }),
      ),
    );
  });

  it("loads the next page when Load more is clicked", async () => {
    // First page reports a larger total → hasNextPage. Each page returns rows
    // with distinct ids (page 1 → fp1-*, page 2 → fp2-*), matching how the real
    // API returns non-overlapping rows, so the merged list has unique keys.
    vi.mocked(api.listAlertFatigueFingerprints).mockImplementation((params) =>
      Promise.resolve(
        params?.page === 2
          ? page([finding({ id: "fp2-a" })], {
              total: 120,
              page: 2,
              page_size: 50,
            })
          : page([finding({ id: "fp1-a" })], {
              total: 120,
              page: 1,
              page_size: 50,
            }),
      ),
    );
    renderPage();

    const more = await screen.findByTestId("alert-fatigue-load-more");
    fireEvent.click(more);

    await waitFor(() =>
      expect(api.listAlertFatigueFingerprints).toHaveBeenCalledWith(
        expect.objectContaining({ page: 2 }),
      ),
    );
  });

  it("renders rows from the backend's { fingerprints: [...] } payload", async () => {
    // Build the response verbatim (not via the page() helper) so this test
    // guards the wire-key contract directly: the backend ships `fingerprints`,
    // not `items`. A regression to `items` renders an EMPTY table here.
    vi.mocked(api.listAlertFatigueFingerprints).mockResolvedValue({
      fingerprints: [
        finding({
          id: "wire",
          service: "billing",
          detection_occurrence_count: 500,
        }),
      ],
      total: 1,
      page: 1,
      page_size: 50,
    } as never);
    renderPage();

    expect(await screen.findByText("billing")).toBeTruthy();
    expect(screen.getByText("500")).toBeTruthy();
    expect(screen.queryByText(/No fingerprints yet/)).toBeNull();
  });

  it("sorts server-side: default last_seen desc, and a header click changes sort/dir + refetches", async () => {
    vi.mocked(api.listAlertFatigueFingerprints).mockResolvedValue(page([finding()]));
    renderPage();

    // The initial load requests the default ordering.
    await screen.findByText("checkout");
    expect(api.listAlertFatigueFingerprints).toHaveBeenCalledWith(
      expect.objectContaining({ sort: "last_seen", dir: "desc" }),
    );

    // Clicking the Priority header selects that column (desc by default).
    fireEvent.click(screen.getByTestId("alert-fatigue-sort-priority"));
    await waitFor(() =>
      expect(api.listAlertFatigueFingerprints).toHaveBeenCalledWith(
        expect.objectContaining({ sort: "priority", dir: "desc" }),
      ),
    );

    // Clicking the active column again flips direction to asc.
    fireEvent.click(await screen.findByTestId("alert-fatigue-sort-priority"));
    await waitFor(() =>
      expect(api.listAlertFatigueFingerprints).toHaveBeenCalledWith(
        expect.objectContaining({ sort: "priority", dir: "asc" }),
      ),
    );

    // The UI never requests an unknown sort value the server would 400.
    for (const call of vi.mocked(api.listAlertFatigueFingerprints).mock.calls) {
      const s = call[0]?.sort;
      if (s !== undefined) {
        expect(["last_seen", "repeat_count", "priority"]).toContain(s);
      }
    }
  });
});

describe("AlertFatiguePage — priority column (AF-5)", () => {
  beforeEach(() => {
    signInAs("admin");
    vi.mocked(api.getAlertFatigueConfig).mockResolvedValue(cfg({ enabled: true }));
  });

  it("renders a priority badge from priority_score on the row", async () => {
    vi.mocked(api.listAlertFatigueFingerprints).mockResolvedValue(
      page([
        finding({
          id: "hi",
          service: "checkout",
          priority_score: 0.92,
          priority_reason: "severity=critical",
        }),
      ]),
    );
    renderPage();

    await screen.findByText("checkout");
    // 0.92 → 92 badge.
    expect(screen.getByText("92")).toBeTruthy();
  });
});

describe("AlertFatiguePage — floored (high/critical) rows can't be suppressed", () => {
  beforeEach(() => {
    signInAs("admin");
    vi.mocked(api.getAlertFatigueConfig).mockResolvedValue(cfg({ enabled: true }));
  });

  it("replaces Mark as spam with a non-interactive hint on a floored tracking row", async () => {
    vi.mocked(api.listAlertFatigueFingerprints).mockResolvedValue(
      page([
        finding({
          id: "hi",
          status: "tracking",
          severity: "critical",
          // A severity-only floor: score is BELOW the old 0.8 proxy but the
          // backend authoritatively marks it floored.
          priority_score: 0.7549,
          priority_reason: "severity=critical",
          floor: true,
        }),
      ]),
    );
    renderPage();

    const hint = await screen.findByTestId("alert-fatigue-mark-spam-floored");
    // The affordance is a compact, non-interactive hint (a span), not an
    // enabled button — there's nothing to click and nothing to suppress.
    expect(hint.tagName).toBe("SPAN");
    expect(screen.queryByRole("button", { name: "Mark as spam" })).toBeNull();

    // Interacting with it must NOT fire the suppression mutation.
    fireEvent.click(hint);
    expect(api.confirmAlertFatigueFingerprint).not.toHaveBeenCalled();

    // It conveys the "always pages / can't be suppressed" reason via its
    // visible text plus the full explanation in title/aria-label.
    expect(hint.textContent).toMatch(/always pages/i);
    expect(hint.getAttribute("title")).toMatch(/always page.*can't be suppressed/i);
    expect(hint.getAttribute("aria-label")).toMatch(/cannot be suppressed/i);
  });

  it("annotates a floored fatigued row so it doesn't read as silenced", async () => {
    vi.mocked(api.listAlertFatigueFingerprints).mockResolvedValue(
      page([
        finding({
          id: "f",
          status: "fatigued",
          severity: "critical",
          priority_score: 0.7549,
          floor: true,
        }),
      ]),
    );
    renderPage();

    const note = await screen.findByTestId("alert-fatigue-still-pages");
    expect(note.textContent).toMatch(/still pages/i);
  });

  it("keeps Mark as spam actionable on a non-floored tracking row", async () => {
    vi.mocked(api.listAlertFatigueFingerprints).mockResolvedValue(
      page([
        // High score but NOT floored → still actionable; the button keys off
        // `floor`, never the score.
        finding({
          id: "lo",
          status: "tracking",
          priority_score: 0.95,
          floor: false,
        }),
      ]),
    );
    vi.mocked(api.confirmAlertFatigueFingerprint).mockResolvedValue(
      finding({ id: "lo", status: "fatigued" }),
    );
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "Mark as spam" }));
    await waitFor(() =>
      expect(api.confirmAlertFatigueFingerprint).toHaveBeenCalledWith("lo"),
    );
    // A non-floored row never renders the disabled floored variant.
    expect(screen.queryByTestId("alert-fatigue-mark-spam-floored")).toBeNull();
  });
});

describe("AlertFatiguePage — sections gated off for viewers", () => {
  it("renders no analytics/review section for a signed-in viewer", async () => {
    signInAs("viewer");
    renderPage();
    await screen.findByTestId("admin-access-notice");
    expect(screen.queryByTestId("alert-fatigue-analytics")).toBeNull();
    // No section data is ever fetched.
    expect(api.getAlertFatigueAnalytics).not.toHaveBeenCalled();
  });
});

describe("AlertFatiguePage — analytics strip", () => {
  beforeEach(() => {
    signInAs("admin");
    vi.mocked(api.getAlertFatigueConfig).mockResolvedValue(cfg({ enabled: true }));
    vi.mocked(api.listAlertFatigueFingerprints).mockResolvedValue(page([]));
  });

  it("renders the noise read-model and top-noisy services", async () => {
    // The task scenario: total 11 across all statuses, but only 1 reviewable
    // (fatigued) — the other 10 are still tracking. The per-status decomposition
    // now annotates the Fingerprints tabs, not this strip.
    vi.mocked(api.getAlertFatigueAnalytics).mockResolvedValue({
      window: "7d",
      total: 11,
      by_status: { tracking: 10, fatigued: 1, pending_review: 0, reclaimed: 0 },
      noise_ratio: 0.32,
      diverted: 1,
      reclaim_count: 0,
      reclaim_rate: 0,
      top_noisy: [{ service: "checkout", repeat_total: 88, findings: 9 }],
      trend: [],
    });
    renderPage();

    const strip = await screen.findByTestId("alert-fatigue-analytics");
    // The total stat is relabelled + hinted so it no longer implies a
    // row-per-alert table.
    expect(await screen.findByText("Alerts tracked")).toBeTruthy();
    expect(strip.textContent).toContain("across all statuses");
    expect(strip.textContent).toContain("32%");

    // The per-status breakdown pill row no longer lives in the analytics strip.
    expect(
      screen.queryByTestId("alert-fatigue-status-breakdown"),
    ).toBeNull();

    // The stat cards sit on a single row (grid-flow-col), not a wrapping grid.
    const statsRow =
      screen.getByTestId("alert-fatigue-stat-total").parentElement;
    expect(statsRow?.className).toContain("grid-flow-col");
    expect(statsRow?.className).not.toContain("grid-cols-5");

    expect(
      (await screen.findByTestId("alert-fatigue-top-noisy")).textContent,
    ).toContain("checkout");
  });

  it("switches the analytics window via tabs (7d default → 30d)", async () => {
    vi.mocked(api.getAlertFatigueAnalytics).mockResolvedValue({
      window: "7d",
      total: 0,
      by_status: {},
      noise_ratio: 0,
      diverted: 0,
      reclaim_count: 0,
      reclaim_rate: 0,
      top_noisy: [],
      trend: [],
    });
    renderPage();

    const tablist = await screen.findByRole("tablist", {
      name: "Analytics window",
    });
    // The window control keeps its stable test id (now a tablist, not a select).
    expect(tablist.getAttribute("data-testid")).toBe(
      "alert-fatigue-analytics-window",
    );
    const tabs = within(tablist).getAllByRole("tab");
    expect(tabs.map((t) => t.textContent)).toEqual(["7 days", "30 days"]);

    // The default window (7d) issues the first read.
    await waitFor(() =>
      expect(api.getAlertFatigueAnalytics).toHaveBeenCalledWith("7d"),
    );

    fireEvent.click(within(tablist).getByRole("tab", { name: "30 days" }));
    await waitFor(() =>
      expect(api.getAlertFatigueAnalytics).toHaveBeenCalledWith("30d"),
    );
  });

  it("annotates the Fingerprints status tabs with per-status counts", async () => {
    // The counts come from the SAME by_status analytics read-model (default 7d
    // window) the noise strip reads — shared react-query cache, one fetch, no
    // backend change. Missing statuses render as 0.
    vi.mocked(api.getAlertFatigueAnalytics).mockResolvedValue({
      window: "7d",
      total: 15,
      by_status: { tracking: 10, fatigued: 3, pending_review: 2 },
      noise_ratio: 0.2,
      diverted: 3,
      reclaim_count: 0,
      reclaim_rate: 0,
      top_noisy: [],
      trend: [],
    });
    renderPage();

    // The counts now ride on the status tabs as badges, not a separate chip row.
    expect(screen.queryByTestId("alert-fatigue-status-counts")).toBeNull();

    const tablist = await screen.findByRole("tablist", {
      name: "Status filter",
    });
    const tab = (name: string) =>
      within(tablist).getByRole("tab", { name: new RegExp(`^${name}`) });

    // Badges fill in once the shared analytics query resolves.
    await waitFor(() => expect(tab("Tracking").textContent).toContain("10"));
    expect(tab("Fatigued").textContent).toContain("3");
    expect(tab("Pending review").textContent).toContain("2");
    // Missing status → 0.
    expect(tab("Reclaimed").textContent).toContain("0");
    // The "All" tab carries no count badge.
    expect(tab("All").textContent).toBe("All");
    // by_status groups by the STORED status column, so it can never carry the
    // `unreachable` pseudo-status. The tab degrades to no badge rather than
    // printing a wrong 0.
    expect(tab("Unreachable").textContent).toBe("Unreachable");

    // The status-aware caption is gone from the Fingerprints header.
    expect(screen.queryByTestId("alert-fatigue-caption")).toBeNull();
  });

  it("renders honest routed/suppressed stats when the read-model exposes them", async () => {
    // The split read-model reports how many fatigued alerts were ROUTED to a
    // custom channel vs SILENTLY SUPPRESSED — replacing the old "Diverted" stat
    // that implied every fatigued alert was sent somewhere.
    vi.mocked(api.getAlertFatigueAnalytics).mockResolvedValue({
      window: "7d",
      total: 20,
      by_status: { tracking: 12, fatigued: 8 },
      noise_ratio: 0.4,
      diverted: 8,
      routed: 5,
      suppressed: 3,
      reclaim_count: 0,
      reclaim_rate: 0,
      top_noisy: [],
      trend: [],
    });
    renderPage();

    const strip = await screen.findByTestId("alert-fatigue-analytics");
    const routed = await screen.findByTestId("alert-fatigue-stat-routed");
    expect(routed.textContent).toContain("Routed to channel");
    expect(routed.textContent).toContain("5");
    const suppressed = screen.getByTestId("alert-fatigue-stat-suppressed");
    expect(suppressed.textContent).toContain("Suppressed");
    expect(suppressed.textContent).toContain("3");

    // The misleading "Diverted" label is gone, and the single-stat fallback is
    // not rendered when the split fields are present.
    expect(strip.textContent).not.toContain("Diverted");
    expect(screen.queryByTestId("alert-fatigue-stat-fatigued")).toBeNull();
  });

  it("falls back to one honest Fatigued stat when routed/suppressed are absent", async () => {
    // Older read-model: only the `diverted` aggregate. The UI must not resurrect
    // the misleading "Diverted" wording — it shows a plain "Fatigued" count.
    vi.mocked(api.getAlertFatigueAnalytics).mockResolvedValue({
      window: "7d",
      total: 20,
      by_status: { tracking: 12, fatigued: 8 },
      noise_ratio: 0.4,
      diverted: 8,
      reclaim_count: 0,
      reclaim_rate: 0,
      top_noisy: [],
      trend: [],
    });
    renderPage();

    const strip = await screen.findByTestId("alert-fatigue-analytics");
    const fallback = await screen.findByTestId("alert-fatigue-stat-fatigued");
    expect(fallback.textContent).toContain("Fatigued");
    expect(fallback.textContent).toContain("8");
    expect(strip.textContent).not.toContain("Diverted");
    expect(screen.queryByTestId("alert-fatigue-stat-routed")).toBeNull();
    expect(screen.queryByTestId("alert-fatigue-stat-suppressed")).toBeNull();
  });

  it("labels the Fingerprints count by the active filter so it reads as a subset", async () => {
    // The analytics `total` counts all statuses (incl. tracking); the "All" tab
    // lists only reviewable rows. Labelling the header count "reviewable" keeps
    // the two on-screen numbers from reading as a contradiction.
    vi.mocked(api.listAlertFatigueFingerprints).mockResolvedValue(
      page([finding()], { total: 3 }),
    );
    renderPage();

    await screen.findByText("checkout");
    expect(screen.getByText(/reviewable/)).toBeTruthy();
  });
});

describe("AlertFatiguePage — Tracking tab + top-noisy drill-down", () => {
  beforeEach(() => {
    signInAs("admin");
    vi.mocked(api.getAlertFatigueConfig).mockResolvedValue(cfg({ enabled: true }));
  });

  it("lists tracking rows and offers Mark as spam wired to confirm", async () => {
    vi.mocked(api.listAlertFatigueFingerprints).mockResolvedValue(
      page([finding({ id: "f-track", status: "tracking", service: "account" })]),
    );
    vi.mocked(api.confirmAlertFatigueFingerprint).mockResolvedValue(
      finding({ id: "f-track", status: "fatigued" }),
    );
    renderPage();

    // Switch to the Tracking tab; the query is issued with status=tracking.
    const tablist = await screen.findByRole("tablist", { name: "Status filter" });
    fireEvent.click(within(tablist).getByRole("tab", { name: /^Tracking/ }));
    await waitFor(() =>
      expect(api.listAlertFatigueFingerprints).toHaveBeenCalledWith(
        expect.objectContaining({ status: "tracking" }),
      ),
    );

    // The tracking row is actionable via "Mark as spam" → confirm(id).
    await screen.findByText("account");
    fireEvent.click(screen.getByRole("button", { name: "Mark as spam" }));
    await waitFor(() =>
      expect(api.confirmAlertFatigueFingerprint).toHaveBeenCalledWith("f-track"),
    );
    // The list is re-read after the mutation (invalidate → refetch).
    await waitFor(() =>
      expect(
        vi.mocked(api.listAlertFatigueFingerprints).mock.calls.length,
      ).toBeGreaterThan(1),
    );
  });

  it("clicking a top-noisy service filters the table to that service in the Tracking tab", async () => {
    vi.mocked(api.getAlertFatigueAnalytics).mockResolvedValue({
      window: "7d",
      total: 2048,
      by_status: { tracking: 2048, fatigued: 0, pending_review: 0, reclaimed: 0 },
      noise_ratio: 0.9,
      diverted: 0,
      reclaim_count: 0,
      reclaim_rate: 0,
      top_noisy: [{ service: "account", repeat_total: 2048, findings: 12 }],
      trend: [],
    });
    vi.mocked(api.listAlertFatigueFingerprints).mockResolvedValue(
      page([finding({ id: "f-acc", status: "tracking", service: "account" })]),
    );
    renderPage();

    // The top-noisy service renders as a button; clicking it drills in.
    const noisyBtn = await screen.findByTestId("alert-fatigue-top-noisy-account");
    fireEvent.click(noisyBtn);

    // The query is re-issued scoped to the service AND the Tracking status.
    await waitFor(() =>
      expect(api.listAlertFatigueFingerprints).toHaveBeenCalledWith(
        expect.objectContaining({ service: "account", status: "tracking" }),
      ),
    );

    // The status tab reflects Tracking as selected.
    const tablist = screen.getByRole("tablist", { name: "Status filter" });
    expect(
      within(tablist)
        .getByRole("tab", { name: /^Tracking/ })
        .getAttribute("aria-selected"),
    ).toBe("true");

    // A dismissable filter chip shows the active service and clears it.
    const chip = await screen.findByTestId("alert-fatigue-service-chip");
    expect(chip.textContent).toContain("account");
    fireEvent.click(chip);
    await waitFor(() =>
      expect(
        screen.queryByTestId("alert-fatigue-service-chip"),
      ).toBeNull(),
    );
    // After clearing, the query no longer carries a service filter.
    await waitFor(() =>
      expect(api.listAlertFatigueFingerprints).toHaveBeenCalledWith(
        expect.objectContaining({ service: undefined, status: "tracking" }),
      ),
    );
  });
});

// Unreachable rows are stale keys from an older fingerprint format: no future
// alert can match them, so the server hides them from every view except
// ?status=unreachable and refuses confirm/reclaim on them with 409. The UI must
// never offer an action that provably does nothing — and must stay usable
// against a server that has neither the field nor the filter.
describe("AlertFatiguePage — unreachable fingerprints", () => {
  beforeEach(() => {
    signInAs("admin");
    vi.mocked(api.getAlertFatigueConfig).mockResolvedValue(
      cfg({ enabled: true }),
    );
  });

  const unreachableTab = async () => {
    const tablist = await screen.findByRole("tablist", {
      name: "Status filter",
    });
    fireEvent.click(within(tablist).getByRole("tab", { name: /^Unreachable/ }));
  };

  it("renders an unreachable row as non-actionable with an explanation and fires no mutation", async () => {
    vi.mocked(api.listAlertFatigueFingerprints).mockResolvedValue(
      page([
        finding({ id: "dead", status: "fatigued", unreachable: true }),
      ]),
    );
    renderPage();

    const hint = await screen.findByTestId("alert-fatigue-unreachable-action");
    // Non-interactive: a span, not a disabled-looking button.
    expect(hint.tagName).toBe("SPAN");
    expect(screen.queryByRole("button", { name: "Not spam" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Confirm spam" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Mark as spam" })).toBeNull();

    // Clicking the hint does nothing — no write is attempted.
    fireEvent.click(hint);
    expect(api.confirmAlertFatigueFingerprint).not.toHaveBeenCalled();
    expect(api.reclaimAlertFatigueFingerprint).not.toHaveBeenCalled();

    // The reason travels with the row for assistive tech.
    expect(hint.getAttribute("title")).toMatch(
      /older fingerprint format.*age out on retention/is,
    );
    expect(hint.getAttribute("aria-label")).toMatch(/no action available/i);
  });

  it("requests status=unreachable and explains the tab above the rows", async () => {
    vi.mocked(api.listAlertFatigueFingerprints).mockResolvedValue(
      page([finding({ id: "dead", unreachable: true })]),
    );
    renderPage();
    await screen.findByText("checkout");

    await unreachableTab();
    await waitFor(() =>
      expect(api.listAlertFatigueFingerprints).toHaveBeenCalledWith(
        expect.objectContaining({ status: "unreachable" }),
      ),
    );

    const note = await screen.findByTestId("alert-fatigue-unreachable-note");
    expect(note.textContent).toMatch(/stale records from an older fingerprint/i);
    expect(note.textContent).toMatch(/can't be confirmed or reclaimed/i);
    expect(note.textContent).toMatch(/age out on retention/i);
  });

  it("badges the Unreachable tab from its own list total, never from by_status", async () => {
    vi.mocked(api.getAlertFatigueAnalytics).mockResolvedValue({
      window: "7d",
      total: 4,
      by_status: { fatigued: 4 },
      noise_ratio: 1,
      diverted: 0,
      reclaim_count: 0,
      reclaim_rate: 0,
      top_noisy: [],
      trend: [],
    });
    vi.mocked(api.listAlertFatigueFingerprints).mockImplementation((params) =>
      Promise.resolve(
        params?.status === "unreachable"
          ? page([finding({ id: "dead", unreachable: true })], { total: 7 })
          : page([finding()], { total: 4 }),
      ),
    );
    renderPage();

    const tablist = await screen.findByRole("tablist", {
      name: "Status filter",
    });
    const tab = () =>
      within(tablist).getByRole("tab", { name: /^Unreachable/ });
    // by_status has no `unreachable` key, so the tab shows no badge at all
    // rather than a wrong 0.
    expect(tab().textContent).toBe("Unreachable");

    await unreachableTab();
    // Once the view's own list answers, the whole-set total is the true count.
    await waitFor(() => expect(tab().textContent).toBe("Unreachable7"));
  });

  it("surfaces the server's 409 message and refreshes when a row turns out unreachable", async () => {
    vi.mocked(api.listAlertFatigueFingerprints).mockResolvedValue(
      page([finding({ id: "f-fat", status: "fatigued" })]),
    );
    vi.mocked(api.reclaimAlertFatigueFingerprint).mockRejectedValue(
      new ApiError(
        409,
        "fingerprint is unreachable: no future alert can match this key, so the decision would have no effect",
        { error: "…", unreachable: true },
      ),
    );
    renderPage();

    const listCalls = () =>
      vi.mocked(api.listAlertFatigueFingerprints).mock.calls.length;
    await screen.findByRole("button", { name: "Not spam" });
    const before = listCalls();

    fireEvent.click(screen.getByRole("button", { name: "Not spam" }));

    // The SERVER's explanation, not a generic "could not update".
    const err = await screen.findByTestId("alert-fatigue-action-error");
    expect(err.textContent).toMatch(/no future alert can match this key/i);

    // …and the list is re-read so the refused row leaves the view.
    await waitFor(() => expect(listCalls()).toBeGreaterThan(before));
  });

  it("keeps the default tabs free of unreachable rows and fully actionable", async () => {
    // The default view only ever returns reachable rows (unreachable: false).
    vi.mocked(api.listAlertFatigueFingerprints).mockResolvedValue(
      page([
        finding({ id: "f-fat", status: "fatigued", unreachable: false }),
        finding({ id: "f-track", status: "tracking", unreachable: false }),
      ]),
    );
    renderPage();

    expect(await screen.findByRole("button", { name: "Not spam" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Mark as spam" })).toBeTruthy();
    expect(screen.queryByTestId("alert-fatigue-unreachable-action")).toBeNull();
    expect(screen.queryByTestId("alert-fatigue-unreachable-note")).toBeNull();
    // The default list never asks for the pseudo-status.
    expect(api.listAlertFatigueFingerprints).toHaveBeenCalledWith(
      expect.objectContaining({ status: undefined }),
    );
  });

  it("degrades against an older server that sends neither the field nor the filter", async () => {
    // No `unreachable` key on the row → it reads as reachable and stays
    // actionable, exactly as before the contract shipped.
    vi.mocked(api.listAlertFatigueFingerprints).mockImplementation((params) =>
      params?.status === "unreachable"
        ? Promise.reject(new ApiError(400, "invalid status filter"))
        : Promise.resolve(page([finding({ id: "old", status: "fatigued" })])),
    );
    renderPage();
    expect(await screen.findByRole("button", { name: "Not spam" })).toBeTruthy();
    expect(screen.queryByTestId("alert-fatigue-unreachable-action")).toBeNull();

    // The rejected filter resolves to a plain capability note, not a red error
    // wall with a Retry that can only fail again.
    await unreachableTab();
    expect(
      await screen.findByText(/doesn't offer the unreachable view yet/i),
    ).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Retry" })).toBeNull();
    // No stray tab note or badge on a view the server can't serve.
    expect(screen.queryByTestId("alert-fatigue-unreachable-note")).toBeNull();
    const tablist = screen.getByRole("tablist", { name: "Status filter" });
    expect(
      within(tablist).getByRole("tab", { name: /^Unreachable/ }).textContent,
    ).toBe("Unreachable");
  });
});

