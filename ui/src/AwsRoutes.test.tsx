// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter, Outlet, useLocation } from "react-router-dom";
import App from "./App";

// The real route table with the shell and pages stubbed: each AWS page
// renders its name, so a test sees where a URL lands.

vi.mock("./lib/auth", () => ({ AuthGate: ({ children }: { children: React.ReactNode }) => <>{children}</> }));

function Probe() {
  const l = useLocation();
  return <output data-testid="at">{`${l.pathname}${l.search}`}</output>;
}

vi.mock("./components/AppShell", () => ({
  AppShell: () => (
    <>
      <Outlet />
      <Probe />
    </>
  ),
}));

vi.mock("./pages/aws/overview/AwsOverviewPage", () => ({ AwsOverviewPage: () => <p>AwsOverviewPage</p> }));
vi.mock("./pages/aws/topology/AwsTopologyPage", () => ({ AwsTopologyPage: () => <p>AwsTopologyPage</p> }));
vi.mock("./pages/aws/coverage/AwsCoveragePage", () => ({ AwsCoveragePage: () => <p>AwsCoveragePage</p> }));
vi.mock("./pages/aws/issues/AwsIssuesPage", () => ({ AwsIssuesPage: () => <p>AwsIssuesPage</p> }));
vi.mock("./pages/aws/changes/AwsChangesPage", () => ({ AwsChangesPage: () => <p>AwsChangesPage</p> }));
vi.mock("./pages/aws/finops/AwsFinOpsPage", () => ({ AwsFinOpsPage: () => <p>AwsFinOpsPage</p> }));
vi.mock("./pages/aws/AwsSettingsPage", () => ({ AwsSettingsPage: () => <p>AwsSettingsPage</p> }));

afterEach(cleanup);

function at(path: string) {
  render(
    <MemoryRouter initialEntries={[path]}>
      <App />
    </MemoryRouter>,
  );
}

describe("AWS routes", () => {
  it.each([
    ["/aws", "/aws/overview", "AwsOverviewPage"],
    ["/aws/map", "/aws/topology", "AwsTopologyPage"],
    ["/aws/resources", "/aws/topology?view=list", "AwsTopologyPage"],
    ["/aws/applications", "/aws/topology?level=groups", "AwsTopologyPage"],
    ["/aws/security", "/aws/issues", "AwsIssuesPage"],
  ])("sends the old bookmark %s to %s", async (from, to, page) => {
    at(from);
    expect(await screen.findByText(page)).toBeTruthy();
    expect(screen.getByTestId("at").textContent).toBe(to);
  });

  it.each([
    ["/aws/overview", "AwsOverviewPage"],
    ["/aws/issues", "AwsIssuesPage"],
    ["/aws/topology", "AwsTopologyPage"],
    ["/aws/coverage", "AwsCoveragePage"],
    ["/aws/changes", "AwsChangesPage"],
    ["/aws/finops", "AwsFinOpsPage"],
    ["/aws/settings", "AwsSettingsPage"],
  ])("opens %s", async (path, page) => {
    at(path);
    expect(await screen.findByText(page)).toBeTruthy();
    expect(screen.getByTestId("at").textContent).toBe(path);
  });
});
