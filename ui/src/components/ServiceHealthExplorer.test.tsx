// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vitest";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import type { ServiceHealthService, ServiceHealthSnapshot } from "@/lib/api";
import { ServiceHealthExplorer } from "./ServiceHealthExplorer";

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

function show(services: ServiceHealthService[]) {
  const snapshot: ServiceHealthSnapshot = {
    snapshot_id: "sample", generated_at: "2026-09-10T12:00:00Z", latest_attempt: "2026-09-10T12:00:00Z",
    next_collection_at: "2026-09-10T12:01:00Z", settings_revision: 1, window_seconds: 300,
    services, domains: [], facets: { nominal: 1, pressure: 1, unknown: 1 },
    coverage: { total_services: services.length, observed_services: services.length, partial: false }, capabilities: [],
  };
  return render(<MemoryRouter><ServiceHealthExplorer snapshot={snapshot} /></MemoryRouter>);
}

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
    expect(within(tile).getByText(/Stale.*Estimated/)).toBeTruthy();
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
});