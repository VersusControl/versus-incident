import { describe, it, expect } from "vitest";
import { resolveSignInSurface } from "@/lib/signInSurface";

// resolveSignInSurface is the credential mix the standalone sign-in screen
// (lib/auth.tsx SecretForm) renders. The contract these tests pin:
//   - The gateway-secret form is OSS-only: PRESENT when enterprise is false,
//     ABSENT on every licensed (enterprise) binary.
//   - A licensed binary offers the built-in default-admin form plus any SSO
//     buttons — never the gateway secret, even with zero SSO and no require_sso.
//   - OSS must not regress: with no enterprise it always shows the gateway
//     secret (community has no SSO / require_sso to retire it).

describe("resolveSignInSurface — enterprise (licensed) binary", () => {
  it("offers ONLY the built-in admin (no SSO, no gateway secret) on a fresh licensed boot", () => {
    const s = resolveSignInSurface({
      enterprise: true,
      hasSso: false,
      requireSso: false,
    });
    expect(s.showLocalAdmin).toBe(true);
    expect(s.showSso).toBe(false);
    // The bug fix: a licensed binary with zero SSO and no require_sso must
    // still NEVER surface the gateway-secret form.
    expect(s.showGatewaySecret).toBe(false);
  });

  it("offers built-in admin + SSO (still no gateway secret) when SSO is configured but not enforced", () => {
    const s = resolveSignInSurface({
      enterprise: true,
      hasSso: true,
      requireSso: false,
    });
    expect(s.showLocalAdmin).toBe(true);
    expect(s.showSso).toBe(true);
    expect(s.showGatewaySecret).toBe(false);
  });

  it("offers built-in admin + SSO (no gateway secret) when require_sso is enforced", () => {
    const s = resolveSignInSurface({
      enterprise: true,
      hasSso: true,
      requireSso: true,
    });
    expect(s.showLocalAdmin).toBe(true);
    expect(s.showSso).toBe(true);
    expect(s.showGatewaySecret).toBe(false);
  });
});

describe("resolveSignInSurface — OSS/community binary", () => {
  it("offers ONLY the gateway secret (no built-in admin, no SSO)", () => {
    const s = resolveSignInSurface({
      enterprise: false,
      hasSso: false,
      requireSso: false,
    });
    expect(s.showLocalAdmin).toBe(false);
    expect(s.showSso).toBe(false);
    // OSS must not regress — the gateway secret IS the OSS sign-in.
    expect(s.showGatewaySecret).toBe(true);
  });
});
