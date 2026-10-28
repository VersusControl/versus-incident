// @vitest-environment jsdom
import { describe, it, expect, afterEach, beforeEach, vi } from "vitest";
import {
  render,
  screen,
  cleanup,
  fireEvent,
  within,
} from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, useLocation } from "react-router-dom";
import { ToastProvider } from "@/components/Toast";
import { PatternsPage } from "./PatternsPage";
import {
  api,
  type Pattern,
  type SeasonalBucket,
} from "@/lib/api";

// The logs LIST endpoint strips `samples` and (on the Postgres backend) can
// carry a leaner baseline set than the full record. These pin that opening the
// peek FETCHES the pattern DETAIL (the same read the full page uses) and
// renders the complete baselines — incl. the hour-of-day grid — and the
// redacted sample example from THAT detail, not the thin list row.
//
// The deployment / license probes answer 403 (community / OSS) so the
// licensed-admin bulk column stays absent and each row shows exactly one
// unambiguous "View details" eye.
vi.mock("@/lib/api", async (importActual) => {
  const actual = await importActual<typeof import("@/lib/api")>();
  return {
    ...actual,
    api: {
      ...actual.api,
      listPatterns: vi.fn(),
      listPatternsIndex: vi.fn(),
      getPattern: vi.fn(),
      listBaselines: vi
        .fn()
        .mockRejectedValue(new actual.ApiError(403, "community")),
      listServiceOverrides: vi.fn().mockResolvedValue([]),
      getSSODeployment: vi
        .fn()
        .mockRejectedValue(new actual.ApiError(403, "community")),
    },
  };
});

afterEach(cleanup);

function seasonalOneWarmedHour(): SeasonalBucket[] {
  return Array.from({ length: 24 }, (_, h) =>
    h === 0
      ? { mean: 7.5, variance: 0.25, count: 4 }
      : { mean: 0, variance: 0, count: 0 },
  );
}

// listRow is what the LIST endpoint returns: NO samples, and no seasonal /
// cumulative baselines (the leaner Postgres list shape).
function listRow(overrides: Partial<Pattern> = {}): Pattern {
  return {
    id: "p-checkout-1",
    template: "payment <*> failed",
    first_seen: new Date().toISOString(),
    last_seen: new Date().toISOString(),
    count: 1200,
    baseline_frequency: 1.3,
    verdict: "",
    rule_name: "checkout",
    source: "logs",
    service: "checkout",
    readiness: { ready: false, seen: 40, needed: 100, rate_per_min: 2 },
    ...overrides,
  };
}

// detail is the DETAIL read: full baselines + the redacted sample ring.
function detail(overrides: Partial<Pattern> = {}): Pattern {
  return {
    ...listRow(),
    baseline_variance: 0.25,
    baseline_avg: 1.1,
    seasonal: seasonalOneWarmedHour(),
    samples: ["payment 8471 failed", "payment 22 failed"],
    ...overrides,
  };
}

function renderPage(path = "/agent/logs") {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={qc}>
      <ToastProvider>
        <MemoryRouter
          initialEntries={[path]}
          future={{ v7_startTransition: true, v7_relativeSplatPath: true }}
        >
          <LocationProbe />
          <PatternsPage />
        </MemoryRouter>
      </ToastProvider>
    </QueryClientProvider>,
  );
}

function LocationProbe() {
  const location = useLocation();
  return <div data-testid="search">{location.search}</div>;
}

async function openPeek(): Promise<HTMLElement> {
  const eye = await screen.findByTitle("View details");
  fireEvent.click(eye);
  return screen.getByRole("dialog", { name: "Details panel" });
}

