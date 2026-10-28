// @vitest-environment jsdom
import { describe, expect, it } from "vitest";
import { ssoLoginWithReturn } from "@/lib/ssoReturn";

describe("SSO return path", () => {
  it.each([
    ["/agent/kubernetes", "?cluster=prod%2Feu&view=timeline&root_uid=uid&namespace=shop&center=2026-10-19T12%3A00%3A00Z"],
    ["/agent/chat", "?provider=kubernetes&cluster=east&resource_id=apps~v1~deployments&name=checkout&include_changes=true&issue=pod.crash_loop"],
    ["/", ""],
  ])("preserves the relative path and encoded query for %s", (pathname, search) => {
    const url = new URL(ssoLoginWithReturn("/api/sso/login/oidc?connection=work&return_to=old", { pathname, search }), window.location.origin);
    expect(url.searchParams.get("connection")).toBe("work");
    expect(url.searchParams.getAll("return_to")).toEqual([pathname + search]);
    expect(url.origin).toBe(window.location.origin);
  });
  it.each(["//other.example/path", "/\\other.example", "https://other.example"])("falls back for an unsafe path: %s", (pathname) => {
    const url = new URL(ssoLoginWithReturn("/api/sso/login/oidc", { pathname, search: "" }), window.location.origin);
    expect(url.searchParams.get("return_to")).toBe("/");
  });
});
