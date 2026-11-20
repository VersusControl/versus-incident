// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Route, Routes, useLocation } from "react-router-dom";
import { ToastProvider } from "@/components/Toast";
import { api } from "@/lib/api";
import {
  changesQueryString,
  compareQueryString,
  noChangesText,
  serviceFilterLabel,
  type ChangesQuery,
  type ChangesService,
  type ChangesView,
  type CompareQuery,
} from "@/lib/awsChanges";
import { AwsChangesPage } from "./AwsChangesPage";
import { SAVE_DEBOUNCE_MS } from "./useServiceFilter";
import { SUSPECT, changesStates, usualCompare, usualView } from "./changesFixtures";

// The Changes page's service filter: the saved choice (RDS and IAM by
// default), a shared link's `service=` that wins without saving, the panel's
// counts for every service, the card's filter label, a likely cause outside
// the filter, and the warning line that no filter narrows.

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
  TopBar: ({ title }: { title: string }) => <header>{title}</header>,
}));

const SERVICES: ChangesService[] = [
  { service: "config", label: "Config", count: 2042 },
  { service: "athena", label: "Athena", count: 1190 },
  { service: "rds", label: "RDS", count: 12 },
  { service: "iam", label: "IAM", count: 3 },
];

function view(over: Partial<ChangesView> = {}): ChangesView {
  return usualView({ services: SERVICES, ...over });
}

let search = "";
function LocationProbe() {
  search = useLocation().search;
  return null;
}

function renderPage(route = "/aws/changes") {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <ToastProvider>
        <MemoryRouter initialEntries={[route]} future={{ v7_startTransition: true, v7_relativeSplatPath: true }}>
          <Routes>
            <Route
              path="/aws/changes"
              element={
                <>
                  <AwsChangesPage />
                  <LocationProbe />
                </>
              }
            />
          </Routes>
        </MemoryRouter>
      </ToastProvider>
    </QueryClientProvider>,
  );
}

async function summary(): Promise<HTMLElement> {
  return waitFor(() => {
    const el = document.querySelector("[data-summary-card]");
    if (!el) throw new Error("summary card not rendered");
    return el as HTMLElement;
  });
}

function lastChanges(): ChangesQuery {
  return vi.mocked(api.awsChanges).mock.calls.filter((c) => (c[0] as ChangesQuery).limit !== 1).at(-1)?.[0] as ChangesQuery;
}

function servicesButton(): HTMLElement {
  return within(document.querySelector("[data-services-filter]") as HTMLElement).getByRole("button", { name: /Services/ });
}

function openPanel(): HTMLElement {
  fireEvent.click(servicesButton());
  return screen.getByRole("dialog", { name: "Services" });
}

/** Waits past the save debounce. */
async function afterDebounce() {
  await act(async () => {
    await new Promise((r) => setTimeout(r, SAVE_DEBOUNCE_MS + 100));
  });
}