describe("PatternsPage peek — fetches the pattern DETAIL", () => {
  beforeEach(() => {
    vi.mocked(api.listServiceOverrides).mockResolvedValue([]);
    vi.mocked(api.listPatternsIndex).mockResolvedValue({
      patterns: [listRow()],
      total: 1,
      next_offset: null,
    });
    vi.mocked(api.getPattern).mockResolvedValue(detail());
  });

  it("calls getPattern for the opened row (same read as the full page)", async () => {
    renderPage();
    await openPeek();
    expect(api.getPattern).toHaveBeenCalledWith("p-checkout-1");
  });

  it("renders the redacted sample example from the fetched detail", async () => {
    renderPage();
    const panel = await openPeek();
    // The list row carries NO samples — the example can only come from detail.
    expect(await within(panel).findByText("payment 22 failed")).toBeTruthy();
    expect(within(panel).getByText("Example log line")).toBeTruthy();
    expect(
      within(panel).queryByText("No example captured yet"),
    ).toBeNull();
  });

  it("renders the detail's baselines incl. the warmed hour-of-day cell", async () => {
    renderPage();
    const panel = await openPeek();
    // seasonal[0] warmed to 7.5/s — only present on the detail read.
    expect(await within(panel).findByText("7.5")).toBeTruthy();
    // The cumulative-mean baseline is likewise a detail-only number.
    expect(within(panel).getByText(/≈ 1\.1\/s/)).toBeTruthy();
  });

  it("wraps the pattern template instead of scrolling it sideways", async () => {
    renderPage();
    const panel = await openPeek();
    const pre = within(panel).getByText("payment <*> failed");
    expect(pre.tagName).toBe("PRE");
    // The whole template is visible — it wraps rather than scrolling left/right.
    expect(pre.className).toContain("whitespace-pre-wrap");
    expect(pre.className).toContain("break-words");
    expect(pre.className).not.toContain("overflow-auto");
  });
});

// The Logs Verdict cell renders a learning hint next to the "Still learning"
// pill: a seen/needed progress meter ("40 / 100"). The server always ships a
// positive `needed` (a non-positive auto_promote_after is normalized to the
// default upstream), so the cell always has a count target to render against.
describe("PatternsPage — Verdict cell learning hint", () => {
  beforeEach(() => {
    vi.mocked(api.listServiceOverrides).mockResolvedValue([]);
    vi.mocked(api.getPattern).mockResolvedValue(detail());
  });

  it("shows the seen/needed progress meter when a count target exists", async () => {
    vi.mocked(api.listPatternsIndex).mockResolvedValue({
      patterns: [
        listRow({
          readiness: { ready: false, seen: 40, needed: 100, rate_per_min: 2 },
        }),
      ],
      total: 1,
      next_offset: null,
    });
    renderPage();
    const row = await screen.findByText("payment <*> failed");
    const cell = row.closest("tr")!;
    expect(within(cell).getByRole("progressbar")).toBeTruthy();
    expect(cell.textContent).toContain("40");
    expect(cell.textContent).toContain("100");
    expect(within(cell).queryByText(/auto-promotion off/)).toBeNull();
  });
});

// The Patterns list is served as bounded pages (a cheap total + one page), not
// the whole catalog. These pin the paged wiring: the whole-set total drives the
// header, and a server-advertised next page surfaces a "Load more" control that
// pulls the following page on demand.
describe("PatternsPage — server-side paging", () => {
  beforeEach(() => {
    vi.mocked(api.listServiceOverrides).mockResolvedValue([]);
    vi.mocked(api.getPattern).mockResolvedValue(detail());
  });

  it("renders the whole-set total in the header, not the loaded page size", async () => {
    vi.mocked(api.listPatternsIndex).mockResolvedValue({
      patterns: [listRow()],
      total: 4200,
      next_offset: null,
    });
    renderPage();
    // The subtitle reflects the server total (4,200), not the one loaded row.
    expect(await screen.findByText(/4,200 log templates learned/)).toBeTruthy();
  });

  it("loads the next page when Load more is clicked", async () => {
    vi.mocked(api.listPatternsIndex)
      .mockResolvedValueOnce({
        patterns: [listRow({ id: "p-1", template: "first page <*>" })],
        total: 2,
        next_offset: 1,
      })
      .mockResolvedValueOnce({
        patterns: [listRow({ id: "p-2", template: "second page <*>" })],
        total: 2,
        next_offset: null,
      });
    renderPage();

    // First page rendered; the load-more control advertises more rows.
    expect(await screen.findByText("first page <*>")).toBeTruthy();
    const more = await screen.findByTestId("pattern-load-more");
    fireEvent.click(within(more).getByRole("button"));

    // The second page is fetched and appended.
    expect(await screen.findByText("second page <*>")).toBeTruthy();
    expect(api.listPatternsIndex).toHaveBeenCalledWith({ offset: 1 });
  });
});

