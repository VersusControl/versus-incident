// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import type { SSOSession } from "@/lib/api";
import { AppShell } from "./AppShell";

// The shell mounts no page before the session's permissions are known, so a
// deep link or reload of a page the role can't open sends no request.

const role = vi.hoisted(() => ({ session: undefined as SSOSession | undefined, loading: false }));

vi.mock("@/lib/useEffectiveRole", () => ({
  useEffectiveRole: () => ({
    org: role.session ? "acme" : null,
    enterprise: Boolean(role.session),
    role: role.session?.role ?? null,
    isAdmin: false,
    hasSession: Boolean(role.session),
    loading: role.loading,
    session: { data: role.session },
  }),
}));
vi.mock("./Sidebar", () => ({ Sidebar: () => <nav />, SidebarContent: () => <nav /> }));
vi.mock("@/lib/auth", () => ({ ReauthModal: () => null }));
vi.mock("./TopBar", () => ({ TopBar: ({ title }: { title: string }) => <header>{title}</header> }));

const VIEWER: SSOSession = {
  org: "acme",
  email: "viewer@example.com",
  subject: "viewer",
  mfa: false,
  role: "viewer",
  roles: ["viewer"],
  permissions: ["incidents:view"],
  issued_at: "2026-11-20T00:00:00Z",
  expires_at: "2026-11-21T00:00:00Z",
};

afterEach(() => {
  cleanup();
  role.session = undefined;
  role.loading = false;
});

const client = new QueryClient();

function tree(path: string, mounted: () => void) {
  function Page() {
    mounted();
    return <p>Changes page</p>;
  }
  return (
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={[path]}>
        <Routes>
          <Route element={<AppShell />}>
            <Route path="/aws/changes" element={<Page />} />
          </Route>
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>
  );
}

function shell(path: string, mounted: () => void) {
  return render(tree(path, mounted));
}

describe("AppShell — role not yet known", () => {
  it("a deep link to a page the role can't open mounts nothing while whoami loads, then the plain refusal, never the page", () => {
    const mounted = vi.fn();
    role.loading = true;
    const view = shell("/aws/changes", mounted);
    expect(mounted).not.toHaveBeenCalled();
    expect(document.querySelector("[data-shell-pending]")).toBeTruthy();
    expect(screen.queryByText("You don't have access to this page")).toBeNull();

    role.loading = false;
    role.session = VIEWER;
    view.rerender(tree("/aws/changes", mounted));
    expect(screen.getByText("You don't have access to this page")).toBeTruthy();
    expect(screen.queryByText("Changes page")).toBeNull();
    expect(mounted).not.toHaveBeenCalled();
  });

  it("once the role is known and allows the page, it mounts", () => {
    const mounted = vi.fn();
    shell("/aws/changes", mounted);
    expect(mounted).toHaveBeenCalled();
    expect(screen.getByText("Changes page")).toBeTruthy();
  });
});
