// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { api, ApiError, type ServiceHealthSnapshot, type ServiceTopology } from "@/lib/api";
import { topologyErrorCopy } from "@/lib/serviceTopologyPresentation";
import { ServiceTopologyStrip } from "./ServiceTopologyStrip";

vi.mock("@/lib/api", async (importActual) => {
  const actual = await importActual<typeof import("@/lib/api")>();
  return { ...actual, api: { ...actual.api, getServiceTopology: vi.fn() } };
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

const snapshot = {
  services: [
    { service: "api", severity: "critical" },
    { service: "db", severity: "nominal" },
    { service: "web", severity: "pressure" },
  ],
} as ServiceHealthSnapshot;

function graph(overrides: Partial<ServiceTopology> = {}): ServiceTopology {
  return {
    availability: "ready",
    provenance: ["operator_config"],
    nodes: [{ service: "web" }, { service: "db" }, { service: "api" }],
    edges: [
      { service: "web", depends_on: "db", source: "operator_config" },
      { service: "api", depends_on: "db", source: "operator_config" },
      { service: "web", depends_on: "api", source: "learned_latest" },
    ],
    ...overrides,
  };
}

function renderStrip(onSelectService = vi.fn()) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return {
    client,
    onSelectService,
    ...render(<QueryClientProvider client={client}><ServiceTopologyStrip snapshot={snapshot} onSelectService={onSelectService} /></QueryClientProvider>),
  };
}

function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (reason?: unknown) => void;
  const promise = new Promise<T>((promiseResolve, promiseReject) => {
    resolve = promiseResolve;
    reject = promiseReject;
  });
  return { promise, resolve, reject };
}

