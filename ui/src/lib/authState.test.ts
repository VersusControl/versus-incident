import { describe, it, expect } from "vitest";
import {
  ApiError,
  isCommunityDeploymentError,
  resolveInitialAuth,
  type AuthProbe,
} from "@/lib/api";

// resolveInitialAuth is the pure console-entry decision AuthGate runs on mount.
// These tests pin cookie-first reload admission without a DOM harness.

// base returns an AuthProbe whose checks all reject, so each test overrides
// only the behavior it asserts.
function base(): AuthProbe {
  return {
    probeGatewaySession: () => Promise.reject(new ApiError(401, "unauthorized")),
    deploymentOrg: () => Promise.reject(new ApiError(403, "not enterprise")),
    probeSession: () => Promise.reject(new ApiError(401, "no active session")),
  };
}

describe("resolveInitialAuth — gateway session path", () => {
  it("opens the console when the protected cookie probe succeeds", async () => {
    const p = base();
    p.probeGatewaySession = () => Promise.resolve({});
    expect(await resolveInitialAuth(p)).toBe("ok");
  });

  it("does not probe enterprise discovery after a valid gateway cookie", async () => {
    const p = base();
    p.probeGatewaySession = () => Promise.resolve({});
    let probed = false;
    p.deploymentOrg = () => {
      probed = true;
      return Promise.resolve("b");
    };
    await resolveInitialAuth(p);
    expect(probed).toBe(false);
  });
});

describe("resolveInitialAuth — authoritative deployment states", () => {
  it("admits a valid SSO session and opens the console", async () => {
    const p = base();
    p.deploymentOrg = () => Promise.resolve("b");
    p.probeSession = (org) => {
      expect(org).toBe("b");
      return Promise.resolve({ org, email: "u@galaxyfinx.com" });
    };
    expect(await resolveInitialAuth(p)).toBe("ok");
  });

  it("requires Enterprise login when a licensed deployment has no session", async () => {
    const p = base();
    p.deploymentOrg = () => Promise.resolve("b");
    p.probeSession = () => Promise.reject(new ApiError(401, "no active session"));
    expect(await resolveInitialAuth(p)).toBe("needs-enterprise-login");
  });

  it("requires gateway login on a community binary (no deployment org)", async () => {
    const p = base();
    p.deploymentOrg = () => Promise.reject(new ApiError(403, "not enterprise"));
    // probeSession must never be reached.
    p.probeSession = () => {
      throw new Error("probeSession should not run without a deployment org");
    };
    expect(await resolveInitialAuth(p)).toBe("needs-gateway-secret");
  });

  it("requires retry when deployment discovery fails transiently", async () => {
    const p = base();
    p.deploymentOrg = () => Promise.reject(new TypeError("network unavailable"));
    p.probeSession = () => {
      throw new Error("probeSession should not run after failed discovery");
    };
    expect(await resolveInitialAuth(p)).toBe("retry");
  });

  it("requires retry when the licensed session probe fails transiently", async () => {
    const p = base();
    p.deploymentOrg = () => Promise.resolve("b");
    p.probeSession = () => Promise.reject(new ApiError(503, "unavailable"));
    expect(await resolveInitialAuth(p)).toBe("retry");
  });
});

describe("isCommunityDeploymentError", () => {
  it("accepts only authoritative community responses", () => {
    expect(isCommunityDeploymentError(new ApiError(403, "not enterprise"))).toBe(true);
    expect(isCommunityDeploymentError(new ApiError(404, "not mounted"))).toBe(true);
    expect(isCommunityDeploymentError(new ApiError(503, "unavailable"))).toBe(false);
    expect(isCommunityDeploymentError(new TypeError("network unavailable"))).toBe(false);
  });
});
