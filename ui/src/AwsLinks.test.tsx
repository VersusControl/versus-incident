// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter, Outlet, useLocation } from "react-router-dom";
import App from "./App";

// Every place the AWS pages link to, and every old bookmark, lands on a
// real page: none falls through to the catch-all route.

vi.mock("./lib/auth", () => ({ AuthGate: ({ children }: { children: React.ReactNode }) => <>{children}</> }));

vi.mock("./components/AccessDenied", async (importActual) => ({
  ...(await importActual<typeof import("./components/AccessDenied")>()),
  LandingRedirect: () => <p>CatchAll</p>,
}));

function Probe() {
  const l = useLocation();
  return <output data-testid="at">{`${l.pathname}${l.search}${l.hash}`}</output>;
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

describe("AWS links land on real pages", () => {
  it.each([
    // Summary numbers, card links and rows on Overview.
    ["/aws/issues?tier=act_now", "AwsIssuesPage"],
    ["/aws/issues?tier=worth_a_look", "AwsIssuesPage"],
    ["/aws/issues?id=iss-1", "AwsIssuesPage"],
    ["/aws/issues?account=333344445555", "AwsIssuesPage"],
    ["/aws/topology?level=groups", "AwsTopologyPage"],
    ["/aws/topology?level=groups&account=333344445555", "AwsTopologyPage"],
    ["/aws/topology?level=accounts", "AwsTopologyPage"],
    ["/aws/topology?level=focus&account=333344445555&ref=arn%3Aaws%3As3%3A%3A%3Ab", "AwsTopologyPage"],
    ["/aws/changes", "AwsChangesPage"],
    // Every warning line's Details, and the breadcrumb.
    ["/aws/coverage", "AwsCoveragePage"],
    ["/aws/overview", "AwsOverviewPage"],
    // Coverage and Topology links into Settings.
    ["/aws/settings", "AwsSettingsPage"],
    ["/aws/settings#collection-cost", "AwsSettingsPage"],
    // A trailing slash on the old AWS root.
    ["/aws/", "AwsOverviewPage"],
  ])("%s opens %s", async (path, page) => {
    at(path);
    expect(await screen.findByText(page)).toBeTruthy();
    expect(screen.queryByText("CatchAll")).toBeNull();
  });

  it.each([
    ["/aws", "/aws/overview"],
    ["/aws/map", "/aws/topology"],
    ["/aws/resources", "/aws/topology?view=list"],
    ["/aws/applications", "/aws/topology?level=groups"],
    ["/aws/security", "/aws/issues"],
  ])("redirects %s to %s", async (from, to) => {
    at(from);
    await screen.findByTestId("at");
    expect(screen.getByTestId("at").textContent).toBe(to);
    expect(screen.queryByText("CatchAll")).toBeNull();
  });
});
