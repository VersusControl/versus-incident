// @vitest-environment jsdom
import { describe, it, expect, afterEach, beforeEach, vi } from "vitest";
import {
  render,
  screen,
  cleanup,
  fireEvent,
  waitFor,
} from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";
import { AlertFatiguePage } from "./AlertFatiguePage";
import {
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
      // Dedicated fatigue-channels endpoint (name + effective enabled).
      listAlertFatigueChannels: vi.fn().mockResolvedValue([]),
      // Read-only sections default to empty/inert so an enabled config renders
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
      getAlertFatigueCorrelation: vi.fn().mockResolvedValue({
        correlation_enabled: false,
        correlation_window_seconds: 0,
        effective_window_seconds: 300,
      }),
      setAlertFatigueCorrelation: vi.fn(),
      listAlertFatigueCorrelationGroups: vi
        .fn()
        .mockResolvedValue({ groups: [], total: 0, page: 1, page_size: 50 }),
      listAlertFatigueCorrelationMembers: vi
        .fn()
        .mockResolvedValue({ group_id: 0, members: [] }),
      getAlertFatigueDependency: vi.fn().mockResolvedValue({
        dependency_suppress_enabled: false,
        dependency_lookback_seconds: 0,
        effective_lookback_seconds: 3600,
      }),
      setAlertFatigueDependency: vi.fn(),
      listAlertFatigueDependencyEdges: vi
        .fn()
        .mockResolvedValue({ edges: [], total: 0, page: 1, page_size: 50 }),
      addAlertFatigueDependencyEdge: vi.fn(),
      removeAlertFatigueDependencyEdge: vi.fn(),
      listAlertFatigueDependencyHolds: vi
        .fn()
        .mockResolvedValue({ holds: [], total: 0, page: 1, page_size: 50 }),
      reclaimAlertFatigueDependencyHold: vi.fn(),
      getChannelSettings: vi.fn().mockResolvedValue({}),
    },
  };
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

const NOW = new Date().toISOString();

