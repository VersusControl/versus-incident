import { describe, expect, it, vi } from "vitest";
import { ApiError, type KubernetesChange, type KubernetesChangesPage, type KubernetesCrashEvent, type KubernetesCrashPage } from "./api";
import { focusedChanges, focusedCrashes, focusedHistoryRequests } from "./kubernetesFocusedHistory";

const sync = { state: "ready", age_s: 0, partial: false };
const change: KubernetesChange = { id: "image", at: "2026-10-14T12:00:00Z", cluster: "east", namespace: "shop", kind: "Deployment", name: "checkout", uid: "root", type: "image_changed", source: "capture" };
const event: KubernetesCrashEvent = { id: "crash", at: change.at, sequence: 1, cluster: "east", namespace: "shop", pod: "checkout", pod_uid: "pod", container: "api", init: false, root_uid: "root", root_kind: "Deployment", root_name: "checkout", observed_at: change.at, type: "termination", time_source: "finished_at", restart_count: 1 };
const crashPage: KubernetesCrashPage = { items: [event], since: change.at, until: change.at, retention_from: change.at, retention_basis: "capture", started_at: change.at, gaps: [], capture_gaps: [], coverage: { Pod: { from: change.at, to: change.at } }, limitations: [], capture: { mode: "continuous", behind: false }, sync };

describe("Focused history continuation", () => {
  it("follows an empty intermediate page, deduplicates stable IDs and retains every gap and failure", async () => {
    const gap = { from: "2026-10-14T11:00:00Z", to: change.at };
    const fetch = vi.fn<(cursor?: string) => Promise<KubernetesChangesPage>>()
      .mockResolvedValueOnce({ items: [change], next: "a+/=", sync, gaps: [gap] })
      .mockResolvedValueOnce({ items: [], next: "b+/=", sync, partial_failures: [{ class: "storage_unavailable" }] })
      .mockResolvedValueOnce({ items: [change, { ...change, id: "older", at: "2026-10-14T11:59:00Z" }], sync, gaps: [gap] });
    const result = await focusedChanges(fetch, new AbortController().signal);
    expect(fetch.mock.calls.map(([cursor]) => cursor)).toEqual([undefined, "a+/=", "b+/="]);
    expect(result.items.map(item => item.id)).toEqual(["older", "image"]);
    expect(result.gaps).toEqual([gap]);
    expect(result.partial_failures).toEqual([{ class: "storage_unavailable" }]);
    expect(result.incomplete_range).toBe(false);
    expect("pages" in result).toBe(false);
  });

  it("bounds empty continuations by requests and repeated opaque cursors", async () => {
    const endless = vi.fn(async (cursor?: string) => ({ items: [], next: `${cursor ?? ""}x`, sync }));
    expect((await focusedChanges(endless, new AbortController().signal)).incomplete_range).toBe(true);
    expect(endless).toHaveBeenCalledTimes(focusedHistoryRequests);
    const repeated = vi.fn(async () => ({ items: [change], next: "repeat", sync }));
    const result = await focusedChanges(repeated, new AbortController().signal);
    expect(repeated).toHaveBeenCalledTimes(2);
    expect(result.items).toHaveLength(1);
    expect(result.incomplete_range).toBe(true);
  });

  it("bounds rendered evidence even if a transport returns too many records", async () => {
    const fetch = vi.fn(async () => ({ items: Array.from({ length: 501 }, (_, i) => ({ ...change, id: String(i) })), sync }));
    const result = await focusedChanges(fetch, new AbortController().signal);
    expect(result.items).toHaveLength(500);
    expect(result.incomplete_range).toBe(true);
    expect(fetch).toHaveBeenCalledTimes(1);
  });

  it("retains admitted evidence on continuation failure but never hides authority loss", async () => {
    const transport = vi.fn<(cursor?: string) => Promise<KubernetesChangesPage>>()
      .mockResolvedValueOnce({ items: [change], next: "next", sync })
      .mockRejectedValueOnce(new ApiError(503, "unavailable"));
    expect(await focusedChanges(transport, new AbortController().signal)).toMatchObject({ items: [change], incomplete_range: true, continuation_failed: true });
    for (const status of [401, 403, 404]) {
      const denied = vi.fn<(cursor?: string) => Promise<KubernetesChangesPage>>()
        .mockResolvedValueOnce({ items: [change], next: "next", sync })
        .mockRejectedValueOnce(new ApiError(status, "denied"));
      await expect(focusedChanges(denied, new AbortController().signal)).rejects.toMatchObject({ status });
    }
  });

  it("scrubs late ignored-abort responses and does not continue the obsolete chain", async () => {
    const controller = new AbortController();
    let finish!: (value: KubernetesChangesPage) => void;
    const fetch = vi.fn(() => new Promise<KubernetesChangesPage>(resolve => { finish = resolve; }));
    const result = focusedChanges(fetch, controller.signal);
    controller.abort();
    finish({ items: [change], next: "next", sync });
    await expect(result).rejects.toMatchObject({ name: "AbortError" });
    expect(fetch).toHaveBeenCalledTimes(1);
  });

  it("merges crashes through empty pages with conservative capture coverage", async () => {
    const gap = { from: change.at, to: "2026-10-14T12:01:00Z" };
    const fetch = vi.fn<(cursor?: string) => Promise<KubernetesCrashPage>>()
      .mockResolvedValueOnce({ ...crashPage, next: "a", capture_gaps: [gap] })
      .mockResolvedValueOnce({ ...crashPage, items: [], next: "b", coverage: {}, limitations: ["outside_retention"] })
      .mockResolvedValueOnce({ ...crashPage, items: [event, { ...event, id: "second" }], capture: { mode: "continuous", behind: true } });
    const result = await focusedCrashes(fetch, new AbortController().signal);
    expect(result.items.map(item => item.id)).toEqual(["crash", "second"]);
    expect(result.coverage).toEqual({});
    expect(result.capture_gaps).toEqual([gap]);
    expect(result.limitations).toEqual(["outside_retention"]);
    expect(result.capture.behind).toBe(true);
    expect(result.crash_count).toBe(2);
    expect(result.incomplete_range).toBe(false);
  });

  it("keeps a later-page capture failure rather than presenting first-page coverage as complete", async () => {
    const fetch = vi.fn<(cursor?: string) => Promise<KubernetesCrashPage>>()
      .mockResolvedValueOnce({ ...crashPage, next: "next" })
      .mockResolvedValueOnce({ ...crashPage, capture: { mode: "continuous", behind: false, evidence_error: "storage_unavailable" } });
    const result = await focusedCrashes(fetch, new AbortController().signal);
    expect(result.incomplete_range).toBe(true);
    expect(result.capture.evidence_error).toBe("storage_unavailable");
    expect(result.items).toHaveLength(1);
  });
});
