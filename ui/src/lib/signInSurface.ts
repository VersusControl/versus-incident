// resolveSignInSurface is the pure decision behind the standalone sign-in
// screen (lib/auth.tsx SecretForm). It is extracted so the credential mix can
// be pinned without a DOM harness (the vitest env is "node"), mirroring
// resolveInitialAuth.
//
// The credentials split by binary:
//   - Enterprise (licensed): the built-in default-admin form is the bootstrap
//     path, plus any configured SSO connection buttons. The gateway secret is
//     an OSS-only data-plane credential and is NEVER offered on a licensed
//     binary — not even with zero SSO and no require_sso.
//   - OSS/community (unlicensed): the gateway secret IS the sign-in. There is
//     no built-in admin and no SSO, so the gateway-secret form is the only path.
export interface SignInSurface {
  // Built-in default-admin username/password form (enterprise-only, G1).
  showLocalAdmin: boolean;
  // One button per enabled SSO connection (enterprise-only).
  showSso: boolean;
  // Gateway-secret form — OSS-only.
  showGatewaySecret: boolean;
}

export function resolveSignInSurface(input: {
  // True once the license-issued /deployment route 200s (licensed binary).
  enterprise: boolean;
  // True when at least one SSO connection is enabled for the deployment org.
  hasSso: boolean;
  // True when the org ENFORCES SSO (require_sso) — only meaningful with hasSso.
  requireSso: boolean;
}): SignInSurface {
  // require_sso retires every non-SSO path; it only ever applies on enterprise.
  const ssoOnly = input.hasSso && input.requireSso;
  return {
    showLocalAdmin: input.enterprise,
    showSso: input.hasSso,
    // Gateway secret is OSS-only. On a licensed binary it is suppressed
    // unconditionally; on OSS it shows unless SSO is enforced (which a
    // community binary never reaches anyway).
    showGatewaySecret: !input.enterprise && !ssoOnly,
  };
}
