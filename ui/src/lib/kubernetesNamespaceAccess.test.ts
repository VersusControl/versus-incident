import { describe, expect, it } from "vitest";
import { ApiError, type KubernetesSyncHealthReport } from "./api";
import { allNamespacesLabel, connectorSetupDetail, helmAccessDenial, historyOff, historyOffFor, metricsApiMissing, metricsState, namespaceAccess, namespaceAccessLabel } from "./kubernetesNamespaceAccess";
import { reportSyncHealth } from "./kubernetesSyncHealth";
import { failureWarnings, timelineWarnings } from "@/pages/kubernetesWarningItems";

const observed = "2026-11-14T09:00:00Z";
const scoped = { state: "partial" as const, observed_at: observed, records: 4, partial: true, error: "namespace_scope" };
const report = (overrides: Partial<KubernetesSyncHealthReport> = {}): KubernetesSyncHealthReport => ({
  state: "partial", partial: true, age_s: 10, first_ready_at: observed, kinds: { Pod: scoped, Deployment: scoped },
  total_kinds: 2, ready_kinds: 0, failing_kinds: 2, pending_kinds: 0, truncated: false, ...overrides,
});

describe("namespace access", () => {
  it("reads cluster-wide unless the server says false", () => {
    expect(namespaceAccess(undefined)).toEqual({ clusterWide: true, namespaces: [] });
    expect(namespaceAccess({})).toEqual({ clusterWide: true, namespaces: [] });
    expect(namespaceAccess({ cluster_wide: true, readable_namespaces: ["shop"] })).toEqual({ clusterWide: true, namespaces: [] });
    expect(namespaceAccess({ cluster_wide: false, readable_namespaces: ["shop", "payments", "shop", ""] })).toEqual({ clusterWide: false, namespaces: ["payments", "shop"] });
    // The first source that says anything decides; a later one only fills in.
    expect(namespaceAccess({}, { cluster_wide: false, readable_namespaces: ["shop"] })).toEqual({ clusterWide: false, namespaces: ["shop"] });
  });

  it("words the steady status and the empty namespace choice", () => {
    expect(namespaceAccessLabel(["payments", "shop"])).toBe("Namespace access — showing payments, shop");
    expect(namespaceAccessLabel(["a", "b", "c", "d", "e"])).toBe("Namespace access — showing a, b, c and 2 more");
    expect(allNamespacesLabel({ clusterWide: true, namespaces: [] })).toBe("All namespaces");
    expect(allNamespacesLabel({ clusterWide: false, namespaces: ["shop"] })).toBe("All readable namespaces");
  });
});

describe("sync health for a namespace-only identity", () => {
  it("is ready, not degraded, when only namespace-scoped reads hold the sync partial", () => {
    const health = reportSyncHealth(report({ cluster_wide: false, readable_namespaces: ["shop", "payments"] }));
    expect(health.state).toBe("ready");
    expect(health.problems).toEqual([]);
    expect(health.readableNamespaces).toEqual(["payments", "shop"]);
  });

  it("still names a real fault next to the namespace scope", () => {
    const health = reportSyncHealth(report({ cluster_wide: false, readable_namespaces: ["shop"], kinds: { Pod: scoped, Event: { state: "error", observed_at: observed, records: 0, partial: true, error: "timeout" } } }));
    expect(health.state).toBe("failed");
    expect(health.problems.map((problem) => problem.kind)).toEqual(["Event"]);
  });

  it("names a namespace-scoped kind as a gap, never a blocking fault, for a cluster-wide identity", () => {
    const health = reportSyncHealth(report({ cluster_wide: true, kinds: { Pod: { ...scoped, state: "ready", partial: false, error: undefined }, Secret: scoped } }));
    expect(health.state).toBe("ready");
    expect(health.problems).toEqual([]);
    expect(health.namespaceScoped).toEqual([{ kind: "Secret", error: "namespace_scope", reason: "readable only in some namespaces" }]);
    expect(health.unreadable).toEqual([]);
    expect(health.readableNamespaces).toBeUndefined();
  });

  it("uses the report's namespace_scoped rows when it sends them", () => {
    const health = reportSyncHealth(report({ state: "ready", partial: false, cluster_wide: true, kinds: { Pod: { ...scoped, state: "ready", partial: false, error: undefined } },
      namespace_scoped: [{ kind: "Secret", state: "ready", class: "namespace_scope", records: 2 }], namespace_scoped_kinds: 1 }));
    expect(health.state).toBe("ready");
    expect(health.namespaceScoped?.map((row) => row.kind)).toEqual(["Secret"]);
  });
});