beforeEach(() => {
  Element.prototype.scrollIntoView = vi.fn();
  search = "";
  vi.mocked(api.awsChanges).mockResolvedValue(view());
  vi.mocked(api.awsChangesCompare).mockResolvedValue(usualCompare());
  vi.mocked(api.awsChangesGroup).mockResolvedValue({ items: [], next_cursor: null });
  vi.mocked(api.awsChangesPreferences).mockResolvedValue({ services: ["rds", "iam"] });
  vi.mocked(api.saveAwsChangesPreferences).mockImplementation(async (p) => p);
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("Changes service filter — default and saved choice", () => {
  it("a user who has never chosen sees RDS and IAM: the first request carries service=rds,iam", async () => {
    renderPage();
    await summary();
    expect(vi.mocked(api.awsChanges).mock.calls[0][0]).toMatchObject({ service: "rds,iam" });
    expect(changesQueryString({ service: "rds,iam" })).toBe(`?service=${encodeURIComponent("rds,iam")}`);
    expect(document.querySelector("[data-service-filter]")?.textContent).toBe("Service filter: RDS, IAM");
  });

  it("falls back to RDS and IAM when the saved choice can't be loaded", async () => {
    vi.mocked(api.awsChangesPreferences).mockRejectedValue(new Error("boom"));
    renderPage();
    await summary();
    expect(lastChanges().service).toBe("rds,iam");
  });

  it("an explicit change applies at once and saves once, debounced; the saved choice comes back on the next visit", async () => {
    renderPage();
    await summary();
    const panel = openPanel();
    fireEvent.click(within(panel).getByRole("checkbox", { name: /^IAM/ }));
    fireEvent.click(within(panel).getByRole("checkbox", { name: /^Config/ }));
    await waitFor(() => expect(lastChanges().service).toBe("rds,config"));
    expect(api.saveAwsChangesPreferences).not.toHaveBeenCalled();
    await afterDebounce();
    expect(api.saveAwsChangesPreferences).toHaveBeenCalledTimes(1);
    expect(api.saveAwsChangesPreferences).toHaveBeenCalledWith({ services: ["rds", "config"] });

    cleanup();
    vi.mocked(api.awsChanges).mockClear();
    vi.mocked(api.awsChangesPreferences).mockResolvedValue({ services: ["rds", "config"] });
    renderPage();
    await summary();
    expect(vi.mocked(api.awsChanges).mock.calls[0][0]).toMatchObject({ service: "rds,config" });
  });

  it("a change followed at once by a reload or tab close is sent once with a request that outlives the page", async () => {
    const fetchMock = vi.fn(async () => new Response(null, { status: 204 }));
    vi.stubGlobal("fetch", fetchMock);
    try {
      const { unmount } = renderPage();
      await summary();
      const panel = openPanel();
      fireEvent.click(within(panel).getByRole("checkbox", { name: /^Athena/ }));
      await waitFor(() => expect(lastChanges().service).toBe("rds,iam,athena"));

      Object.defineProperty(document, "visibilityState", { configurable: true, get: () => "hidden" });
      fireEvent(document, new Event("visibilitychange"));
      fireEvent(window, new Event("pagehide"));

      expect(fetchMock).toHaveBeenCalledTimes(1);
      expect(fetchMock).toHaveBeenCalledWith("/api/admin/aws/changes/preferences", {
        method: "PUT",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ services: ["rds", "iam", "athena"] }),
        credentials: "same-origin",
        cache: "no-store",
        keepalive: true,
      });
      // Neither the debounce nor the unmount sends it again.
      await afterDebounce();
      unmount();
      expect(fetchMock).toHaveBeenCalledTimes(1);
      expect(api.saveAwsChangesPreferences).not.toHaveBeenCalled();
    } finally {
      delete (document as { visibilityState?: unknown }).visibilityState;
      vi.unstubAllGlobals();
    }
  });

  it("Clear asks for every service (no service parameter) and saves the empty list", async () => {
    renderPage();
    await summary();
    const panel = openPanel();
    fireEvent.click(within(panel).getByRole("button", { name: "Clear all" }));
    await waitFor(() => expect(lastChanges().service).toBeUndefined());
    await afterDebounce();
    expect(api.saveAwsChangesPreferences).toHaveBeenCalledWith({ services: [] });
    expect(document.querySelector("[data-service-filter]")).toBeNull();
  });

  it("after Clear all the card shows loading, never the old filter's \"No changes\", until the new answer lands", async () => {
    const none = changesStates()["04-no-changes"];
    let answer: (v: ChangesView) => void = () => {};
    vi.mocked(api.awsChanges).mockImplementation(async (q: ChangesQuery) =>
      q.service ? { ...none.view, services: SERVICES } : new Promise<ChangesView>((resolve) => (answer = resolve)),
    );
    renderPage();
    await summary();
    expect(document.querySelector("[data-no-changes]")?.textContent).toBe("No RDS or IAM changes in the last 24 h");
    fireEvent.click(within(openPanel()).getByRole("button", { name: "Clear all" }));
    await waitFor(() => expect(lastChanges().service).toBeUndefined());
    // The empty RDS/IAM answer stands in only as loading for every service.
    const card = await summary();
    await waitFor(() => expect(card.getAttribute("aria-busy")).toBe("true"));
    expect(document.querySelector("[data-no-changes]")).toBeNull();
    expect(card.textContent).not.toContain("No changes");
    expect(card.querySelector("[data-loading-text]")?.textContent).toBe("Loading…");
    expect(document.querySelector('[data-number="changes"] dd')?.textContent).toBe("—");
    expect(document.querySelector("[data-list-loading]")).toBeTruthy();
    expect(screen.queryByText("Nothing to list.")).toBeNull();
    await act(async () => answer(view()));
    await waitFor(() => expect(card.getAttribute("aria-busy")).toBeNull());
    expect(document.querySelector('[data-number="changes"] dd')?.textContent).not.toBe("—");
  });

  it("Select all checks every listed service and saves them", async () => {
    renderPage();
    await summary();
    const panel = openPanel();
    fireEvent.click(within(panel).getByRole("button", { name: "Select all" }));
    await waitFor(() => expect(lastChanges().service).toBe("config,athena,rds,iam"));
    expect(document.querySelector("[data-service-filter]")?.textContent).toBe("Service filter: 4 services");
    await afterDebounce();
    expect(api.saveAwsChangesPreferences).toHaveBeenCalledWith({ services: ["config", "athena", "rds", "iam"] });
  });

  it("removing a chip under the scope bar is an explicit change and is saved", async () => {
    renderPage();
    await summary();
    fireEvent.click(screen.getByRole("button", { name: "Remove filter Service: IAM" }));
    await waitFor(() => expect(lastChanges().service).toBe("rds"));
    await afterDebounce();
    expect(api.saveAwsChangesPreferences).toHaveBeenCalledWith({ services: ["rds"] });
  });
});

