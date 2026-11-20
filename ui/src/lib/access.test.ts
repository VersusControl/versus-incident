import { describe, expect, it } from "vitest";
import { ApiError, type SSOSession } from "@/lib/api";
import { badgeRole, canOpen, isForbidden, landingPath, pathArea, sessionPermissions } from "@/lib/access";
import { roleLabel } from "@/lib/role";

// The Enterprise whoami, field for field, for each role the shell treats apart.
function whoami(over: Partial<SSOSession>): SSOSession {
  return {
    org: "acme",
    email: "user@example.com",
    subject: "user",
    mfa: false,
    issued_at: "2026-11-20T00:00:00Z",
    expires_at: "2026-11-21T00:00:00Z",
    ...over,
  };
}

const FINANCE = whoami({ role: "finance", roles: ["finance"], permissions: ["finops:view"] });
const VIEWER = whoami({ role: "viewer", roles: ["viewer"], permissions: ["incidents:view"] });
const OWNER = whoami({
  role: "owner",
  roles: ["owner"],
  permissions: ["audit:view", "finops:view", "incidents:act", "incidents:view", "infrastructure:view", "roles:manage"],
});

describe("access from the session whoami", () => {
  it("reads a finance whoami: FinOps and chat open, every incident and infrastructure page closed", () => {
    const perms = sessionPermissions(FINANCE);
    expect(perms).toEqual(["finops:view"]);
    expect(canOpen(pathArea("/aws/finops"), perms)).toBe(true);
    expect(canOpen(pathArea("/aws/finops/increases/inc-1"), perms)).toBe(true);
    expect(canOpen(pathArea("/agent/chat"), perms)).toBe(true);
    for (const p of ["/now", "/incidents", "/agent", "/agent/metrics", "/people", "/admin", "/settings"]) {
      expect(canOpen(pathArea(p), perms)).toBe(false);
    }
    for (const p of ["/aws/changes", "/aws/settings", "/agent/kubernetes"]) {
      expect(canOpen(pathArea(p), perms)).toBe(false);
    }
  });

  it("lands finance on FinOps and everyone who reads incidents on Now", () => {
    expect(landingPath(sessionPermissions(FINANCE))).toBe("/aws/finops");
    expect(landingPath(sessionPermissions(VIEWER))).toBe("/now");
    expect(landingPath(sessionPermissions(OWNER))).toBe("/now");
    expect(landingPath(null)).toBe("/now");
  });

  it("keeps a viewer off the AWS pages, which need the infrastructure view", () => {
    const perms = sessionPermissions(VIEWER);
    expect(canOpen(pathArea("/aws/changes"), perms)).toBe(false);
    expect(canOpen(pathArea("/aws/finops"), perms)).toBe(false);
    expect(canOpen(pathArea("/now"), perms)).toBe(true);
  });

  it("hides nothing when the whoami lists no permissions (older server, or grants unreadable)", () => {
    expect(sessionPermissions(whoami({ role: "viewer" }))).toBeNull();
    expect(sessionPermissions(whoami({ role: "", roles: [], permissions: [] }))).toBeNull();
    expect(sessionPermissions(undefined)).toBeNull();
    expect(canOpen("infrastructure", null)).toBe(true);
  });

  it("names the real role on the badge: Finance, not Viewer", () => {
    expect(roleLabel(badgeRole(FINANCE))).toBe("Finance");
    expect(roleLabel(badgeRole(whoami({ role: "finance" })))).toBe("Finance");
    expect(roleLabel(badgeRole(OWNER))).toBe("Owner");
  });

  it("treats only a 403 as a refusal", () => {
    expect(isForbidden(new ApiError(403, "forbidden", { error: "forbidden", code: "permission_required", permission: "infrastructure:view" }))).toBe(true);
    expect(isForbidden(new ApiError(500, "boom"))).toBe(false);
    expect(isForbidden(new Error("network"))).toBe(false);
  });
});
