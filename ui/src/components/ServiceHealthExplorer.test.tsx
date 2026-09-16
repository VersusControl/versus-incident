// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import type { ServiceHealthCapability, ServiceHealthService, ServiceHealthSignalEvidence, ServiceHealthSnapshot } from "@/lib/api";
import { ServiceHealthExplorer } from "./ServiceHealthExplorer";
import { tabbableCandidates } from "./peekPanelFocus";

afterEach(cleanup);

function service(name: string, overrides: Partial<ServiceHealthService> = {}): ServiceHealthService {
  return {
    org_id: "default", service: name, domain: "", kind: "unknown", severity: "unknown",
    assessment_basis: "Internal data only", active_incidents: null,
    logs: { service: name, source_id: "", matched_logs: 0, unique_patterns: 0, new_patterns: 0, unknown_patterns: 0, spiking_patterns: 0, severity_counts: {}, estimated: false },
    availability: { logs: { state: "not_configured" }, internal: { state: "no_data" } },
    ...overrides,
  };
}

function makeSnapshot(services: ServiceHealthService[], overrides: Partial<ServiceHealthSnapshot> = {}): ServiceHealthSnapshot {
  return {
    snapshot_id: "sample", generated_at: "2026-09-10T12:00:00Z", latest_attempt: "2026-09-10T12:00:00Z",
    next_collection_at: "2026-09-10T12:01:00Z", settings_revision: 1, window_seconds: 300,
    services, domains: [], facets: { nominal: 1, pressure: 1, unknown: 1 },
    coverage: { total_services: services.length, observed_services: services.length, partial: false }, capabilities: [],
    ...overrides,
  };
}

function show(services: ServiceHealthService[], overrides: Partial<ServiceHealthSnapshot> = {}) {
  const snapshot = makeSnapshot(services, overrides);
  return render(<MemoryRouter><ServiceHealthExplorer snapshot={snapshot} /></MemoryRouter>);
}

function evidence(overrides: Partial<ServiceHealthSignalEvidence> = {}): ServiceHealthSignalEvidence {
  return {
    org_id: "acme", service: "checkout", family: "metrics", measure: "latency", value: 245.5, unit: "ms",
    availability: { state: "ready" }, source_ref: "prometheus", signal_ref: "latency_p99",
    observed_at: "2026-09-10T11:59:00Z", window_start: "2026-09-10T11:55:00Z", window_end: "2026-09-10T12:00:00Z",
    fresh_until: "2026-09-10T12:04:00Z", provenance: "learned_latest", ...overrides,
  };
}

const metricCapabilities: ServiceHealthCapability[] = [{ family: "metrics", measures: {
  latency: { state: "ready" }, request_error_ratio: { state: "ready" }, throughput: { state: "ready" },
} }];

