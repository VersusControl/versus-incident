// @vitest-environment jsdom
import { act, cleanup, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, useLocation } from "react-router-dom";
import { afterEach, describe, expect, it, vi } from "vitest";
import { api, type KubernetesCrashEvent, type KubernetesIssue } from "./api";
import {
  CrashLogNavigationContext,
  useBreakLogsState,
} from "./useKubernetesBreakLogs";

const issue: KubernetesIssue = {
  root: {
    kind: "Deployment",
    namespace: "shop",
    name: "checkout",
    uid: "root",
  },
  rule: "restarts",
  severity: "critical",
  count: 2,
  broken: true,
  broke_at: "2026-10-21T08:00:00Z",
};
const crash: KubernetesCrashEvent = {
  id: "crash",
  sequence: 1,
  cluster: "production",
  namespace: "shop",
  pod: "checkout-a",
  pod_uid: "pod-a",
  root_uid: "root",
  root_kind: "Deployment",
  root_name: "checkout",
  container: "app",
  type: "termination",
  reason: "OOMKilled",
  exit_code: 137,
  restart_count: 2,
  at: "2026-10-21T08:01:00Z",
  time_source: "finished_at",
  observed_at: "2026-10-21T08:01:01Z",
};
function Probe() {
  const state = useBreakLogsState(issue, vi.fn());
  const location = useLocation();
  return (
    <output aria-label="Crash log state">
      {JSON.stringify({
        search: location.search,
        mode: state.mode,
        pods: state.pods,
      })}
    </output>
  );
}
afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});
describe("Resource crash log navigation", () => {
  it("reuses a mounted matching break-log panel and clears drawer navigation without inventing a resource", async () => {
    vi.spyOn(api, "kubernetesBreakLogs").mockResolvedValue({
      window: { from: "2026-10-21T07:55:00Z", to: "2026-10-21T08:02:00Z" },
      pods: [],
      lines: [],
      markers: [],
      counts: { all: 0, errors: 0, warnings: 0 },
      rate: [],
      patterns: [],
      sources: [],
      coverage: { state: "none", tip: true, reason: "capture_late", gaps: [] },
    });
    const navigation: {
      current: ((event: KubernetesCrashEvent) => boolean) | null;
    } = { current: null };
    const { unmount } = render(
      <QueryClientProvider
        client={
          new QueryClient({ defaultOptions: { queries: { retry: false } } })
        }
      >
        <MemoryRouter
          initialEntries={[
            "/agent/kubernetes?view=timeline&r=core~v1~pods/shop/checkout-a&tab=timeline",
          ]}
        >
          <CrashLogNavigationContext.Provider value={navigation}>
            <Probe />
          </CrashLogNavigationContext.Provider>
        </MemoryRouter>
      </QueryClientProvider>,
    );
    await waitFor(() => expect(navigation.current).toBeTypeOf("function"));
    act(() =>
      expect(navigation.current?.({ ...crash, root_uid: "unrelated" })).toBe(
        false,
      ),
    );
    expect(
      JSON.parse(screen.getByLabelText("Crash log state").textContent!).mode,
    ).toBe("merged");
    act(() => expect(navigation.current?.(crash)).toBe(true));
    const state = JSON.parse(
      screen.getByLabelText("Crash log state").textContent!,
    );
    expect(state.mode).toBe("previous");
    expect(state.pods).toEqual(["pod-a"]);
    const params = new URLSearchParams(state.search);
    expect(params.get("logs_from")).toBe("2026-10-21T07:56:00.000Z");
    expect(params.get("logs_to")).toBe("2026-10-21T08:01:00.000Z");
    expect(params.get("root_uid")).toBe("root");
    expect(params.get("namespace")).toBe("shop");
    expect(params.get("logs_pod")).toBe("pod-a");
    expect(params.get("logs_mode")).toBe("previous");
    expect(params.has("r")).toBe(false);
    expect(params.has("tab")).toBe(false);
    unmount();
    expect(navigation.current).toBeNull();
  });
});