describe("PatternsPage — group by service and facet filters", () => {
  const rows = [
    listRow({ id: "c1", template: "checkout one", service: "checkout" }),
    listRow({ id: "a1", template: "auth one", service: "auth", rule_name: "auth" }),
    listRow({ id: "c2", template: "checkout two", service: "checkout" }),
    listRow({ id: "c3", template: "checkout three", service: "checkout" }),
  ];

  beforeEach(() => {
    vi.mocked(api.listServiceOverrides).mockResolvedValue([]);
    vi.mocked(api.getPattern).mockResolvedValue(detail());
    vi.mocked(api.listPatternsIndex).mockResolvedValue({
      patterns: rows,
      total: rows.length,
      next_offset: null,
    });
  });

  it("previews two rows per service and expands the rest on demand", async () => {
    renderPage("/agent/logs?group=service");
    const toggle = await screen.findByRole("button", { name: "Expand checkout (3)" });
    expect(screen.getByText("checkout one")).toBeTruthy();
    expect(screen.getByText("checkout two")).toBeTruthy();
    expect(screen.queryByText("checkout three")).toBeNull();
    expect(screen.getByText("auth one")).toBeTruthy();

    fireEvent.click(toggle);
    expect(screen.getByText("checkout three")).toBeTruthy();
    const collapse = screen.getByRole("button", { name: "Collapse checkout (3)" });
    expect(collapse.getAttribute("aria-expanded")).toBe("true");
  });

  it("reveals hidden rows from the Show more row", async () => {
    renderPage("/agent/logs?group=service");
    fireEvent.click(await screen.findByRole("button", { name: "Show 1 more in checkout" }));
    expect(screen.getByText("checkout three")).toBeTruthy();
  });

  it("defaults to By service and keeps Flat available as an explicit URL choice", async () => {
    renderPage();
    await screen.findByRole("button", { name: "Expand checkout (3)" });
    expect(screen.getByRole("tab", { name: "By service" }).getAttribute("aria-selected")).toBe("true");
    expect(screen.getByRole("tab", { name: "Flat" }).getAttribute("aria-selected")).toBe("false");

    cleanup();
    renderPage("/agent/logs?group=flat");
    expect(await screen.findByText("checkout three")).toBeTruthy();
    expect(screen.getByRole("tab", { name: "Flat" }).getAttribute("aria-selected")).toBe("true");
    expect(screen.queryByRole("button", { name: /Expand checkout/ })).toBeNull();
  });

  it("falls back to the grouped default for an unknown group value", async () => {
    renderPage("/agent/logs?group=unrecognized");
    await screen.findByRole("button", { name: "Expand checkout (3)" });
    expect(screen.getByRole("tab", { name: "By service" }).getAttribute("aria-selected")).toBe("true");
    expect(screen.getByRole("tab", { name: "Flat" }).getAttribute("aria-selected")).toBe("false");
  });

  it("keeps unknown scope values inert and preserves them when clearing an unknown facet", async () => {
    renderPage("/agent/logs?scope=unrecognized&f.service=missing");
    expect(await screen.findByRole("button", { name: "Remove filter Service: missing" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: /Ignored/ })).toBeNull();

    fireEvent.click(screen.getByRole("button", { name: "Remove filter Service: missing" }));
    expect(await screen.findByText("checkout one")).toBeTruthy();
    expect(screen.getByTestId("search").textContent).toBe("?scope=unrecognized");
  });

  it("applies a deep-linked facet filter and shows it as a removable chip", async () => {
    renderPage("/agent/logs?f.service=auth");
    expect(await screen.findByText("auth one")).toBeTruthy();
    expect(screen.queryByText("checkout one")).toBeNull();

    fireEvent.click(screen.getByRole("button", { name: "Remove filter Service: auth" }));
    expect(await screen.findByText("checkout one")).toBeTruthy();
  });

  it("checks a value in the filter panel, including an unassigned service", async () => {
    vi.mocked(api.listPatternsIndex).mockResolvedValue({
      patterns: [...rows, listRow({ id: "u1", template: "unassigned one", service: "" })],
      total: rows.length + 1,
      next_offset: null,
    });
    renderPage();
    await screen.findByText("auth one");
    fireEvent.click(screen.getByRole("button", { name: /^Filters/ }));
    const panel = screen.getByRole("dialog", { name: "Filters" });
    const service = within(panel).getByRole("group", { name: /Service/ });

    const auth = within(service).getByRole("checkbox", { name: /auth/ }) as HTMLInputElement;
    fireEvent.click(auth);
    expect(auth.checked).toBe(true);
    expect(await screen.findByRole("button", { name: "Remove filter Service: auth" })).toBeTruthy();
    expect(screen.queryByText("checkout one")).toBeNull();

    const boxes = within(service).getAllByRole("checkbox") as HTMLInputElement[];
    const unassigned = boxes.find((b) => !b.checked && !/auth|checkout/.test(b.closest("label")?.textContent ?? ""));
    fireEvent.click(unassigned!);
    expect(unassigned!.checked).toBe(true);
    expect(await screen.findByText("unassigned one")).toBeTruthy();
  });
});