function cfg(over: Partial<AlertFatigueConfig> = {}): AlertFatigueConfig {
  return { enabled: false, pending_review: false, fatigue_channel: "", ...over };
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

describe("AlertFatiguePage — enable + config controls", () => {
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

  it("hides the pending-review switch and note until the feature is enabled", async () => {
    vi.mocked(api.getAlertFatigueConfig).mockResolvedValue(cfg());
    renderPage();

    await screen.findByTestId("alert-fatigue-enable-toggle");
    // Disabled: no pending switch, no note, no channel picker.
    expect(screen.queryByTestId("alert-fatigue-pending-toggle")).toBeNull();
    expect(screen.queryByTestId("alert-fatigue-pending-note")).toBeNull();
    expect(screen.queryByTestId("alert-fatigue-channel-select")).toBeNull();
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

  it("populates the channel picker from the dedicated channels endpoint and PUTs the pick", async () => {
    vi.mocked(api.getAlertFatigueConfig).mockResolvedValue(cfg({ enabled: true }));
    vi.mocked(api.listAlertFatigueChannels).mockResolvedValue([
      { name: "slack", enabled: true },
      { name: "telegram", enabled: true },
    ]);
    renderPage();

    const select = (await screen.findByTestId(
      "alert-fatigue-channel-select",
    )) as HTMLSelectElement;
    // The real configured channels are offered as options.
    await waitFor(() =>
      expect(
        Array.from(select.options).map((o) => o.value),
      ).toContain("telegram"),
    );

    fireEvent.change(select, { target: { value: "telegram" } });
    await waitFor(() =>
      expect(api.setAlertFatigueConfig).toHaveBeenCalledWith(
        expect.objectContaining({ fatigue_channel: "telegram", enabled: true }),
      ),
    );
  });

  it("warns inline when the selected fatigue channel was later disabled", async () => {
    vi.mocked(api.getAlertFatigueConfig).mockResolvedValue(
      cfg({ enabled: true, fatigue_channel: "slack", fatigue_channel_valid: false }),
    );
    vi.mocked(api.listAlertFatigueChannels).mockResolvedValue([
      { name: "slack", enabled: false },
      { name: "telegram", enabled: true },
    ]);
    renderPage();

    const warn = await screen.findByTestId("alert-fatigue-channel-warning");
    expect(warn.textContent).toContain("slack");
    expect(warn.textContent?.toLowerCase()).toContain("no longer");
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

  it("filters by status without ever requesting the internal tracking state", async () => {
    vi.mocked(api.listAlertFatigueFingerprints).mockResolvedValue(page([finding()]));
    renderPage();

    const filter = (await screen.findByTestId(
      "alert-fatigue-status-filter",
    )) as HTMLSelectElement;
    // The filter never offers "tracking".
    expect(Array.from(filter.options).map((o) => o.value)).not.toContain(
      "tracking",
    );

    fireEvent.change(filter, { target: { value: "pending_review" } });
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
      fingerprints: [finding({ id: "wire", service: "billing" })],
      total: 1,
      page: 1,
      page_size: 50,
    } as never);
    renderPage();

    expect(await screen.findByText("billing")).toBeTruthy();
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

describe("AlertFatiguePage — new sections gated off for viewers", () => {
  it("renders no analytics/correlation/dependency section for a signed-in viewer", async () => {
    signInAs("viewer");
    renderPage();
    await screen.findByTestId("admin-access-notice");
    expect(screen.queryByTestId("alert-fatigue-analytics")).toBeNull();
    expect(screen.queryByTestId("alert-fatigue-correlation")).toBeNull();
    expect(screen.queryByTestId("alert-fatigue-dependency")).toBeNull();
    // No section data is ever fetched.
    expect(api.getAlertFatigueAnalytics).not.toHaveBeenCalled();
    expect(api.getAlertFatigueCorrelation).not.toHaveBeenCalled();
    expect(api.getAlertFatigueDependency).not.toHaveBeenCalled();
  });
});

describe("AlertFatiguePage — analytics strip", () => {
  beforeEach(() => {
    signInAs("admin");
    vi.mocked(api.getAlertFatigueConfig).mockResolvedValue(cfg({ enabled: true }));
    vi.mocked(api.listAlertFatigueFingerprints).mockResolvedValue(page([]));
  });

  it("renders the noise read-model and top-noisy services", async () => {
    vi.mocked(api.getAlertFatigueAnalytics).mockResolvedValue({
      window: "7d",
      total: 1234,
      by_status: { fatigued: 400 },
      noise_ratio: 0.32,
      diverted: 400,
      reclaim_count: 12,
      reclaim_rate: 0.03,
      top_noisy: [{ service: "checkout", repeat_total: 88, findings: 9 }],
      trend: [],
    });
    renderPage();

    const strip = await screen.findByTestId("alert-fatigue-analytics");
    expect(await screen.findByText("1,234")).toBeTruthy();
    expect(strip.textContent).toContain("32%");
    expect(
      (await screen.findByTestId("alert-fatigue-top-noisy")).textContent,
    ).toContain("checkout");
  });
});

describe("AlertFatiguePage — correlation section", () => {
  beforeEach(() => {
    signInAs("admin");
    vi.mocked(api.getAlertFatigueConfig).mockResolvedValue(cfg({ enabled: true }));
    vi.mocked(api.listAlertFatigueFingerprints).mockResolvedValue(page([]));
    vi.mocked(api.setAlertFatigueCorrelation).mockImplementation((b) =>
      Promise.resolve({ ...b, effective_window_seconds: b.correlation_window_seconds || 300 }),
    );
  });

  it("PUTs correlation_enabled=true when the toggle is turned on", async () => {
    renderPage();
    const toggle = await screen.findByTestId("alert-fatigue-correlation-toggle");
    expect(toggle.getAttribute("aria-checked")).toBe("false");
    fireEvent.click(toggle);
    await waitFor(() =>
      expect(api.setAlertFatigueCorrelation).toHaveBeenCalledWith(
        expect.objectContaining({ correlation_enabled: true }),
      ),
    );
  });

  it("lists correlation groups and expands a group's members", async () => {
    vi.mocked(api.getAlertFatigueCorrelation).mockResolvedValue({
      correlation_enabled: true,
      correlation_window_seconds: 300,
      effective_window_seconds: 300,
    });
    vi.mocked(api.listAlertFatigueCorrelationGroups).mockResolvedValue({
      groups: [
        {
          id: 7,
          group_key: "checkout",
          service: "checkout",
          parent_fingerprint: "pfp",
          parent_severity: "warn",
          window_start: NOW,
          window_end: NOW,
          member_count: 3,
          reason: "same-service",
          created_at: NOW,
        },
      ],
      total: 1,
      page: 1,
      page_size: 50,
    });
    vi.mocked(api.listAlertFatigueCorrelationMembers).mockResolvedValue({
      group_id: 7,
      members: [
        {
          id: 71,
          child_fingerprint: "childfp",
          child_severity: "warn",
          reason: "folded",
          created_at: NOW,
        },
      ],
    });
    renderPage();

    // The group row renders (only the groups-scoped one; ignore other "checkout").
    await screen.findByTestId("alert-fatigue-correlation-groups");
    const expand = await screen.findByTestId("alert-fatigue-group-expand-7");
    fireEvent.click(expand);

    await waitFor(() =>
      expect(api.listAlertFatigueCorrelationMembers).toHaveBeenCalledWith(7),
    );
    expect(
      (await screen.findByTestId("alert-fatigue-group-members-7")).textContent,
    ).toContain("childfp");
  });
});

describe("AlertFatiguePage — dependency section", () => {
  beforeEach(() => {
    signInAs("admin");
    vi.mocked(api.getAlertFatigueConfig).mockResolvedValue(cfg({ enabled: true }));
    vi.mocked(api.listAlertFatigueFingerprints).mockResolvedValue(page([]));
    vi.mocked(api.setAlertFatigueDependency).mockImplementation((b) =>
      Promise.resolve({
        ...b,
        effective_lookback_seconds: b.dependency_lookback_seconds || 3600,
      }),
    );
  });

  it("PUTs dependency_suppress_enabled=true when the toggle is turned on", async () => {
    renderPage();
    const toggle = await screen.findByTestId("alert-fatigue-dependency-toggle");
    expect(toggle.getAttribute("aria-checked")).toBe("false");
    fireEvent.click(toggle);
    await waitFor(() =>
      expect(api.setAlertFatigueDependency).toHaveBeenCalledWith(
        expect.objectContaining({ dependency_suppress_enabled: true }),
      ),
    );
  });

  it("adds and removes a dependency edge", async () => {
    vi.mocked(api.getAlertFatigueDependency).mockResolvedValue({
      dependency_suppress_enabled: true,
      dependency_lookback_seconds: 3600,
      effective_lookback_seconds: 3600,
    });
    vi.mocked(api.listAlertFatigueDependencyEdges).mockResolvedValue({
      edges: [
        { id: 5, downstream: "checkout", upstream: "postgres", created_at: NOW },
      ],
      total: 1,
      page: 1,
      page_size: 50,
    });
    vi.mocked(api.addAlertFatigueDependencyEdge).mockResolvedValue({
      id: 6,
      downstream: "web",
      upstream: "redis",
    });
    vi.mocked(api.removeAlertFatigueDependencyEdge).mockResolvedValue({ id: 5 });
    renderPage();

    // Add an edge.
    const down = await screen.findByTestId("alert-fatigue-edge-downstream");
    const up = screen.getByTestId("alert-fatigue-edge-upstream");
    fireEvent.change(down, { target: { value: "web" } });
    fireEvent.change(up, { target: { value: "redis" } });
    fireEvent.click(screen.getByTestId("alert-fatigue-edge-add"));
    await waitFor(() =>
      expect(api.addAlertFatigueDependencyEdge).toHaveBeenCalledWith({
        downstream: "web",
        upstream: "redis",
      }),
    );

    // Remove the existing edge.
    fireEvent.click(screen.getByTestId("alert-fatigue-edge-remove-5"));
    await waitFor(() =>
      expect(api.removeAlertFatigueDependencyEdge).toHaveBeenCalledWith(5),
    );
  });

  it("lists held symptoms and peeks the redacted content", async () => {
    vi.mocked(api.getAlertFatigueDependency).mockResolvedValue({
      dependency_suppress_enabled: true,
      dependency_lookback_seconds: 3600,
      effective_lookback_seconds: 3600,
    });
    vi.mocked(api.listAlertFatigueDependencyHolds).mockResolvedValue({
      holds: [
        {
          id: 3,
          fingerprint: "holdfp",
          downstream: "checkout",
          upstream: "postgres",
          incident_id: "inc-1",
          alert_content: { title: "db down" },
          source: "agent",
          service: "checkout",
          severity: "warn",
          hold_count: 4,
          reason: "dep-suppress",
          first_seen: NOW,
          last_seen: NOW,
        },
      ],
      total: 1,
      page: 1,
      page_size: 50,
    });
    renderPage();

    await screen.findByTestId("alert-fatigue-holds");
    expect(await screen.findByText("postgres")).toBeTruthy();

    // Peek opens the redacted content.
    fireEvent.click(screen.getByRole("button", { name: "checkout" }));
    await waitFor(() =>
      expect(screen.getByText(/db down/)).toBeTruthy(),
    );
  });

  it("reclaims a held symptom and refreshes the holds list", async () => {
    vi.mocked(api.getAlertFatigueDependency).mockResolvedValue({
      dependency_suppress_enabled: true,
      dependency_lookback_seconds: 3600,
      effective_lookback_seconds: 3600,
    });
    vi.mocked(api.listAlertFatigueDependencyHolds).mockResolvedValue({
      holds: [
        {
          id: 9,
          fingerprint: "holdfp",
          downstream: "checkout",
          upstream: "postgres",
          incident_id: "inc-1",
          alert_content: { title: "db down" },
          source: "agent",
          service: "checkout",
          severity: "warn",
          hold_count: 4,
          reason: "dep-suppress",
          first_seen: NOW,
          last_seen: NOW,
        },
      ],
      total: 1,
      page: 1,
      page_size: 50,
    });
    vi.mocked(api.reclaimAlertFatigueDependencyHold).mockResolvedValue({
      id: 9,
      reclaimed: true,
    });
    renderPage();

    const btn = await screen.findByTestId("alert-fatigue-hold-reclaim-9");
    fireEvent.click(btn);

    await waitFor(() =>
      expect(api.reclaimAlertFatigueDependencyHold).toHaveBeenCalledWith(9),
    );
    // The holds list is re-read after the reclaim (invalidate → refetch).
    await waitFor(() =>
      expect(
        vi.mocked(api.listAlertFatigueDependencyHolds).mock.calls.length,
      ).toBeGreaterThan(1),
    );
  });

  it("never renders the reclaim-from-hold action for a signed-in viewer", async () => {
    vi.mocked(getSsoSession).mockReset();
    signInAs("viewer");
    renderPage();

    // A viewer gets the read-only notice; the dependency section (and its
    // reclaim action) is never mounted, so nothing privileged is offered.
    await screen.findByTestId("admin-access-notice");
    expect(screen.queryByTestId("alert-fatigue-holds")).toBeNull();
    expect(screen.queryByTestId("alert-fatigue-hold-reclaim-9")).toBeNull();
    expect(api.reclaimAlertFatigueDependencyHold).not.toHaveBeenCalled();
  });
});