describe("ServiceHealthExplorer", () => {
  it("shows ungrouped services directly and never renders missing logs as zero", () => {
    show([service("checkout", { active_incidents: 2 })]);
    const tile = screen.getByRole("button", { name: "Inspect checkout" });
    expect(within(tile).getByText("Not measured")).toBeTruthy();
    expect(within(tile).getByText("2")).toBeTruthy();
    expect(screen.getByRole("heading", { name: "Ungrouped" })).toBeTruthy();
  });

  it("preserves filtering across grid/list changes and orders affected services first", () => {
    show([service("catalog", { severity: "nominal" }), service("checkout", { severity: "pressure" })]);
    expect(screen.getAllByRole("button", { name: /^Inspect / })[0].textContent).toContain("checkout");
    fireEvent.change(screen.getByRole("searchbox"), { target: { value: "check" } });
    fireEvent.click(screen.getByRole("button", { name: "List view" }));
    expect(screen.getByRole("button", { name: "List view" }).getAttribute("aria-pressed")).toBe("true");
    expect(screen.queryByRole("button", { name: "Inspect catalog" })).toBeNull();
    fireEvent.change(screen.getByLabelText("Health state"), { target: { value: "nominal" } });
    expect(screen.getByRole("status").textContent).toContain("No services match");
  });

  it("labels estimates and stale observations without changing impact with evidence mode", () => {
    const stale = service("checkout", { severity: "pressure", availability: { logs: { state: "stale" } } });
    stale.logs = { ...stale.logs, matched_logs: 18, estimated: true, latest_observation: "2026-09-10T11:59:00Z" };
    show([stale]);
    const tile = screen.getByRole("button", { name: "Inspect checkout" });
    expect(within(tile).getByText("~18")).toBeTruthy();
    expect(within(tile).getByText("Logs estimated (stale)")).toBeTruthy();
    expect(tile.textContent).not.toContain("·");
    fireEvent.change(screen.getByLabelText("Show data"), { target: { value: "incidents" } });
    expect(within(tile).getByText("Not assessed")).toBeTruthy();
    expect(within(tile).getByText("Warning")).toBeTruthy();
  });

  it("opens evidence and restores focus on Escape without losing search", () => {
    show([service("checkout", { assessment_basis: "Patterns + incidents" })]);
    fireEvent.change(screen.getByRole("searchbox"), { target: { value: "check" } });
    const tile = screen.getByRole("button", { name: "Inspect checkout" });
    tile.focus();
    fireEvent.click(tile);
    const dialog = screen.getByRole("dialog");
    expect(within(dialog).getByText("Log patterns and incident records")).toBeTruthy();
    expect(within(dialog).queryByText("Patterns + incidents")).toBeNull();
    expect(within(dialog).getByText("Not observed")).toBeTruthy();
    expect(within(dialog).getByRole("link", { name: "Open service" }).getAttribute("href")).toBe("/agent/services/checkout");
    fireEvent.keyDown(document, { key: "Escape" });
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(document.activeElement).toBe(tile);
    expect((screen.getByRole("searchbox") as HTMLInputElement).value).toBe("check");
  });

  it("keeps the drawer summary independent of the selected heatmap mode", () => {
    const checkout = service("checkout", { active_incidents: 0 });
    checkout.logs = { ...checkout.logs, matched_logs: 18, spiking_patterns: 0, latest_observation: "2026-09-10T11:59:00Z" };
    checkout.availability.logs = { state: "ready" };
    show([checkout]);
    fireEvent.change(screen.getByLabelText("Show data"), { target: { value: "incidents" } });
    fireEvent.click(screen.getByRole("button", { name: "Inspect checkout" }));
    const dialog = screen.getByRole("dialog", { name: "checkout" });
    expect(within(dialog).getByText("Log events")).toBeTruthy();
    expect(within(dialog).getByText("18")).toBeTruthy();
    expect(within(dialog).getByText("Active incidents")).toBeTruthy();
    expect(within(dialog).getAllByText("0")).toHaveLength(2);
  });

  it.each([
    ["logs and internal only", [], ["All data", "Logs", "Incidents"]],
    ["metrics only", metricCapabilities, ["All data", "Logs", "Incidents", "Latency P99", "Request error rate", "Throughput"]],
    ["traces only", [{ family: "traces", measures: { latency: { state: "ready" }, request_error_ratio: { state: "unsupported" }, throughput: { state: "collecting" } } }], ["All data", "Logs", "Incidents", "Latency P99", "Request error rate", "Throughput"]],
    ["metrics and traces", [...metricCapabilities, { family: "traces", measures: { latency: { state: "ready" } } }, { family: "assessment", measures: { regression_score: { state: "ready" }, silent: { state: "ready" } } }], ["All data", "Logs", "Incidents", "Latency P99", "Request error rate", "Throughput", "Regression"]],
    ["restricted community", [{ family: "metrics", measures: { latency: { state: "restricted" }, request_error_ratio: { state: "restricted" }, throughput: { state: "restricted" } } }], ["All data", "Logs", "Incidents"]],
  ] as Array<[string, ServiceHealthCapability[], string[]]>)("derives modes for %s capabilities", (_, capabilities, expected) => {
    show([service("checkout")], { capabilities });
    expect(within(screen.getByLabelText("Show data")).getAllByRole("option").map((option) => option.textContent)).toEqual(expected);
    expect(screen.queryByRole("option", { name: /Apdex/i })).toBeNull();
  });

  it("formats P99, ratio, and throughput by known wire units without converting unknown units", () => {
    const checkout = service("checkout", { evidence: [
      evidence(),
      evidence({ measure: "request_error_ratio", value: 0.03125, unit: "ratio", signal_ref: "error_rate" }),
      evidence({ measure: "throughput", value: 42.25, unit: "req/s", signal_ref: "request_rate" }),
    ] });
    show([checkout], { capabilities: metricCapabilities });
    const select = screen.getByLabelText("Show data");
    fireEvent.change(select, { target: { value: "latency" } });
    expect(screen.getByText("245.5 ms")).toBeTruthy();
    fireEvent.change(select, { target: { value: "request_error_ratio" } });
    expect(screen.getByText("3.13%")).toBeTruthy();
    fireEvent.change(select, { target: { value: "throughput" } });
    expect(screen.getByText("42.25 req/s")).toBeTruthy();

    checkout.evidence = [evidence({ unit: "ticks" })];
    cleanup();
    show([checkout], { capabilities: metricCapabilities });
    fireEvent.change(screen.getByLabelText("Show data"), { target: { value: "latency" } });
    expect(screen.getByText("245.5 ticks")).toBeTruthy();
  });

  it.each([
    ["metrics", "insufficient_baseline", "collecting", "Learning baseline"],
    ["traces", "stale_baseline", "stale", "Stale"],
    ["metrics", "timeout", "error", "Source timeout"],
    ["traces", "source_not_configured", "not_configured", "Not connected"],
    ["metrics", "permission", "restricted", "Restricted"],
    ["traces", "no_observations_in_window", "no_data", "No recent data"],
    ["metrics", "unsupported_measure", "unsupported", "Unsupported"],
  ] as const)("uses %s composite availability for %s", (family, reason, state, expected) => {
    show([service("checkout", { availability: { ...service("checkout").availability, [`${family}.latency`]: { state, reason_code: reason } } })], { capabilities: metricCapabilities });
    fireEvent.change(screen.getByLabelText("Show data"), { target: { value: "latency" } });
    expect(screen.getByText(expected)).toBeTruthy();
  });

  it.each([
    ["metrics", "latency"],
    ["traces", "throughput"],
  ] as const)("uses licensed but unconfigured %s capability availability when %s has no service evidence", (family, measure) => {
    show([service("checkout")], { capabilities: [{ family, measures: { [measure]: { state: "not_configured" } } }] });
    fireEvent.change(screen.getByLabelText("Show data"), { target: { value: measure } });
    expect(screen.getByText("Not connected")).toBeTruthy();
    expect(screen.queryByText("No recent data")).toBeNull();
  });

  it.each([
    ["metrics", "insufficient_baseline", "collecting", "Learning baseline"],
    ["traces", "stale_baseline", "stale", "Stale"],
    ["metrics", "timeout", "error", "Source timeout"],
    ["metrics", "no_observations_in_window", "no_data", "No recent data"],
    ["traces", "unsupported_measure", "unsupported", "Unsupported"],
  ] as const)("labels capability-only %s availability for %s as %s", (family, reason, state, expected) => {
    show([service("checkout")], { capabilities: [{ family, measures: { latency: { state, reason_code: reason } } }] });
    fireEvent.change(screen.getByLabelText("Show data"), { target: { value: "latency" } });
    expect(screen.getByText(expected)).toBeTruthy();
  });

  it("renders a restricted capability fallback when another family keeps the measure selectable", () => {
    show([service("checkout")], { capabilities: [
      { family: "metrics", measures: { latency: { state: "restricted", reason_code: "permission" } } },
      { family: "traces", measures: { latency: { state: "unsupported" } } },
    ] });
    fireEvent.change(screen.getByLabelText("Show data"), { target: { value: "latency" } });
    expect(screen.getByText("Restricted")).toBeTruthy();
  });

  it("uses No recent data for capability-ready measures without evidence and preserves another family's failure", () => {
    const checkout = service("checkout");
    const view = show([checkout], { capabilities: [{ family: "metrics", measures: { latency: { state: "ready" } } }] });
    fireEvent.change(screen.getByLabelText("Show data"), { target: { value: "latency" } });
    expect(screen.getByText("No recent data")).toBeTruthy();

    view.rerender(<MemoryRouter><ServiceHealthExplorer snapshot={makeSnapshot([checkout], { capabilities: [
      { family: "metrics", measures: { latency: { state: "ready" } } },
      { family: "traces", measures: { latency: { state: "error", reason_code: "timeout" } } },
    ] })} /></MemoryRouter>);
    expect(screen.getByText("Source timeout")).toBeTruthy();
  });

  it("uses availability priority and metrics as the stable family tie-break for capability-only measures", () => {
    show([service("checkout")], { capabilities: [
      { family: "metrics", measures: { latency: { state: "error", reason_code: "timeout" } } },
      { family: "traces", measures: { latency: { state: "error" } } },
    ] });
    fireEvent.change(screen.getByLabelText("Show data"), { target: { value: "latency" } });
    expect(screen.getByText("Source timeout")).toBeTruthy();
  });

  it("considers metric and trace evidence without conflating sources and operations", () => {
    const capabilities = [...metricCapabilities, { family: "traces", measures: { latency: { state: "ready" as const } } }];
    const checkout = service("checkout", { evidence: [
      evidence({ family: "metrics", source_ref: "prometheus" }),
      evidence({ family: "traces", operation: "POST /pay", source_ref: "signoz_traces" }),
    ] });
    show([checkout], { capabilities });
    fireEvent.change(screen.getByLabelText("Show data"), { target: { value: "latency" } });
    expect(screen.getByText("Multiple sources")).toBeTruthy();
    expect(screen.queryByText("Multiple operations")).toBeNull();
  });

  it("labels multiple providers in one family as multiple sources", () => {
    const checkout = service("checkout", { evidence: [
      evidence({ source_ref: "prometheus-primary" }),
      evidence({ source_ref: "prometheus-secondary", value: 260 }),
    ] });
    show([checkout], { capabilities: metricCapabilities });
    fireEvent.change(screen.getByLabelText("Show data"), { target: { value: "latency" } });
    expect(screen.getByText("Multiple sources")).toBeTruthy();
    expect(screen.queryByText("Multiple operations")).toBeNull();
  });

  it("does not average operation evidence and exposes exact safe rows in the drawer", () => {
    show([service("checkout", { evidence: [
      evidence({ family: "traces", operation: "GET /cart", value: 210, source_ref: "signoz_traces" }),
      evidence({ family: "traces", operation: "POST /pay", value: 480, source_ref: "signoz_traces" }),
    ] })], { capabilities: [{ family: "traces", measures: { latency: { state: "ready" } } }] });
    fireEvent.change(screen.getByLabelText("Show data"), { target: { value: "latency" } });
    expect(screen.getByText("Multiple operations")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Inspect checkout" }));
    const dialog = screen.getByRole("dialog");
    fireEvent.click(within(dialog).getByRole("tab", { name: "Signals" }));
    expect(within(dialog).getByText("GET /cart")).toBeTruthy();
    expect(within(dialog).getByText("POST /pay")).toBeTruthy();
    expect(within(dialog).getAllByText("Signoz Traces")).toHaveLength(2);
    expect(within(dialog).getAllByText("latency_p99")).toHaveLength(2);
    expect(dialog.textContent).not.toContain("acme");
    expect(dialog.textContent).not.toContain("algorithm");
  });

  it.each([
    [0, true, "0 / 100", true],
    [null, false, "Assessment withheld", false],
    [undefined, null, "Low confidence", false],
  ] as const)("keeps score %s and silent %s distinct", (score, silent, expected) => {
    show([service("checkout", { active_incidents: 0, assessment: {
      org_id: "acme", service: "checkout", regression_score: score, regressing: score != null && score >= 50,
      silent, confidence: 0.7842, reason_code: score === undefined ? "low_confidence" : score === null ? "score_withheld" : "no_regression",
      drivers: [{ family: "metrics", measure: "latency", operation: "POST /pay" }],
      algorithm_version: "learned-adverse-z-v1", baseline_reference: "learned:metrics:latency_p99", assessed_at: "2026-09-10T12:00:00Z",
      alert_state_known: true,
    } })], { capabilities: [{ family: "assessment", measures: { regression_score: { state: score == null ? "no_data" : "ready" }, silent: { state: silent == null ? "no_data" : "ready" } } }] });
    fireEvent.change(screen.getByLabelText("Show data"), { target: { value: "regression" } });
    expect(screen.getByText(expected)).toBeTruthy();
    expect(screen.queryByText("No Versus incident")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Inspect checkout" }));
    const dialog = screen.getByRole("dialog");
    expect(within(dialog).queryByText(/Heuristic confidence 78%/) != null).toBe(score != null);
    expect(dialog.textContent).toContain(expected);
    expect(within(dialog).getByText(/Latency P99.*POST \/pay/)).toBeTruthy();
    expect(dialog.textContent).not.toContain("learned-adverse-z-v1");
    expect(dialog.textContent).not.toContain("learned:metrics:latency_p99");
  });

  it.each([
    ["stale", "stale_baseline", "Stale"],
    ["error", "timeout", "Source timeout"],
    ["partial", undefined, "Partial"],
  ] as const)("withholds a %s assessment score and confidence in the tile and drawer", (state, reasonCode, expected) => {
    show([service("checkout", {
      availability: {
        ...service("checkout").availability,
        "assessment.regression_score": { state, reason_code: reasonCode },
      },
      assessment: {
        org_id: "acme", service: "checkout", regression_score: 82, regressing: true, silent: false,
        confidence: 0.91, reason_code: "baseline_regression", algorithm_version: "v2", assessed_at: "2026-09-10T12:00:00Z",
      },
    })], { capabilities: [{ family: "assessment", measures: { regression_score: { state: "ready" } } }] });
    fireEvent.change(screen.getByLabelText("Show data"), { target: { value: "regression" } });
    expect(screen.getByText(expected)).toBeTruthy();
    expect(screen.queryByText("82 / 100")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Inspect checkout" }));
    const dialog = screen.getByRole("dialog");
    expect(within(dialog).getAllByText(expected).length).toBeGreaterThanOrEqual(2);
    expect(within(dialog).queryByText("82 / 100")).toBeNull();
    expect(within(dialog).queryByText("91%")).toBeNull();
    expect(within(dialog).queryByText("Confidence")).toBeNull();
  });

  it("withholds expired assessment score, confidence, and silent label", () => {
    show([service("checkout", {
      assessment: {
        org_id: "acme", service: "checkout", regression_score: 88, regressing: true, silent: true,
        confidence: 0.94, reason_code: "baseline_regression", algorithm_version: "v2",
        assessed_at: "2026-09-10T11:55:00Z", fresh_until: "2026-09-10T11:59:00Z",
      },
    })], {
      generated_at: "2026-09-10T12:00:00Z",
      capabilities: [{ family: "assessment", measures: { regression_score: { state: "ready" }, silent: { state: "ready" } } }],
    });
    fireEvent.click(screen.getByRole("button", { name: "Inspect checkout" }));
    const dialog = screen.getByRole("dialog");
    expect(within(dialog).getAllByText("Stale").length).toBeGreaterThan(0);
    expect(within(dialog).queryByText("88 / 100")).toBeNull();
    expect(within(dialog).queryByText(/Heuristic confidence/)).toBeNull();
    expect(within(dialog).queryByText("No Versus incident")).toBeNull();
  });

  it.each([
    [Number.NaN, 0.8],
    [-1, 0.8],
    [101, 0.8],
    [75, 0],
    [75, Number.NaN],
    [75, 1.1],
  ])("reports an unavailable assessment for invalid score %s or confidence %s", (regressionScore, confidence) => {
    show([service("checkout", { active_incidents: 0, assessment: {
      org_id: "acme", service: "checkout", regression_score: regressionScore, regressing: true,
      silent: true, confidence, algorithm_version: "v2", assessed_at: "2026-09-10T12:00:00Z", alert_state_known: true,
    } })], { capabilities: [{ family: "assessment", measures: { regression_score: { state: "ready" }, silent: { state: "ready" } } }] });
    fireEvent.change(screen.getByLabelText("Show data"), { target: { value: "regression" } });
    expect(screen.getByText("Unavailable")).toBeTruthy();
    expect(screen.queryByText("No Versus incident")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Inspect checkout" }));
    expect(within(screen.getByRole("dialog")).queryByText(/Heuristic confidence/)).toBeNull();
  });

  it("uses active incidents as the only visible incident-state indicator", () => {
    const assessment = {
      org_id: "acme", service: "checkout", regression_score: 82, regressing: true,
      silent: true, confidence: 0.91, algorithm_version: "v2", assessed_at: "2026-09-10T12:00:00Z", alert_state_known: true,
    };
    show([service("checkout", { active_incidents: 1, assessment })], {
      facets: { silent_regressions: 1 },
      capabilities: [{ family: "assessment", measures: { regression_score: { state: "ready" }, silent: { state: "ready" } } }],
    });
    expect(screen.queryByText("No Versus incident")).toBeNull();
    expect(within(screen.getByRole("button", { name: "Inspect checkout" })).getByText("1")).toBeTruthy();
    expect(screen.queryByRole("option", { name: /No Versus incident/ })).toBeNull();
  });

  it("expands and collapses the drawer with stable desktop focus boundaries", () => {
    show([service("checkout")]);
    fireEvent.click(screen.getByRole("button", { name: "Inspect checkout" }));
    const dialog = screen.getByRole("dialog", { name: "checkout" });
    const expand = within(dialog).getByRole("button", { name: "Expand panel" });
    expect(expand.getAttribute("aria-pressed")).toBe("false");
    fireEvent.click(expand);
    const collapse = within(dialog).getByRole("button", { name: "Collapse panel" });
    expect(collapse.getAttribute("aria-pressed")).toBe("true");
    const openService = within(dialog).getByRole("link", { name: "Open service" });
    const candidates = tabbableCandidates(dialog, () => true);
    expect(candidates[0]).toBe(collapse);
    expect(candidates.at(-1)).toBe(openService);
  });

  it("wires tab ownership and supports arrow, Home, and End roving focus", () => {
    show([service("checkout", { evidence: [evidence()] })], { capabilities: metricCapabilities });
    fireEvent.click(screen.getByRole("button", { name: "Inspect checkout" }));
    const dialog = screen.getByRole("dialog");
    const overview = within(dialog).getByRole("tab", { name: "Overview" });
    const signals = within(dialog).getByRole("tab", { name: "Signals" });
    const logs = within(dialog).getByRole("tab", { name: "Logs" });
    const overviewPanel = within(dialog).getByRole("tabpanel", { name: "Overview" });
    expect(overview.getAttribute("aria-controls")).toBe(overviewPanel.id);
    expect(overviewPanel.getAttribute("aria-labelledby")).toBe(overview.id);
    expect(overviewPanel.tabIndex).toBe(0);

    overview.focus();
    fireEvent.keyDown(overview, { key: "ArrowRight" });
    expect(document.activeElement).toBe(signals);
    expect(signals.getAttribute("aria-selected")).toBe("true");
    fireEvent.keyDown(signals, { key: "End" });
    expect(document.activeElement).toBe(logs);
    fireEvent.keyDown(logs, { key: "Home" });
    expect(document.activeElement).toBe(overview);
  });

  it("shows Overview immediately when Signals disappears for the same service", () => {
    const initial = makeSnapshot([service("checkout", { evidence: [evidence()] })], { capabilities: metricCapabilities });
    const view = render(<MemoryRouter><ServiceHealthExplorer snapshot={initial} /></MemoryRouter>);
    fireEvent.click(screen.getByRole("button", { name: "Inspect checkout" }));
    fireEvent.click(within(screen.getByRole("dialog")).getByRole("tab", { name: "Signals" }));

    view.rerender(<MemoryRouter><ServiceHealthExplorer snapshot={makeSnapshot([service("checkout")], { capabilities: metricCapabilities })} /></MemoryRouter>);

    const dialog = screen.getByRole("dialog");
    expect(within(dialog).queryByRole("tab", { name: "Signals" })).toBeNull();
    expect(within(dialog).getByRole("tab", { name: "Overview" }).getAttribute("aria-selected")).toBe("true");
    expect(within(dialog).getByRole("tabpanel", { name: "Overview" })).toBeTruthy();
  });

  it("closes the drawer when a refresh removes the inspected service", async () => {
    const initial = makeSnapshot([service("checkout")]);
    const view = render(<MemoryRouter><ServiceHealthExplorer snapshot={initial} /></MemoryRouter>);
    fireEvent.click(screen.getByRole("button", { name: "Inspect checkout" }));
    expect(screen.getByRole("dialog")).toBeTruthy();
    view.rerender(<MemoryRouter><ServiceHealthExplorer snapshot={makeSnapshot([])} /></MemoryRouter>);
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  });

  it("shows regression facets without duplicating the active incident status", () => {
    show([service("checkout", { active_incidents: 0, assessment: { org_id: "acme", service: "checkout", regression_score: 75, regressing: true, silent: true, confidence: 1, algorithm_version: "v1", assessed_at: "2026-09-10T12:00:00Z", alert_state_known: true } })], { facets: { regressing: 7, silent_regressions: 3 }, capabilities: [{ family: "assessment", measures: { regression_score: { state: "ready" }, silent: { state: "ready" } } }] });
    expect(screen.getByRole("option", { name: "Regressing (7)" })).toBeTruthy();
    expect(screen.queryByRole("option", { name: /No Versus incident/ })).toBeNull();
    cleanup();
    show([service("checkout")], { facets: {} });
    expect(screen.queryByRole("option", { name: /Regressing/ })).toBeNull();
    expect(screen.queryByRole("option", { name: /No Versus incident/ })).toBeNull();
  });

  it("resets premium mode and regression facet and clears its drawer after entitlement loss", async () => {
    const premiumService = service("checkout", {
      active_incidents: 0,
      evidence: [evidence()],
      assessment: {
        org_id: "acme", service: "checkout", regression_score: 82, regressing: true, silent: true,
        confidence: 0.91, algorithm_version: "v2", assessed_at: "2026-09-10T12:00:00Z", alert_state_known: true,
      },
    });
    const premium = makeSnapshot([premiumService], {
      capabilities: metricCapabilities,
      facets: { regressing: 1, silent_regressions: 1 },
    });
    const community = makeSnapshot([service("checkout")], {
      capabilities: [{ family: "metrics", measures: { latency: { state: "restricted" } } }],
      facets: { regressing: 1, silent_regressions: 1 },
    });
    const view = render(<MemoryRouter><ServiceHealthExplorer snapshot={premium} /></MemoryRouter>);
    fireEvent.change(screen.getByLabelText("Show data"), { target: { value: "latency" } });
    fireEvent.change(screen.getByLabelText("Health state"), { target: { value: "regressing" } });
    fireEvent.click(screen.getByRole("button", { name: "Inspect checkout" }));
    expect(screen.getByRole("dialog")).toBeTruthy();
    view.rerender(<MemoryRouter><ServiceHealthExplorer snapshot={community} /></MemoryRouter>);
    await waitFor(() => expect((screen.getByLabelText("Show data") as HTMLSelectElement).value).toBe("combined"));
    expect((screen.getByLabelText("Health state") as HTMLSelectElement).value).toBe("all");
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(screen.getByText("Premium Service Health data is no longer available. Showing All data and all services.")).toBeTruthy();
    expect(screen.queryByText("245.5 ms")).toBeNull();
  });

  it("clears the entitlement announcement without repeating it on an unchanged snapshot", () => {
    vi.useFakeTimers();
    try {
      const premium = makeSnapshot([service("checkout", { evidence: [evidence()] })], { capabilities: metricCapabilities });
      const community = makeSnapshot([service("checkout")], { capabilities: [{ family: "metrics", measures: { latency: { state: "restricted" } } }] });
      const view = render(<MemoryRouter><ServiceHealthExplorer snapshot={premium} /></MemoryRouter>);
      fireEvent.change(screen.getByLabelText("Show data"), { target: { value: "latency" } });
      view.rerender(<MemoryRouter><ServiceHealthExplorer snapshot={community} /></MemoryRouter>);
      const message = "Premium Service Health data is no longer available. Showing All data and all services.";
      expect(screen.getByText(message)).toBeTruthy();
      act(() => vi.advanceTimersByTime(4000));
      expect(screen.queryByText(message)).toBeNull();
      view.rerender(<MemoryRouter><ServiceHealthExplorer snapshot={community} /></MemoryRouter>);
      expect(screen.queryByText(message)).toBeNull();
    } finally {
      vi.useRealTimers();
    }
  });

  it("does not announce entitlement loss or close the drawer for a transient capability failure", () => {
    const premium = makeSnapshot([service("checkout", { evidence: [evidence()] })], { capabilities: metricCapabilities });
    const transient = makeSnapshot([service("checkout")], { capabilities: [{ family: "metrics", measures: { latency: { state: "error", reason_code: "timeout" } } }] });
    const view = render(<MemoryRouter><ServiceHealthExplorer snapshot={premium} /></MemoryRouter>);
    fireEvent.click(screen.getByRole("button", { name: "Inspect checkout" }));
    view.rerender(<MemoryRouter><ServiceHealthExplorer snapshot={transient} /></MemoryRouter>);
    expect(screen.getByRole("dialog")).toBeTruthy();
    expect(screen.queryByText(/Premium Service Health data is no longer available/)).toBeNull();
  });
});