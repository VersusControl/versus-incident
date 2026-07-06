import { describe, it, expect } from "vitest";
import { ApiError, resolveInitialAuth, type AuthProbe } from "@/lib/api";

// resolveInitialAuth is the pure console-entry decision AuthGate runs on mount.
// These tests pin the two-credential contract without a DOM harness:
//   1. a held gateway secret that verifies -> ok; a 401 -> needs-secret;
//      a transient (non-401) error must NOT trap the user -> ok.
//   2. with NO secret, an established SSO session opens the console; a 401
//      no-session (or a community binary with no deployment org) falls back to
//      the secret screen.

// base returns an AuthProbe whose checks all reject, so each test overrides
// only the behavior it asserts.
function base(): AuthProbe {
  return {
    hasSecret: () => false,
    checkSecret: () => Promise.reject(new ApiError(401, "unauthorized")),
    deploymentOrg: () => Promise.reject(new ApiError(403, "not enterprise")),
    probeSession: () => Promise.reject(new ApiError(401, "no active session")),
  };
}

describe("resolveInitialAuth — held secret path", () => {
  it("opens the console when a held secret verifies", async () => {
    const p = base();
    p.hasSecret = () => true;
    p.checkSecret = () => Promise.resolve({});
    expect(await resolveInitialAuth(p)).toBe("ok");
  });

  it("shows the secret screen when the held secret is rejected (401)", async () => {
    const p = base();
    p.hasSecret = () => true;
    p.checkSecret = () => Promise.reject(new ApiError(401, "bad secret"));
    expect(await resolveInitialAuth(p)).toBe("needs-secret");
  });

  it("does NOT trap the user on a transient (non-401) error", async () => {
    const p = base();
    p.hasSecret = () => true;
    p.checkSecret = () => Promise.reject(new Error("network down"));
    expect(await resolveInitialAuth(p)).toBe("ok");
  });

  it("does NOT probe SSO when a secret is held", async () => {
    const p = base();
    p.hasSecret = () => true;
    p.checkSecret = () => Promise.resolve({});
    let probed = false;
    p.deploymentOrg = () => {
      probed = true;
      return Promise.resolve("b");
    };
    await resolveInitialAuth(p);
    expect(probed).toBe(false);
  });
});

describe("resolveInitialAuth — SSO session path (no secret)", () => {
  it("admits a valid SSO session and opens the console", async () => {
    const p = base();
    p.deploymentOrg = () => Promise.resolve("b");
    p.probeSession = (org) => {
      expect(org).toBe("b");
      return Promise.resolve({ org, email: "u@galaxyfinx.com" });
    };
    expect(await resolveInitialAuth(p)).toBe("ok");
  });

  it("falls back to needs-secret when the session probe 401s", async () => {
    const p = base();
    p.deploymentOrg = () => Promise.resolve("b");
    p.probeSession = () => Promise.reject(new ApiError(401, "no active session"));
    expect(await resolveInitialAuth(p)).toBe("needs-secret");
  });

  it("falls back to needs-secret on a community binary (no deployment org)", async () => {
    const p = base();
    p.deploymentOrg = () => Promise.reject(new ApiError(403, "not enterprise"));
    // probeSession must never be reached.
    p.probeSession = () => {
      throw new Error("probeSession should not run without a deployment org");
    };
    expect(await resolveInitialAuth(p)).toBe("needs-secret");
  });
});
