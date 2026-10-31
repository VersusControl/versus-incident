import { describe, expect, it, vi } from "vitest";
import { ApiError, type KubernetesCrashEvent, type KubernetesCrashPage } from "./api";
import { focusedChanges, focusedCrashes, focusedHistoryRequests } from "./kubernetesFocusedHistory";

const from = "2026-10-14T12:00:00Z";
const to = "2026-10-14T12:10:00Z";
const sync = { state: "ready", age_s: 0, partial: false };
const event: KubernetesCrashEvent = { id: "termination", at: from, sequence: 1, cluster: "east", namespace: "shop", pod: "api-0", pod_uid: "pod", container: "api", init: false, root_uid: "root", root_kind: "StatefulSet", root_name: "api", observed_at: from, type: "termination", time_source: "finished_at", restart_count: 1 };
const page: KubernetesCrashPage = { items: [event], since: from, until: to, retention_from: from, retention_basis: "capture", started_at: from, gaps: [], capture_gaps: [], coverage: { Pod: { from, to } }, limitations: [], capture: { mode: "continuous", behind: false }, sync };

describe("Focused history adversarial boundaries", () => {
  it("does not publish an inverted coverage interval when admitted pages have disjoint coverage", async () => {
    const fetch = vi.fn<(cursor?: string) => Promise<KubernetesCrashPage>>()
      .mockResolvedValueOnce({ ...page, next: "next" })
      .mockResolvedValueOnce({ ...page, coverage: { Pod: { from: "2026-10-14T12:20:00Z", to: "2026-10-14T12:30:00Z" } } });
    const result = await focusedCrashes(fetch, new AbortController().signal);
    expect(fetch).toHaveBeenCalledTimes(2);
    expect(result.coverage).toEqual({});
    expect(result.items).toEqual([event]);
  });

  it.each([
    { from: "invalid", to },
    { from: to, to: from },
  ])("drops malformed coverage on a single admitted page: %j", async coverage => {
    const result = await focusedCrashes(async () => ({ ...page, coverage: { Pod: coverage } }), new AbortController().signal);
    expect(result.coverage).toEqual({});
    expect(result.items).toEqual([event]);
  });

  it("retains only the common covered interval without manufacturing a union", async () => {
    const fetch = vi.fn<(cursor?: string) => Promise<KubernetesCrashPage>>()
      .mockResolvedValueOnce({ ...page, next: "next" })
      .mockResolvedValueOnce({ ...page, coverage: { Pod: { from: "2026-10-14T12:05:00Z", to: "2026-10-14T12:15:00Z" } } });
    const result = await focusedCrashes(fetch, new AbortController().signal);
    expect(result.coverage).toEqual({ Pod: { from: "2026-10-14T12:05:00.000Z", to: "2026-10-14T12:10:00.000Z" } });
    expect(result.items).toEqual([event]);
  });

  it("detects a multi-token crash continuation cycle through empty pages without erasing gaps", async () => {
    const gap = { from, to };
    const fetch = vi.fn<(cursor?: string) => Promise<KubernetesCrashPage>>()
      .mockResolvedValueOnce({ ...page, next: "a+/=" })
      .mockResolvedValueOnce({ ...page, items: [], next: "b+/=", capture_gaps: [gap] })
      .mockResolvedValueOnce({ ...page, items: [], next: "a+/=" });
    const result = await focusedCrashes(fetch, new AbortController().signal);
    expect(fetch.mock.calls.map(([cursor]) => cursor)).toEqual([undefined, "a+/=", "b+/="]);
    expect(result).toMatchObject({ items: [event], incomplete_range: true, capture_gaps: [gap] });
  });

  it.each([250, 251])("distinguishes the crash display boundary at %i unique observations", async (count) => {
    const fetch = vi.fn(async () => ({ ...page, items: Array.from({ length: count }, (_, i) => ({ ...event, id: String(i) })) }));
    const result = await focusedCrashes(fetch, new AbortController().signal);
    expect(result.items).toHaveLength(Math.min(count, 250));
    expect(result.crash_count).toBe(Math.min(count, 250));
    expect(result.incomplete_range).toBe(count > 250);
    expect(fetch).toHaveBeenCalledTimes(1);
  });

  it("exhausts the crash request budget even when every intermediate page is empty", async () => {
    const fetch = vi.fn(async (cursor?: string) => ({ ...page, items: [], next: `${cursor ?? ""}x` }));
    const result = await focusedCrashes(fetch, new AbortController().signal);
    expect(fetch).toHaveBeenCalledTimes(focusedHistoryRequests);
    expect(result).toMatchObject({ items: [], crash_count: 0, incomplete_range: true });
  });

  it("propagates typed credential loss on a later successful chain rather than retaining authorized-looking crashes", async () => {
    const fetch = vi.fn<(cursor?: string) => Promise<KubernetesCrashPage>>()
      .mockResolvedValueOnce({ ...page, next: "next" })
      .mockRejectedValueOnce(new ApiError(503, "credential retired", { code: "credential_unavailable", retryable: false }));
    await expect(focusedCrashes(fetch, new AbortController().signal)).rejects.toMatchObject({ status: 503 });
    expect(fetch).toHaveBeenCalledTimes(2);
  });

  it("keeps truncation from an empty terminal changes page instead of claiming full evidence", async () => {
    const fetch = vi.fn()
      .mockResolvedValueOnce({ items: [], next: "next", sync })
      .mockResolvedValueOnce({ items: [], truncated: true, sync });
    const result = await focusedChanges(fetch, new AbortController().signal);
    expect(fetch).toHaveBeenCalledTimes(2);
    expect(result.truncated).toBe(true);
  });

  it("rejects an obsolete second-page callback after a replacement root has already loaded", async () => {
    const controller = new AbortController();
    let finish!: (value: KubernetesCrashPage) => void;
    const oldFetch = vi.fn<(cursor?: string) => Promise<KubernetesCrashPage>>()
      .mockResolvedValueOnce({ ...page, next: "old-next" })
      .mockImplementationOnce(() => new Promise(resolve => { finish = resolve; }));
    const obsolete = focusedCrashes(oldFetch, controller.signal);
    const rejection = expect(obsolete).rejects.toMatchObject({ name: "AbortError" });
    await vi.waitFor(() => expect(oldFetch).toHaveBeenCalledTimes(2));
    controller.abort();
    const replacement = { ...event, id: "replacement-event", root_uid: "replacement-root", namespace: "other" };
    const newFetch = vi.fn(async () => ({ ...page, items: [replacement] }));
    const current = await focusedCrashes(newFetch, new AbortController().signal);
    finish({ ...page, next: "must-not-read", items: [event] });
    await rejection;
    expect(oldFetch).toHaveBeenCalledTimes(2);
    expect(current.items).toEqual([replacement]);
    expect(newFetch).toHaveBeenCalledTimes(1);
  });
});
