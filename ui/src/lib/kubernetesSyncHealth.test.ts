import { describe, expect, it } from "vitest";
import type { KubernetesKindStatus, KubernetesSync } from "./api";
import { clusterSyncEnvelope, firstReadyAt, isZeroTime, kindFailed, kindFailureReason, reportSyncHealth, syncFailureReport, syncHealth, syncHealthStateLabel, syncPollIntervalMs, syncReportIdlePollMs, syncReportPollIntervalMs } from "./kubernetesSyncHealth";

const ready = { state: "ready", age_s: 4, partial: false };
const zero = "0001-01-01T00:00:00Z";
const kind = (overrides: Partial<KubernetesKindStatus> = {}): KubernetesKindStatus => ({ state: "ready", observed_at: "2026-10-26T08:30:00Z", completed_at: "2026-10-26T08:30:01Z", last_attempt_at: "2026-10-26T08:30:00Z", records: 10, partial: false, ...overrides });

describe("Cluster sync health", () => {
  it("does not classify a known credential or publication fault as warming", () => {
    for (const error of ["credential_unavailable", "publication_failed"]) {
      const health = syncHealth({ sync: { state: "syncing", age_s: 100, partial: false }, kinds: { Pod: kind({ state: "warming", partial: true, error }) } });
      expect(health.state).toBe("degraded");
      expect(health.problems[0].error).toBe(error);
      expect(health.problems[0].reason).not.toMatch(/being listed|last sync attempt/);
    }
  });

  it("uses the report fault class over a successful-looking aggregate state", () => {
    const health = reportSyncHealth({ class: "unauthorized", state: "syncing", age_s: 30, partial: false, kinds: {}, total_kinds: 0, ready_kinds: 0, failing_kinds: 0, pending_kinds: 0, truncated: false, first_ready_at: "2026-10-28T00:00:00Z" });
    expect(health.state).toBe("failed");
    expect(health.firstReadyAt).toBe("2026-10-28T00:00:00Z");
  });
  it("is ready when every served kind is ready and clean", () => {
    const health = syncHealth({ sync: ready, kinds: { Pod: kind({ records: 507 }), Deployment: kind() } });
    expect(health.state).toBe("ready");
    expect(health.problems).toEqual([]);
    expect(health.loadingCount).toBe(0);
  });

  it("does not count a kind the cluster does not serve", () => {
    const health = syncHealth({ sync: ready, kinds: { Pod: kind(), Job: kind({ observed_at: zero, records: 0, absent: true }) } });
    expect(health.state).toBe("ready");
    expect(health.problems).toEqual([]);
  });

  it("is preparing while the first sync runs and carries its progress", () => {
    const progress = { step: "listing" as const, step_index: 2, step_total: 4, steps_done: ["connected"], counts: { Pod: 12 }, kinds_done: 1, kinds_total: 4, started_at: "2026-10-26T08:00:00Z", updated_at: "2026-10-26T08:00:04Z" };
    const health = syncHealth({ sync: { state: "syncing", age_s: 0, partial: false, progress } });
    expect(health.state).toBe("preparing");
    expect(health.progress).toBe(progress);
  });

  it("is preparing for a kind that is warming or was never observed, and names the pending types", () => {
    const warming = syncHealth({ sync: { ...ready, state: "partial", partial: true, pending: ["core~v1~configmaps"], pending_kinds: 3 }, kinds: { Pod: kind(), ReplicaSet: kind({ state: "warming", observed_at: zero, records: 0 }) } });
    expect(warming.state).toBe("preparing");
    expect(warming.loading).toEqual(["ConfigMap"]);
    expect(warming.loadingCount).toBe(3);
    expect(syncHealth({ sync: ready, kinds: { Secret: kind({ observed_at: zero, records: 0 }) } }).state).toBe("preparing");
  });

  it("is degraded when a kind dropped records and the rest is current", () => {
    const health = syncHealth({
      sync: { state: "partial", age_s: 0, partial: true },
      kinds: { Pod: kind({ records: 507 }), Node: kind({ state: "partial", partial: true, error: "record_rejected", skipped: 3, records: 12 }) },
    });
    expect(health.state).toBe("degraded");
    expect(health.kinds).toEqual(["Node"]);
    expect(health.problems[0]).toEqual({
      kind: "Node",
      error: "record_rejected",
      reason: "some objects could not be indexed, everything else is current",
      skipped: 3,
      records: 12,
      since: "2026-10-26T08:30:00Z",
      sinceBasis: "observed",
    });
  });

  it("is failed as soon as a kind's listing errored, ahead of any degraded kind", () => {
    const health = syncHealth({
      sync: { state: "partial", age_s: 0, partial: true },
      kinds: {
        Node: kind({ state: "partial", partial: true, error: "record_rejected", skipped: 3, records: 12 }),
        Secret: kind({ state: "error", observed_at: zero, completed_at: zero, records: 0, partial: true, error: "forbidden" }),
      },
    });
    expect(health.state).toBe("failed");
    expect(health.kinds).toEqual(["Node", "Secret"]);
    // The zero time the wire uses for "never observed" is never a failure date:
    // a kind that never listed is dated by its last attempt instead.
    expect(health.problems[1]).toMatchObject({ since: "2026-10-26T08:30:00Z", sinceBasis: "attempt" });
  });

  it("classifies the exact payload the index stream sends", () => {
    const payload = {
      state: "partial", age_s: 0, partial: true,
      kinds: {
        Node: { state: "partial", observed_at: "2026-10-26T08:30:00Z", completed_at: "2026-10-26T08:30:02Z", last_attempt_at: "2026-10-26T08:30:00Z", records: 12, partial: true, error: "record_rejected", skipped: 3 },
        Pod: { state: "ready", observed_at: "2026-10-26T08:30:00Z", completed_at: "2026-10-26T08:30:01Z", last_attempt_at: "2026-10-26T08:30:00Z", records: 507, partial: false },
        Secret: { state: "error", observed_at: zero, completed_at: zero, last_attempt_at: "2026-10-26T08:30:00Z", records: 0, partial: true, error: "forbidden" },
        Job: { state: "ready", observed_at: "2026-10-26T08:30:00Z", completed_at: zero, last_attempt_at: zero, records: 0, partial: false, absent: true },
        ReplicaSet: { state: "warming", observed_at: zero, completed_at: zero, last_attempt_at: "2026-10-26T08:30:00Z", records: 0, partial: false },
      },
    } as unknown as KubernetesSync & { kinds: Record<string, KubernetesKindStatus> };
    // The event payload is the freshness envelope with the per-kind states inside it.
    const health = syncHealth({ sync: payload, kinds: payload.kinds });
    expect(health.state).toBe("failed");
    expect(health.kinds).toEqual(["Node", "Secret"]);
    expect(health.problems.map((problem) => [problem.kind, problem.reason, problem.skipped])).toEqual([
      ["Node", "some objects could not be indexed, everything else is current", 3],
      ["Secret", "permission denied", 0],
    ]);
    expect(health.loading).toEqual([]);
  });

  it("never counts a failing kind as loading, even while other types are still pending", () => {
    const health = syncHealth({
      sync: { state: "error", age_s: 0, partial: true, pending: ["core~v1~configmaps"], pending_kinds: 4 },
      kinds: { Pod: kind(), Node: kind({ state: "error", error: "timeout", records: 0, partial: true }) },
    });
    expect(health.state).toBe("failed");
    expect(health.loading).toEqual([]);
    expect(health.loadingCount).toBe(0);
    expect(health.kinds).toEqual(["Node"]);
  });

  it("is failed when the sync reports an error the per-kind states do not explain", () => {
    expect(syncHealth({ sync: { state: "error", age_s: 0, partial: true } }).state).toBe("failed");
  });

  it("treats a partial kind without a failure class as failing, with a plain-words reason", () => {
    // The index counts a partial kind as failing even when it named no class, so
    // the surface must block and say what it does know rather than read ready.
    const health = syncHealth({ sync: { ...ready, state: "partial", partial: true }, kinds: { Pod: kind({ partial: true, records: 50 }) } });
    expect(health.state).toBe("degraded");
    expect(health.problems).toEqual([{
      kind: "Pod",
      error: "unavailable",
      reason: "it holds only part of what the cluster returned",
      skipped: 0,
      records: 50,
      since: "2026-10-26T08:30:00Z",
      sinceBasis: "observed",
    }]);
  });

  it("reports a stale snapshot as degraded and drops its loading copy", () => {
    const health = syncHealth({ sync: { state: "stale", age_s: 420, partial: true, pending: ["core~v1~configmaps"], pending_kinds: 3 } });
    expect(health.state).toBe("degraded");
    expect(health.staleSeconds).toBe(420);
    expect(health.loading).toEqual([]);
    expect(health.loadingCount).toBe(0);
  });

  it("cannot classify a response without the freshness envelope, so it does not block it", () => {
    expect(syncHealth({}).state).toBe("ready");
  });

  it("words every failure class for an operator", () => {
    expect(kindFailureReason({ error: "record_rejected" })).toBe("some objects could not be indexed, everything else is current");
    expect(kindFailureReason({ error: "record_limit" })).toBe("too many objects for the safety limit");
    expect(kindFailureReason({ error: "incomplete_pages" })).toBe("the listing could not be paged through");
    expect(kindFailureReason({ error: "commit_conflict" })).toBe("credentials rotated mid-sync, retrying");
    expect(kindFailureReason({ error: "forbidden" })).toBe("permission denied");
    expect(kindFailureReason({ error: "unauthorized" })).toBe("cluster credentials are unavailable or rejected");
    expect(kindFailureReason({ error: "timeout" })).toBe("the cluster is slow or rate-limiting reads");
    expect(kindFailureReason({ error: "throttled" })).toBe("the cluster is slow or rate-limiting reads");
    expect(kindFailureReason({ error: "unavailable" })).toBe("the cluster read failed");
    expect(kindFailureReason({ error: "budget_exhausted" })).toBe("the read budget for this sync ran out");
    expect(kindFailureReason({ error: "namespace_scope" })).toBe("the namespace scope could not be read");
    expect(kindFailureReason({ error: "unsupported" })).toBe("the cluster does not support this read");
    expect(kindFailureReason({ error: "not_a_class_this_build_knows" })).toBe("the last sync attempt at this resource type failed");
    expect(kindFailureReason({})).toBe("the last sync attempt at this resource type failed");
  });

  it("treats an errored, a partial, or a classed partial kind as unusable, and nothing else", () => {
    expect(kindFailed(kind())).toBe(false);
    // Rule: partial means the listing did not finish cleanly, so it fails even
    // with no class attached; pending and warming are only preparing.
    expect(kindFailed(kind({ partial: true }))).toBe(true);
    expect(kindFailed(kind({ partial: true, error: "record_rejected" }))).toBe(true);
    expect(kindFailed(kind({ state: "partial" }))).toBe(true);
    expect(kindFailed(kind({ state: "error", error: "timeout" }))).toBe(true);
    // Pending and warming are checked before the partial flag: the wire marks a
    // kind partial while it waits, and a kind on its way is not broken.
    expect(kindFailed(kind({ state: "pending", partial: false }))).toBe(false);
    expect(kindFailed(kind({ state: "pending", partial: true }))).toBe(false);
    expect(kindFailed(kind({ state: "warming", partial: false }))).toBe(false);
    expect(kindFailed(kind({ state: "warming", partial: true }))).toBe(false);
    expect(kindFailed(kind({ absent: true, state: "error", error: "unsupported" }))).toBe(false);
    expect(kindFailed(kind({ absent: true, state: "partial", partial: true }))).toBe(false);
  });

  it("reads the zero time as never observed", () => {
    expect(isZeroTime(zero)).toBe(true);
    expect(isZeroTime(undefined)).toBe(true);
    expect(isZeroTime("not a date")).toBe(true);
    expect(isZeroTime("2026-10-26T08:30:00Z")).toBe(false);
  });

  it("polls quickly during the first sync, slowly while blocked, and not at all once ready", () => {
    expect(syncPollIntervalMs({ state: "syncing", age_s: 0, partial: false })).toBe(1000);
    expect(syncPollIntervalMs({ state: "error", age_s: 0, partial: true })).toBe(5000);
    expect(syncPollIntervalMs({ state: "stale", age_s: 400, partial: true })).toBe(5000);
    expect(syncPollIntervalMs({ state: "partial", age_s: 0, partial: true })).toBe(5000);
    expect(syncPollIntervalMs({ ...ready, pending_kinds: 2 })).toBe(5000);
    expect(syncPollIntervalMs(ready)).toBe(false);
    expect(syncPollIntervalMs(undefined)).toBe(false);
  });

  it("always rechecks the cluster-wide report, so a later failure or a recovery needs no user action", () => {
    // Ready clusters are rechecked too: a kind can start failing after it was
    // healthy, and only the report can show it.
    expect(syncReportPollIntervalMs(ready)).toBe(15_000);
    expect(syncReportPollIntervalMs(undefined)).toBe(15_000);
    expect(syncReportPollIntervalMs({ state: "syncing", age_s: 0, partial: false })).toBe(1000);
    for (const state of ["partial", "stale", "error"]) expect(syncReportPollIntervalMs({ state, age_s: 0, partial: true })).toBe(5000);
    for (const sync of [ready, { state: "error", age_s: 0, partial: true }, { state: "syncing", age_s: 0, partial: false }]) {
      const interval = syncReportPollIntervalMs(sync);
      expect(interval).toBeGreaterThan(0);
      expect(interval).toBeLessThanOrEqual(syncReportIdlePollMs);
    }
  });

  it("names the four classified states with one set of words", () => {
    expect(syncHealthStateLabel("ready")).toBe("Ready");
    expect(syncHealthStateLabel("preparing")).toBe("Preparing");
    expect(syncHealthStateLabel("degraded")).toBe("Degraded");
    expect(syncHealthStateLabel("failed")).toBe("Failed");
  });

  it("reads the cluster-wide report into the aggregate envelope every consumer already uses", () => {
    expect(clusterSyncEnvelope(undefined)).toBeUndefined();
    expect(clusterSyncEnvelope({ state: "error", partial: true, age_s: 14, kinds: {}, total_kinds: 12, ready_kinds: 10, failing_kinds: 2, pending_kinds: 0, truncated: false }))
      .toEqual({ state: "error", age_s: 14, partial: true, pending_kinds: undefined });
    expect(clusterSyncEnvelope({ state: "syncing", partial: false, age_s: 0, kinds: {}, total_kinds: 12, ready_kinds: 4, failing_kinds: 0, pending_kinds: 8, truncated: false })?.pending_kinds).toBe(8);
  });

  it("counts a known kind with no listing yet as preparing, never as ready", () => {
    const health = syncHealth({ sync: ready, kinds: { Pod: kind(), Secret: kind({ state: "pending", observed_at: zero, completed_at: zero, records: 0 }) } });
    expect(health.state).toBe("preparing");
    expect(health.problems).toEqual([]);
  });

  // The wire a first sync really sends: every known kind pending with
  // partial:false, aggregate syncing. A full kinds map is listing, not data.
  it("reads the real first-sync payload as preparing, with nothing named as failing", () => {
    const known = ["ClusterRole", "ConfigMap", "CronJob", "DaemonSet", "Deployment", "Event", "Gateway", "HTTPRoute", "HorizontalPodAutoscaler", "Ingress", "Job", "Namespace", "Node", "PersistentVolumeClaim", "Pod", "ReplicaSet", "Secret", "Service", "ServiceAccount", "StatefulSet"];
    const kinds = Object.fromEntries(known.map((name) => [name, kind({ state: "pending", observed_at: zero, completed_at: zero, last_attempt_at: zero, records: 0, partial: false })]));
    const health = syncHealth({ sync: { state: "syncing", age_s: 0, partial: false, pending_kinds: known.length }, kinds });
    expect(health.state).toBe("preparing");
    expect(health.problems).toEqual([]);
    expect(health.kinds).toEqual([]);
    expect(health.loadingCount).toBe(20);
  });

  // Defence in depth: the wire never marks a waiting kind partial any more, but
  // if it ever did again the first sync must still not read as degraded.
  it("still reads a waiting kind as preparing even if a payload marks it partial", () => {
    const health = syncHealth({
      sync: { state: "syncing", age_s: 0, partial: true, pending_kinds: 1 },
      kinds: {
        Pod: kind({ records: 507 }),
        Node: kind({ state: "pending", observed_at: zero, completed_at: zero, last_attempt_at: zero, records: 0, partial: true }),
      },
    });
    expect(health.state).toBe("preparing");
    expect(health.problems).toEqual([]);
    expect(health.kinds).toEqual([]);
  });

  it("classifies one ready kind beside one still to be listed as preparing", () => {
    const health = syncHealth({
      sync: { state: "syncing", age_s: 0, partial: false, pending_kinds: 1 },
      kinds: {
        Pod: kind({ records: 507 }),
        Node: kind({ state: "pending", observed_at: zero, completed_at: zero, last_attempt_at: zero, records: 0, partial: false }),
      },
    });
    expect(health.state).toBe("preparing");
    expect(health.problems).toEqual([]);
    expect(health.kinds).toEqual([]);
  });

  it("still fails a partial kind that is actually served", () => {
    const health = syncHealth({
      sync: { state: "partial", age_s: 0, partial: true },
      kinds: { Pod: kind({ records: 507 }), Node: kind({ state: "partial", partial: true, records: 12 }) },
    });
    expect(health.state).toBe("degraded");
    expect(health.kinds).toEqual(["Node"]);
  });

  // QA-197: a kind the connector identity may not read is a capability gap, not
  // a sync fault, so it must never close a view or join the failing rows.
  it("keeps a cluster ready, and names the kinds it may not read, when only permissions are missing", () => {
    const health = syncHealth({
      sync: ready,
      kinds: {
        Pod: kind({ records: 507 }),
        // The wire keeps an unreadable kind in the map with its error state.
        Secret: kind({ state: "error", error: "forbidden", partial: true, records: 0 }),
        ServiceAccount: kind({ state: "error", error: "forbidden", partial: true, records: 0 }),
      },
      unreadable: [{ kind: "Secret", class: "forbidden" }, { kind: "ServiceAccount", class: "forbidden" }],
    });
    expect(health.state).toBe("ready");
    expect(health.problems).toEqual([]);
    expect(health.kinds).toEqual([]);
    expect(health.unreadable).toEqual([
      { kind: "Secret", error: "forbidden", reason: "permission denied" },
      { kind: "ServiceAccount", error: "forbidden", reason: "permission denied" },
    ]);
  });

  it("stays failed for a real sync fault even when unrelated kinds are unreadable", () => {
    const health = syncHealth({
      sync: { state: "error", age_s: 0, partial: true },
      kinds: {
        Pod: kind({ state: "error", error: "timeout", partial: true, records: 0 }),
        Secret: kind({ state: "error", error: "forbidden", partial: true, records: 0 }),
      },
      unreadable: [{ kind: "Secret", class: "forbidden" }],
    });
    expect(health.state).toBe("failed");
    expect(health.kinds).toEqual(["Pod"]);
    expect(health.problems.map((problem) => problem.reason)).toEqual(["the cluster is slow or rate-limiting reads"]);
    expect(health.unreadable.map((entry) => entry.kind)).toEqual(["Secret"]);
  });

  it("reports no unreadable kinds when every kind is readable", () => {
    expect(syncHealth({ sync: ready, kinds: { Pod: kind() } }).unreadable).toEqual([]);
    expect(syncHealth({ sync: ready, kinds: { Pod: kind() }, unreadable: [] }).unreadable).toEqual([]);
  });

  // The per-kind time is authoritative: it is never bounded away, and it does
  // not move with each retry.
  it("dates a failure from the kind's own failing_since, ahead of a bounded failing row", () => {
    const health = syncHealth({
      sync: { state: "error", age_s: 0, partial: true },
      kinds: { Secret: kind({ state: "error", error: "forbidden", partial: true, records: 0, observed_at: zero, failing_since: "2026-10-26T11:18:18Z" }) },
      failing: [{ kind: "Secret", state: "error", class: "forbidden", records: 0, since: "2026-10-26T11:40:00Z" }],
    });
    expect(health.problems[0].since).toBe("2026-10-26T11:18:18Z");
  });

  it("dates a failure from the report's own since, and never renders a zero time as a date", () => {
    const health = syncHealth({
      sync: { state: "error", age_s: 0, partial: true },
      kinds: { Secret: kind({ state: "error", error: "forbidden", partial: true, records: 0, observed_at: zero, last_attempt_at: "2026-10-26T10:59:00Z" }) },
      failing: [{ kind: "Secret", state: "error", class: "forbidden", records: 0, since: "2026-10-26T10:49:24Z", observed_at: zero }],
    });
    // The stable first-failure time wins over the latest retry.
    expect(health.problems[0].since).toBe("2026-10-26T10:49:24Z");
    expect(isZeroTime(health.problems[0].since)).toBe(false);
  });

  it("falls back to the retry time when the report carries no failure time", () => {
    const health = syncHealth({
      sync: { state: "error", age_s: 0, partial: true },
      kinds: { Secret: kind({ state: "error", error: "forbidden", partial: true, records: 0, observed_at: zero, last_attempt_at: "2026-10-26T10:59:00Z" }) },
      failing: [{ kind: "Secret", state: "error", class: "forbidden", records: 0, since: zero }],
    });
    expect(isZeroTime(health.problems[0].since)).toBe(false);
    expect(health.problems[0].sinceBasis).toBe("attempt");
  });

  it("reports a partial aggregate that names no kind as degraded rather than ready", () => {
    expect(syncHealth({ sync: { state: "partial", age_s: 0, partial: true }, kinds: {} }).state).toBe("degraded");
  });

  // K22 S4: "has this cluster ever published" is a durable property of the
  // cluster, so it survives any current state and any restart.
  it("carries the first publication through every state, and never invents one", () => {
    const published = "2026-10-26T08:00:00Z";
    expect(firstReadyAt({ first_ready_at: published })).toBe(published);
    // Absent and the wire's zero time both mean "never", and neither is a date.
    expect(firstReadyAt({})).toBeUndefined();
    expect(firstReadyAt({ first_ready_at: zero })).toBeUndefined();

    for (const state of ["syncing", "partial", "stale", "error", "ready"]) {
      const health = reportSyncHealth({ state, partial: state !== "ready" && state !== "syncing", age_s: 12, kinds: {}, total_kinds: 0, ready_kinds: 0, failing_kinds: 0, pending_kinds: 0, truncated: false, first_ready_at: published });
      expect(health.firstReadyAt, state).toBe(published);
      // The age travels with it, so a strip can label what it keeps on screen.
      expect(health.ageSeconds, state).toBe(12);
    }
  });

  it("never lets a classification decide the cluster has published when the report is silent", () => {
    const health = reportSyncHealth({ state: "error", partial: true, age_s: 0, kinds: { Node: { state: "error", error: "forbidden", records: 0, partial: true, observed_at: zero, last_attempt_at: zero } }, total_kinds: 1, ready_kinds: 0, failing_kinds: 1, pending_kinds: 0, truncated: false });
    expect(health.firstReadyAt).toBeUndefined();
    expect(health.state).toBe("failed");
  });

  // A dead credential makes the report unreadable, and the last report that
  // succeeded would otherwise keep the cluster looking ready while every read
  // answers 502. The cluster's own first publication and the age of what is on
  // screen are still knowable, so the failure can be named over the data.
  it("reads the durable facts a failed sync-health answer still carries", () => {
    const published = "2026-10-26T08:00:00Z";
    expect(syncFailureReport({ error: "credential_revoked", first_ready_at: published, age_s: 240 })).toEqual({ firstReadyAt: published, ageSeconds: 240 });
    // The payload may be nested under the error; the same facts are meant.
    expect(syncFailureReport({ error: { message: "credential revoked", first_ready_at: published, age_s: 30 } })).toEqual({ firstReadyAt: published, ageSeconds: 30 });
    // A zero time is not a publication, and a value that is not a count of
    // seconds is not an age, so neither is presented as one.
    expect(syncFailureReport({ error: "x", first_ready_at: zero, age_s: "old" })).toEqual({});
    expect(syncFailureReport({ error: "x", age_s: -5 })).toEqual({});
    expect(syncFailureReport(undefined)).toEqual({});
    expect(syncFailureReport("bad gateway")).toEqual({});
  });
});

it("prefers authoritative progress to an older overview progress envelope", () => {
 const progress = { step: "listing" as const, step_index: 2, step_total: 4, steps_done: ["connected", "building"], counts: { Pod: 12 }, kinds_done: 12, kinds_total: 20, started_at: "2026-10-27T08:00:00Z", updated_at: "2026-10-27T08:00:16Z" };
 const report = { state: "syncing", partial: false, age_s: 16, kinds: { Pod: kind() }, ready_kinds: 12, pending_kinds: 8, total_kinds: 20, failing_kinds: 0, truncated: false, progress };
 const health = reportSyncHealth(report, { ...progress, kinds_done: 1 });
 expect(health.progress).toBe(progress);
 expect(health.loadingCount).toBe(8);
 expect(health.firstReadyAt).toBeUndefined();
});
