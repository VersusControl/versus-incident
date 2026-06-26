// Pure decision helpers for the built-in default-admin (non-SSO) login surface.
// The console has no DOM test harness, so the state the login form and the
// sign-out affordance render is decided here and unit-tested in isolation —
// the same pattern as resolveInitialAuth / adminGateState. Keeping these pure
// (no React, no fetch) pins the security-relevant contracts:
//   1. a 401 is a GENERIC message — it never distinguishes wrong-password from
//      a disabled account (no user enumeration, mirroring the server);
//   2. a 429 is a DISTINCT lockout state (tell the user to wait, not retry);
//   3. sign-out revokes via the local-admin route iff the session is local.

import { ApiError } from "./api";

// GENERIC_LOGIN_ERROR is the single invalid-credentials message. It is
// deliberately non-enumerating: the server returns the same 401 for a wrong
// password and for a disabled account, so the UI must not reveal which.
export const GENERIC_LOGIN_ERROR = "Invalid username or password.";

// LOCKED_LOGIN_MESSAGE is the distinct lockout copy (429), separate from the
// generic 401 so the user is told to wait rather than to retry immediately.
export const LOCKED_LOGIN_MESSAGE =
  "Too many failed attempts. Please wait and try again.";

// UNREACHABLE_LOGIN_ERROR covers a transport/non-HTTP failure (the agent could
// not be reached at all) — still non-enumerating.
export const UNREACHABLE_LOGIN_ERROR = "Unable to reach the agent.";

// LocalLoginState is the outcome the local-admin login form renders after a
// submit attempt. "locked" drives the distinct lockout hook; "error" drives
// the generic invalid-credentials hook.
export type LocalLoginState =
  | { kind: "locked" }
  | { kind: "error"; message: string };

// classifyLocalLoginError maps a login failure to the form state. It NEVER
// surfaces a server message verbatim for a credential failure (no enumeration):
//   429            → locked (distinct lockout state)
//   401            → generic invalid-credentials
//   other ApiError → generic invalid-credentials (e.g. 403 community, 500)
//   non-HTTP error → "unable to reach the agent"
export function classifyLocalLoginError(err: unknown): LocalLoginState {
  if (err instanceof ApiError) {
    if (err.status === 429) return { kind: "locked" };
    // 401 and every other terminal HTTP answer collapse to the SAME generic
    // message so the form leaks nothing about the account's existence/state.
    return { kind: "error", message: GENERIC_LOGIN_ERROR };
  }
  return { kind: "error", message: UNREACHABLE_LOGIN_ERROR };
}

// isLocalAdminSession reports whether the current session is the built-in
// default admin (a local, non-SSO session). The sign-out affordance uses it to
// pick the local-admin logout route over the SSO one (G4). Fail closed: an
// absent/unknown session is treated as NOT local (SSO logout still revokes the
// shared session cookie).
export function isLocalAdminSession(
  session: { local?: boolean } | null | undefined,
): boolean {
  return Boolean(session?.local);
}

// isNoOtherAdminError reports whether a disable-default-admin failure is the
// server's no-lockout refusal (422 no_other_admin) — the case the Members card
// must surface IN THE DOM, not only as a toast (G3).
export function isNoOtherAdminError(err: unknown): boolean {
  if (!(err instanceof ApiError) || err.status !== 422) return false;
  const body = err.body;
  if (body && typeof body === "object" && "code" in body) {
    return (body as { code?: unknown }).code === "no_other_admin";
  }
  // A 422 on this route is the no-lockout guard even without an explicit code.
  return true;
}