describe("Changes service filter — a shared link", () => {
  it("the URL's service= wins for that view and is never saved", async () => {
    renderPage("/aws/changes?service=s3,ec2");
    await summary();
    expect(vi.mocked(api.awsChanges).mock.calls[0][0]).toMatchObject({ service: "s3,ec2" });
    await afterDebounce();
    expect(api.saveAwsChangesPreferences).not.toHaveBeenCalled();
    // The shared view's services stay listed, and removable, with no count.
    const panel = openPanel();
    expect((within(panel).getByRole("checkbox", { name: /^S3/ }) as HTMLInputElement).checked).toBe(true);
  });

  it("an explicit change on a shared view drops service= from the URL and saves the new choice", async () => {
    renderPage("/aws/changes?service=s3");
    await summary();
    const panel = openPanel();
    fireEvent.click(within(panel).getByRole("checkbox", { name: /^RDS/ }));
    await waitFor(() => expect(lastChanges().service).toBe("s3,rds"));
    expect(new URLSearchParams(search).has("service")).toBe(false);
    await afterDebounce();
    expect(api.saveAwsChangesPreferences).toHaveBeenCalledWith({ services: ["s3", "rds"] });
  });
});

describe("Changes service filter — the panel", () => {
  it("lists every service with changes in the window with its count, unchecked ones included", async () => {
    renderPage();
    await summary();
    const panel = openPanel();
    const rows = [...panel.querySelectorAll("li")].map((li) => li.textContent);
    expect(rows).toEqual(["RDS12", "IAM3", "Config2,042", "Athena1,190"]);
    const checked = (name: RegExp) => (within(panel).getByRole("checkbox", { name }) as HTMLInputElement).checked;
    expect([checked(/^Config/), checked(/^Athena/), checked(/^RDS/), checked(/^IAM/)]).toEqual([false, false, true, true]);
    expect(within(panel).getByRole("searchbox", { name: /Filter services values/ })).toBeTruthy();
  });

  it("checked services sit first and stay put while ticking; a fade and a visible scroll bar mark rows below", async () => {
    // A shared link checks S3 and EC2, which have the fewest changes.
    const many: ChangesService[] = [
      ...SERVICES,
      { service: "ecs", label: "ECS", count: 40 },
      { service: "ec2", label: "EC2", count: 2 },
      { service: "s3", label: "S3", count: 1 },
    ];
    vi.mocked(api.awsChanges).mockResolvedValue(view({ services: many }));
    renderPage("/aws/changes?service=s3,ec2");
    await summary();
    const panel = openPanel();
    const rows = () => [...panel.querySelectorAll("li")].map((li) => li.textContent);
    expect(rows()).toEqual(["EC22", "S31", "Config2,042", "Athena1,190", "ECS40", "RDS12", "IAM3"]);
    fireEvent.click(within(panel).getByRole("checkbox", { name: /^Config/ }));
    await waitFor(() => expect(lastChanges().service).toBe("s3,ec2,config"));
    expect(rows()).toEqual(["EC22", "S31", "Config2,042", "Athena1,190", "ECS40", "RDS12", "IAM3"]);

    const list = panel.querySelector("ul") as HTMLUListElement;
    expect(list.className).toContain("scroll-visible");
    expect(list.className).toContain("max-h-60");
    Object.defineProperty(list, "clientHeight", { configurable: true, value: 240 });
    Object.defineProperty(list, "scrollHeight", { configurable: true, value: 400 });
    list.scrollTop = 0;
    fireEvent.scroll(list);
    expect(panel.querySelector("[data-more-below]")).not.toBeNull();
    list.scrollTop = 160;
    fireEvent.scroll(list);
    expect(panel.querySelector("[data-more-below]")).toBeNull();
  });

  it("search narrows the list; the counts stay the server's", async () => {
    renderPage();
    await summary();
    const panel = openPanel();
    fireEvent.change(within(panel).getByRole("searchbox"), { target: { value: "ath" } });
    expect([...panel.querySelectorAll("li")].map((li) => li.textContent)).toEqual(["Athena1,190"]);
  });

  it("works from the keyboard: Enter opens, Tab reaches search and the boxes, Space toggles, Escape closes back to the button", async () => {
    const user = userEvent.setup();
    renderPage();
    await summary();
    servicesButton().focus();
    await user.keyboard("{Enter}");
    const panel = screen.getByRole("dialog", { name: "Services" });
    await user.tab();
    expect(document.activeElement).toBe(within(panel).getByRole("button", { name: "Select all" }));
    await user.tab();
    expect(document.activeElement).toBe(within(panel).getByRole("searchbox"));
    // Checked services come first: RDS, IAM, then Config.
    await user.tab();
    expect(document.activeElement).toBe(within(panel).getByRole("checkbox", { name: /^RDS/ }));
    await user.tab();
    await user.tab();
    expect(document.activeElement).toBe(within(panel).getByRole("checkbox", { name: /^Config/ }));
    await user.keyboard(" ");
    await waitFor(() => expect(lastChanges().service).toBe("rds,iam,config"));
    await user.keyboard("{Escape}");
    expect(screen.queryByRole("dialog", { name: "Services" })).toBeNull();
    expect(document.activeElement).toBe(servicesButton());
  });
});