describe("history off", () => {
  const denied = new ApiError(403, "denied", { code: "history_authority_denied" });
  it("follows the server's history marker", () => {
    expect(historyOff({ history: "off_namespace_access", items: [] })).toBe(true);
    expect(historyOff({ items: [] })).toBe(false);
    expect(historyOff(undefined)).toBe(false);
  });

  it("reads a refused history read as off only for a namespace-only identity", () => {
    expect(historyOffFor({ clusterWide: false, namespaces: ["shop"] }, undefined, denied)).toBe(true);
    expect(historyOffFor({ clusterWide: true, namespaces: [] }, undefined, denied)).toBe(false);
    expect(historyOffFor({ clusterWide: false, namespaces: ["shop"] }, undefined, new ApiError(503, "busy", { code: "history_store_timeout" }))).toBe(false);
  });

  it("raises no incomplete-history warning for a page that is off by design", () => {
    const off = { items: [], gaps: [], history: "off_namespace_access", sync: { state: "partial", age_s: 0, partial: true } };
    expect(timelineWarnings(undefined, off)).toEqual([]);
    expect(timelineWarnings(undefined, { ...off, history: undefined }).map((warning) => warning.id)).toContain("changes-partial");
  });
});

describe("Helm access", () => {
  it("prefers the server's sentence and link", () => {
    expect(helmAccessDenial({ items: [], access: "needs_secret_list", access_message: "Helm needs Secrets.", docs_url: "https://docs.versusincident.com/#/x" }))
      .toEqual({ message: "Helm needs Secrets.", docsUrl: "https://docs.versusincident.com/#/x" });
  });

  it("refuses a link that is not https", () => {
    expect(helmAccessDenial({ access: "needs_secret_list", docs_url: "javascript:alert(1)" })?.docsUrl).toMatch(/^https:\/\/docs\.versusincident\.com\//);
  });

  it("reads the refused-read code too, and nothing else", () => {
    expect(helmAccessDenial(undefined, new ApiError(403, "Helm releases need list access to Secrets.", { code: "helm_needs_secret_list" }))?.message).toBe("Helm releases need list access to Secrets.");
    expect(helmAccessDenial(undefined, new ApiError(403, "denied", { code: "cluster_permission_denied" }))).toBeUndefined();
    expect(helmAccessDenial({ items: [] })).toBeUndefined();
  });
});

describe("connector setup detail", () => {
  it("returns the server's message and action for a build or address fault", () => {
    const invalid = new ApiError(503, "Kubeconfig exec credentials are not supported.", { code: "connector_configuration_invalid", action: "Use auth.mode eks, aks or gke." });
    expect(connectorSetupDetail(undefined, invalid)).toEqual({ code: "connector_configuration_invalid", message: "Kubeconfig exec credentials are not supported.", action: "Use auth.mode eks, aks or gke." });
    const refused = new ApiError(503, "Versus refused 10.1.2.3.", { code: "connector_endpoint_refused", action: "Set allow_private_networks." });
    expect(connectorSetupDetail(refused)?.code).toBe("connector_endpoint_refused");
  });

  it("ignores every other fault", () => {
    expect(connectorSetupDetail(new ApiError(503, "x", { code: "connector_unreachable", action: "y" }), new Error("z"))).toBeUndefined();
  });
});

describe("coverage reasons", () => {
  it("shows the server's sentence and keeps the server's order", () => {
    const text = failureWarnings([
      { resource_id: "core~v1~events", class: "timeout", kind: "Event", message: "Event took too long to read; the next sync tries again." },
      { resource_id: "core~v1~nodes", class: "forbidden", kind: "Node", message: "Node is not readable with this connector's permissions." },
    ]).map((warning) => warning.content);
    expect(text).toEqual(["Event took too long to read; the next sync tries again.", "Node is not readable with this connector's permissions."]);
  });

  it("words a failure without a message as a sentence with a display name", () => {
    expect(failureWarnings([{ resource_id: "core~v1~pods", class: "namespace_scope" }])[0].content).toBe("Pod is read only in the namespaces this connector can access.");
    expect(failureWarnings([{ resource_id: "core~v1~nodes", class: "forbidden" }])[0].content).toBe("Node is not readable with this connector's permissions.");
  });
});

describe("metrics API state", () => {
  it("reads only the server's field", () => {
    expect(metricsApiMissing({ metrics: "not_installed" })).toBe(true);
    for (const value of ["forbidden", "unavailable", "syncing", "available"]) expect(metricsApiMissing({ metrics: value })).toBe(false);
    // Without the field nothing is guessed from empty sources.
    expect(metricsApiMissing({ availability: "unavailable", items_available: false })).toBe(false);
    expect(metricsState({ metrics: "syncing" })).toBe("syncing");
    expect(metricsState({ metrics: "bogus" })).toBeUndefined();
  });
});