describe("ServiceTopologyStrip", () => {
  beforeEach(() => vi.mocked(api.getServiceTopology).mockReset());

  it("renders returned relationships deterministically with status and provenance labels", async () => {
    vi.mocked(api.getServiceTopology).mockResolvedValue(graph());
    renderStrip();

    const relationships = await screen.findAllByRole("listitem");
    expect(relationships.map((item) => item.getAttribute("aria-label"))).toEqual([
      "api depends on db",
      "web depends on api; learned source",
      "web depends on db",
    ]);
    expect(screen.queryByText("Configured")).toBeNull();
    expect(screen.getAllByText("Learned").length).toBeGreaterThan(0);
    expect(screen.getAllByRole("button", { name: "Inspect api, health Critical" })[0].getAttribute("data-impact")).toBe("critical");
    expect(screen.getByTestId("service-topology-strip").getAttribute("tabindex")).toBe("0");
  });

  it.each([
    ["not_configured", "No service dependencies are configured."],
    ["no_data", "No service dependency data is available."],
    ["restricted", "Service topology is unavailable for this session."],
    ["error", "Service topology is currently unavailable."],
  ] as const)("renders an honest %s empty state", async (availability, copy) => {
    vi.mocked(api.getServiceTopology).mockResolvedValue(graph({ availability, provenance: [], nodes: [], edges: [] }));
    renderStrip();
    expect(await screen.findByText(copy)).toBeTruthy();
    expect(screen.queryByTestId("service-topology-strip")).toBeNull();
    expect(screen.getByTestId("service-topology-announcement")).toBeTruthy();
  });

  it("reports partial and truncated coverage including an empty bounded response", async () => {
    vi.mocked(api.getServiceTopology).mockResolvedValue(graph({
      availability: "partial",
      nodes: [],
      edges: [],
      omitted_nodes: 4,
      omitted_edges: 7,
    }));
    renderStrip();
    expect((await screen.findByTestId("service-topology-omitted")).textContent).toContain("4 services and 7 relationships omitted.");
  });

  it("counts relationships dropped because an endpoint is absent as omitted", async () => {
    vi.mocked(api.getServiceTopology).mockResolvedValue(graph({
      edges: [
        { service: "web", depends_on: "db", source: "operator_config" },
        { service: "web", depends_on: "missing", source: "operator_config" },
      ],
      omitted_edges: 2,
    }));
    renderStrip();

    expect(await screen.findByRole("listitem", { name: "web depends on db" })).toBeTruthy();
    expect(screen.queryByText("missing")).toBeNull();
    expect(screen.getByTestId("service-topology-omitted").textContent).toContain("3 relationships omitted.");
  });

  it("re-announces unmatched node activation and clears stale status", async () => {
    vi.mocked(api.getServiceTopology).mockResolvedValue(graph({
      nodes: [...graph().nodes, { service: "worker" }],
    }));
    renderStrip();
    const unmatchedNode = await screen.findByRole("button", { name: "worker, no health snapshot available" });
    const status = screen.getByTestId("service-topology-announcement");
    vi.useFakeTimers();

    fireEvent.click(unmatchedNode);
    await act(() => vi.advanceTimersByTimeAsync(0));
    expect(status.textContent).toBe("worker is not represented in the current health snapshot.");

    fireEvent.click(unmatchedNode);
    expect(status.textContent).toBe("");
    await act(() => vi.advanceTimersByTimeAsync(0));
    expect(status.textContent).toBe("worker is not represented in the current health snapshot.");
    await act(() => vi.advanceTimersByTimeAsync(5_000));
    expect(status.textContent).toBe("");
  });

  it("retains cached topology and offers retry when a refetch fails", async () => {
    const refetch = deferred<ServiceTopology>();
    vi.mocked(api.getServiceTopology)
      .mockResolvedValueOnce(graph())
      .mockReturnValueOnce(refetch.promise);
    const { client } = renderStrip();

    expect(await screen.findByRole("listitem", { name: "web depends on db" })).toBeTruthy();
    const refresh = client.refetchQueries({ queryKey: ["service-topology"] });
    refetch.reject(new ApiError(503, "private upstream response"));
    await refresh;

    expect(await screen.findByText("Showing saved topology. Refresh failed.")).toBeTruthy();
    expect(screen.getByRole("listitem", { name: "web depends on db" })).toBeTruthy();
    expect(screen.queryByText("private upstream response")).toBeNull();
    expect(screen.getByRole("button", { name: "Retry" })).toBeTruthy();
  });

  it.each([401, 403])("removes all prior topology presentation after an explicit %s refetch failure", async (status) => {
    const refetch = deferred<ServiceTopology>();
    vi.mocked(api.getServiceTopology)
      .mockResolvedValueOnce(graph({
        nodes: [...graph().nodes, { service: "worker" }],
        omitted_nodes: 2,
        omitted_edges: 3,
      }))
      .mockReturnValueOnce(refetch.promise);
    const { client } = renderStrip();

    expect(await screen.findByRole("listitem", { name: "web depends on db" })).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "worker, no health snapshot available" }));
    expect(await screen.findByText("worker is not represented in the current health snapshot.")).toBeTruthy();
    expect(screen.queryByText("Configured")).toBeNull();
    expect(screen.getByText("2 services and 3 relationships omitted.")).toBeTruthy();

    const refresh = client.refetchQueries({ queryKey: ["service-topology"] });
    refetch.reject(new ApiError(status, "private tenant policy"));
    await refresh;

    expect((await screen.findByRole("alert")).textContent).toContain(topologyErrorCopy(new ApiError(status, "ignored")));
    expect(document.body.textContent).not.toContain("worker");
    expect(screen.queryByText("Configured")).toBeNull();
    expect(screen.queryByText("2 services and 3 relationships omitted.")).toBeNull();
    expect(screen.queryByRole("listitem", { name: "web depends on db" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Retry" })).toBeNull();
    expect(screen.queryByTestId("service-topology-announcement")).toBeNull();
    expect(document.body.textContent).not.toContain("private tenant policy");
    expect(api.getServiceTopology).toHaveBeenCalledTimes(2);
  });

  it("does not expose authorization error details", () => {
    const message = topologyErrorCopy(new ApiError(403, "private tenant policy"));
    expect(message).toBe("Service topology is unavailable for this session.");
    expect(message).not.toContain("private tenant policy");
  });
});