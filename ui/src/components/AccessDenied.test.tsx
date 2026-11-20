// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import type { SSOSession } from "@/lib/api";
import { AccessDeniedPage, LandingRedirect } from "./AccessDenied";

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
vi.mock("./TopBar", () => ({ TopBar: ({ title }: { title: string }) => <header>{title}</header> }));

afterEach(() => {
  cleanup();
  role.session = undefined;
  role.loading = false;
});

const FINANCE: SSOSession = {
  org: "acme",
  email: "fin@example.com",
  subject: "fin",
  mfa: false,
  role: "finance",
  roles: ["finance"],
  permissions: ["finops:view"],
  issued_at: "2026-11-20T00:00:00Z",
  expires_at: "2026-11-21T00:00:00Z",
};

function land(path = "/") {
  return render(
    <QueryClientProvider client={new QueryClient()}>
      <MemoryRouter initialEntries={[path]}>
        <Routes>
          <Route path="/" element={<LandingRedirect />} />
          <Route path="/now" element={<p>Now page</p>} />
          <Route path="/aws/finops" element={<p>FinOps page</p>} />
          <Route path="/denied" element={<AccessDeniedPage />} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

describe("role-aware landing", () => {
  it("lands a finance-only user on FinOps, not Now", async () => {
    role.session = FINANCE;
    land();
    expect(await screen.findByText("FinOps page")).toBeTruthy();
    expect(screen.queryByText("Now page")).toBeNull();
  });

  it("lands an OSS operator (no permissions listed) on Now", async () => {
    land();
    expect(await screen.findByText("Now page")).toBeTruthy();
  });

  it("waits for the session before choosing", () => {
    role.loading = true;
    land();
    expect(screen.queryByText("Now page")).toBeNull();
    expect(screen.queryByText("FinOps page")).toBeNull();
  });

  it("the stand-in for a page the role can't open is a plain refusal with a way back, no Retry", () => {
    role.session = FINANCE;
    land("/denied");
    expect(screen.getByText("You don't have access to this page")).toBeTruthy();
    expect(screen.getByRole("link", { name: "Go to your start page" }).getAttribute("href")).toBe("/aws/finops");
    expect(screen.queryByRole("button", { name: "Retry" })).toBeNull();
    expect(screen.queryByRole("alert")).toBeNull();
  });
});