describe("Changes service filter — honest card", () => {
  it("names the filter: RDS, IAM up to three names, else a count", () => {
    expect(serviceFilterLabel(["rds", "iam"], SERVICES)).toBe("RDS, IAM");
    expect(serviceFilterLabel(["rds", "iam", "config", "athena", "s3"], SERVICES)).toBe("5 services");
    expect(serviceFilterLabel([], SERVICES)).toBe("All services");
  });

  it("No changes names the services: No RDS or IAM changes in the last 24 h", async () => {
    const v = usualView();
    expect(noChangesText(v, "24h", false, ["rds", "iam"])).toBe("No RDS or IAM changes in the last 24 h");
    expect(noChangesText(v, "24h", false, ["rds", "iam", "s3"])).toBe("No RDS, IAM or S3 changes in the last 24 h");
    expect(noChangesText(v, "24h", false, ["rds", "iam", "s3", "ec2"])).toBe("No changes in 4 services in the last 24 h");
    expect(noChangesText(v, "24h", false, [])).toBe("No changes in the last 24 h");

    const none = changesStates()["04-no-changes"];
    vi.mocked(api.awsChanges).mockResolvedValue({ ...none.view, services: SERVICES });
    renderPage();
    await summary();
    expect(document.querySelector("[data-no-changes]")?.textContent).toBe("No RDS or IAM changes in the last 24 h");
  });

  it("a likely cause outside the filter still shows, marked; Show › adds its service for this view only and scrolls to the row", async () => {
    vi.mocked(api.awsChanges).mockResolvedValue(
      view({ suspect: { status: "found", change_id: SUSPECT.id, change: SUSPECT, alarm_id: null, searched: null, in_filter: false } }),
    );
    renderPage();
    const card = await summary();
    const row = card.querySelector('[data-likely-cause="found"]') as HTMLElement;
    expect(row.querySelector("[data-likely-cause-text]")?.textContent).toBe(SUSPECT.summary);
    expect(row.querySelector("[data-outside-filter]")?.textContent).toBe("Not in your service filter");
    vi.mocked(Element.prototype.scrollIntoView).mockClear();
    fireEvent.click(within(row).getByRole("button", { name: "Show ›" }));
    await waitFor(() => expect(lastChanges().service).toBe("rds,iam,ecs"));
    expect(new URLSearchParams(search).get("service")).toBe("rds,iam,ecs");
    await waitFor(() => expect(Element.prototype.scrollIntoView).toHaveBeenCalled());
    await afterDebounce();
    expect(api.saveAwsChangesPreferences).not.toHaveBeenCalled();
  });

  it("a likely cause inside the filter carries no mark", async () => {
    vi.mocked(api.awsChanges).mockResolvedValue(
      view({ suspect: { status: "found", change_id: SUSPECT.id, change: SUSPECT, alarm_id: null, searched: null, in_filter: true } }),
    );
    renderPage();
    const card = await summary();
    expect(card.querySelector("[data-outside-filter]")).toBeNull();
  });
});

