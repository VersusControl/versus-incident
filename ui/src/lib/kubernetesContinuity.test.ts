// @vitest-environment jsdom
import { act, cleanup, renderHook, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { createElement, type ReactNode } from "react";
import { afterEach, describe, expect, it } from "vitest";
import { ApiError } from "./api";
import { hasKubernetesPublication, kubernetesAccessLost, kubernetesReadNeedsRecovery, kubernetesReportAuthorityLost, kubernetesQueryUsesKinds, kubernetesInventoryNotReady, retainKubernetesPublication } from "./kubernetesContinuity";
import { useKubernetesQuery } from "./useKubernetesCluster";

const committed = { items: ["checkout"], broken: { total: 3 }, allocatable_cpu: "8", observed_at: "2026-10-28T00:00:00Z", sync: { state: "ready", age_s: 20, partial: false } };
const warming = { items: [], broken: { total: 0 }, allocatable_cpu: undefined, observed_at: "2026-10-28T00:13:00Z", sync: { state: "syncing", age_s: 800, partial: false } };
afterEach(cleanup);

describe("Kubernetes publication continuity", () => {
  it("requires published-cluster and committed projection proof for an initial partial timeout", () => {
    const partial = { items: [{ kind: "Deployment", namespace: "shop", name: "checkout" }], counts: { Deployment: 1, Job: 0 }, sync: { state: "error", partial: true },
      partial_failures: [{ resource_id: "batch~v1~cronjobs", class: "timeout" }] };
    expect(hasKubernetesPublication(partial, committed.observed_at)).toBe(true);
    expect(hasKubernetesPublication({ ...partial, items: [], counts: { Deployment: 0 } }, committed.observed_at)).toBe(true);
    for (const value of [
      partial,
      { ...partial, counts: {} },
      { ...partial, counts: { Unknown: 1 } },
      { ...partial, counts: { Deployment: -1 } },
      { ...partial, items: ["untrusted"] },
      { ...partial, items: [{ kind: "CronJob", name: "unobserved", namespace: "shop" }] },
      { ...partial, sync: { state: "syncing", partial: true }, items: [] },
      { ...partial, sync: { state: "error", partial: false } },
      { ...partial, partial_failures: [] },
      ...["forbidden", "unauthorized", "namespace_scope", "credential_unavailable", "unknown"].map(className =>
        ({ ...partial, partial_failures: [{ resource_id: "batch~v1~cronjobs", class: className }] })),
    ]) expect(hasKubernetesPublication(value, value === partial ? undefined : committed.observed_at)).toBe(false);
    expect(hasKubernetesPublication(partial, "invalid")).toBe(false);
  });

  it("admits only the currently scoped first partial response and hides permission denial", async () => {
    const cache = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const partial = { items: [{ kind: "Deployment", namespace: "shop", name: "checkout" }], counts: { Deployment: 1, Job: 0 }, sync: { state: "error", partial: true },
      partial_failures: [{ resource_id: "batch~v1~cronjobs", class: "timeout" }] };
    let reply: typeof partial | Error = partial;
    const wrapper = ({ children }: { children: ReactNode }) => createElement(QueryClientProvider, { client: cache }, children);
    const { result, rerender } = renderHook(({ scope, proof }: { scope: string; proof?: string }) =>
      useKubernetesQuery({ queryKey: ["kubernetes-workloads", scope], queryFn: async () => {
        if (reply instanceof Error) throw reply;
        return reply;
      } }, proof), { wrapper, initialProps: { scope: "shop", proof: undefined as string | undefined } });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(result.current.data).toBeUndefined();
    rerender({ scope: "shop", proof: committed.observed_at });
    expect(result.current.data?.counts).toEqual({ Deployment: 1, Job: 0 });
    rerender({ scope: "other", proof: undefined });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(result.current.data).toBeUndefined();
    rerender({ scope: "shop", proof: committed.observed_at });
    reply = new ApiError(403, "Permission denied");
    await act(async () => { await result.current.refetch(); });
    await waitFor(() => expect(result.current.data).toBeUndefined());
    expect(cache.getQueryData(["kubernetes", "_", "kubernetes-workloads", "shop"])).toBeUndefined();
    cache.clear();
  });

  it("preserves committed rows, capacity and observation time across repeated successful warming and failed publications", () => {
    const refreshing = retainKubernetesPublication(committed, warming);
    const failed = retainKubernetesPublication(refreshing, { ...warming, sync: { state: "error", age_s: 900, partial: true } });
    expect(failed).toMatchObject({ items: ["checkout"], broken: { total: 3 }, allocatable_cpu: "8", observed_at: committed.observed_at, sync: { state: "error", age_s: 900 } });
    expect(hasKubernetesPublication(failed)).toBe(true);
    expect(hasKubernetesPublication(warming)).toBe(false);
  });

  it("keeps unavailable metrics at their committed observation time and marks retained usage stale", () => {
    const metrics = { availability: "available", fresh: true, complete: true, total: 3, cpu: "4", observed_at: committed.observed_at };
    const previous = { availability: "available", fresh: true, observed_at: committed.observed_at, pod_metrics: metrics, node_metrics: metrics };
    const result = retainKubernetesPublication(previous, { ...previous, observed_at: warming.observed_at, availability: "unavailable" });
    expect(result).toMatchObject({ observed_at: committed.observed_at, availability: "stale", fresh: false, pod_metrics: { cpu: "4", availability: "stale", fresh: false } });
  });

  it("replaces prior rows with a genuine complete empty publication", () => {
    const result = retainKubernetesPublication(committed, { ...warming, sync: committed.sync });
    expect(result.items).toEqual([]);
    expect(result.broken.total).toBe(0);
    expect(result.observed_at).toBe(warming.observed_at);
  });

  it("does not merge a successfully permission-reduced projection with old protected values", () => {
    const reduced = { ...warming, sync: { state: "partial", age_s: 1, partial: true }, partial_failures: [{ class: "forbidden" }] };
    expect(retainKubernetesPublication(committed, reduced)).toBe(reduced);
    expect(kubernetesAccessLost(new ApiError(502, "unavailable", { error: { code: "credential_unavailable" } }))).toBe(true);
    expect(kubernetesAccessLost(new ApiError(503, "publication failed"))).toBe(false);
  });

  it("recognizes only structured not-ready responses, never matching error text or other failures", () => {
    expect(kubernetesInventoryNotReady(new ApiError(503, "different wording", { code: "inventory_not_ready", retryable: true }))).toBe(true);
    expect(kubernetesInventoryNotReady(new ApiError(503, "different wording", { error: { code: "inventory_not_ready" } }))).toBe(true);
    for (const error of [
      new ApiError(503, "Kubernetes inventory has not finished syncing"),
      new ApiError(503, "Kubernetes inventory has not finished syncing", { code: "unavailable" }),
      new ApiError(500, "Kubernetes inventory has not finished syncing", { code: "inventory_not_ready" }),
      new ApiError(403, "Kubernetes inventory has not finished syncing", { code: "inventory_not_ready" }),
      new Error("Kubernetes inventory has not finished syncing"),
    ]) expect(kubernetesInventoryNotReady(error)).toBe(false);
  });

  it("recognizes explicit cycle authority failures without confusing isolated gaps or network faults", () => {
    const kind = { state: "error" as const, partial: true, records: 4, error: "forbidden", cycle_failure: true };
    const report = { state: "error", partial: true, age_s: 20, kinds: { Pod: kind }, total_kinds: 1, ready_kinds: 0, failing_kinds: 1, pending_kinds: 0, truncated: false };
    expect(kubernetesReportAuthorityLost(report)).toBe(true);
    expect(kubernetesReportAuthorityLost({ ...report, kinds: { Pod: { ...kind, error: "unauthorized" } } })).toBe(true);
    expect(kubernetesReportAuthorityLost({ ...report, kinds: {}, class: "forbidden" })).toBe(true);
    expect(kubernetesReportAuthorityLost({ ...report, kinds: { Pod: { ...kind, error: "unavailable" } } })).toBe(false);
    expect(kubernetesReportAuthorityLost({ ...report, kinds: { Pod: { ...kind, absent: true } } })).toBe(false);
    expect(kubernetesReportAuthorityLost({ ...report, state: "ready", failing_kinds: 0, kinds: { Secret: { ...kind, cycle_failure: false } }, unreadable: [{ kind: "Secret", class: "forbidden" }] })).toBe(false);
  });

  it("targets denied-kind identities and mixed dependencies, not unrelated rows or coincidental namespace text", () => {
    const denied = new Set(["Secret"]);
    expect(kubernetesQueryUsesKinds(["kubernetes-detail", "core~v1~secrets", "shop", "settings"], undefined, denied)).toBe(true);
    expect(kubernetesQueryUsesKinds(["kubernetes-graph"], { nodes: [{ kind: "Secret" }, { kind: "Pod" }] }, denied)).toBe(true);
    expect(kubernetesQueryUsesKinds(["kubernetes-graph"], undefined, denied)).toBe(true);
    expect(kubernetesQueryUsesKinds(["kubernetes-workloads", "Secret"], { items: [{ kind: "Deployment" }] }, denied)).toBe(false);
    expect(kubernetesQueryUsesKinds(["kubernetes-workloads"], undefined, denied)).toBe(false);
    expect(kubernetesQueryUsesKinds(["kubernetes-graph"], undefined, new Set())).toBe(false);
  });

  it("fences workload counts and unfinished mixed workload projections even when the displayed page has no denied-kind rows", () => {
    const denied = new Set(["Pod"]);
    expect(kubernetesQueryUsesKinds(["kubernetes-workloads", "shop", "", "Deployment"], undefined, denied)).toBe(true);
    expect(kubernetesQueryUsesKinds(["kubernetes-workloads"], { items: [{ kind: "Deployment" }], counts: { Pod: 9, Deployment: 1 } }, denied)).toBe(true);
    expect(kubernetesQueryUsesKinds(["kubernetes-summary"], { counts: { Pod: 0 } }, denied)).toBe(true);
    expect(kubernetesQueryUsesKinds(["kubernetes-workloads"], undefined, new Set(["Secret"]))).toBe(false);
  });

  it("rejects a malformed empty sync envelope without inventing a publication", () => {
    const malformed = { ...warming, sync: { ...warming.sync, state: "" } };
    expect(hasKubernetesPublication(malformed)).toBe(false);
    const retained = retainKubernetesPublication(committed, malformed);
    expect(retained.items).toEqual(committed.items);
    expect(retained.observed_at).toBe(committed.observed_at);
    expect(hasKubernetesPublication(retained)).toBe(true);
    expect(retainKubernetesPublication(retained, { ...malformed, sync: committed.sync }).items).toEqual([]);
  });

  it("retries only unfinished envelopes and explicitly retryable inventory-not-ready errors", () => {
    const malformed = { ...warming, sync: { ...warming.sync, state: "" } };
    expect(kubernetesReadNeedsRecovery(malformed, null)).toBe(true);
    expect(kubernetesReadNeedsRecovery(warming, null)).toBe(true);
    expect(kubernetesReadNeedsRecovery({ ...warming, sync: committed.sync }, null)).toBe(false);
    expect(kubernetesReadNeedsRecovery(undefined, new ApiError(503, "not ready", { code: "inventory_not_ready", retryable: true }))).toBe(true);
    for (const error of [
      new ApiError(503, "not ready", { code: "inventory_not_ready", retryable: false }),
      new ApiError(503, "storage unavailable"),
      new ApiError(503, "credential unavailable", { code: "credential_unavailable", retryable: true }),
      new ApiError(403, "permission reduced", { code: "inventory_not_ready", retryable: true }),
      new ApiError(401, "signed out"),
    ]) expect(kubernetesReadNeedsRecovery(malformed, error)).toBe(false);
    expect(kubernetesReadNeedsRecovery({ ...malformed, partial_failures: [{ class: "forbidden" }] }, null)).toBe(false);
  });

  it.each(["syncing", ""])("uses same-key committed data through faults, but a denial fences subsequent %s envelopes", async (state) => {
    const cache = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const unfinished = { ...warming, sync: { ...warming.sync, state } };
    let reply: typeof committed | typeof warming | Error = committed;
    const wrapper = ({ children }: { children: ReactNode }) => createElement(QueryClientProvider, { client: cache }, children);
    const { result, rerender } = renderHook(({ scope }) => useKubernetesQuery({ queryKey: ["issues", scope], queryFn: async () => { if (reply instanceof Error) throw reply; return reply; }, retry: false }), { wrapper, initialProps: { scope: "shop" } });
    await waitFor(() => expect(result.current.data?.items).toEqual(["checkout"]));
    reply = unfinished;
    await act(async () => { await result.current.refetch(); });
    expect(result.current.data?.broken.total).toBe(3);
    reply = new ApiError(503, "storage unavailable");
    await act(async () => { await result.current.refetch(); });
    await waitFor(() => expect(result.current.isError).toBe(true));
    expect(result.current.data?.observed_at).toBe(committed.observed_at);
    reply = new ApiError(403, "permission reduced");
    await act(async () => { await result.current.refetch(); });
    await waitFor(() => expect(result.current.data).toBeUndefined());
    expect(cache.getQueryData(["kubernetes", "_", "issues", "shop"])).toBeUndefined();
    reply = unfinished;
    await act(async () => { await result.current.refetch(); });
    expect(result.current.data).toBeUndefined();
    reply = committed;
    await act(async () => { await result.current.refetch(); });
    await waitFor(() => expect(result.current.data?.broken.total).toBe(3));
    reply = unfinished;
    rerender({ scope: "other" });
    await waitFor(() => expect(result.current.isFetching).toBe(false));
    expect(result.current.data).toBeUndefined();
    cache.clear();
  });
});