describe("Changes service filter — what it never narrows", () => {
  it("the warning line and its popover read the same whatever the filter; gaps are not filtered in the browser", async () => {
    const denied = changesStates()["02-cloudtrail-denied"];
    const read = async () => {
      await summary();
      const text = document.querySelector("[data-warning-text]")?.textContent;
      fireEvent.click(document.querySelector("[data-warning-line] button[aria-haspopup='dialog']") as HTMLElement);
      const pop = document.querySelector("[data-warning-popover]")?.textContent;
      cleanup();
      return { text, pop };
    };
    vi.mocked(api.awsChanges).mockResolvedValue({ ...denied.view, services: SERVICES });
    vi.mocked(api.awsChangesPreferences).mockResolvedValue({ services: [] });
    renderPage();
    const all = await read();
    vi.mocked(api.awsChangesPreferences).mockResolvedValue({ services: ["rds"] });
    renderPage();
    const one = await read();
    expect(all.text).toBe("Data not complete: Versus can't read CloudTrail in Staging.");
    expect(one).toEqual(all);
    expect(lastChanges().service).toBe("rds");
  });

  it("Compare asks with the same services", async () => {
    renderPage();
    await summary();
    await waitFor(() => expect(api.awsChangesCompare).toHaveBeenCalled());
    expect((vi.mocked(api.awsChangesCompare).mock.calls.at(-1)?.[0] as CompareQuery).service).toBe("rds,iam");
    expect(compareQueryString({ from: "x", service: "rds,iam" })).toContain(`service=${encodeURIComponent("rds,iam")}`);
  });

  it("no banned wording renders with the filter on, its panel and the outside-filter cause shown", async () => {
    vi.mocked(api.awsChanges).mockResolvedValue(
      view({ suspect: { status: "found", change_id: SUSPECT.id, change: SUSPECT, alarm_id: null, searched: null, in_filter: false } }),
    );
    renderPage();
    await summary();
    openPanel();
    const text = document.body.textContent ?? "";
    for (const re of [/partial/i, /at least/i, /(^|\s)(?<!can't\s)read\b/i, /\d\+/, /still arriving/i, /suspect/i, /Read /, /\bPipelines?\b/]) {
      expect(text.match(re), String(re)).toBeNull();
    }
  });
});
