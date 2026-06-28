// ssoConfig — pure, DOM-free decision logic shared by the Enterprise SSO
// config control (X4 item 2). It mirrors lib/agentAI: everything here is
// side-effect-free so it can be unit-tested in the node vitest env (the UI has
// no jsdom/testing-library); the component is a thin shell over these helpers.

import { ApiError } from "@/lib/api";

// SsoErrorState is how the control routes a failed GET. SSO config is per-org
// AND license-gated, so a 404 is overloaded: it can mean the route is absent
// (OSS binary → locked) OR the org simply has no config yet (licensed →
// editable empty form) OR the org isn't a provisioned tenant. The error body
// disambiguates: the licensed handler returns a JSON `{ error }` message; the
// OSS route-absent 404 returns Fiber's plain-text default (no JSON `error`).
export type SsoErrorState =
  | "locked" // 404 route absent (OSS binary) — no enterprise route
  | "unconfigured" // 404 — licensed, org has no IdP config yet
  | "org-unknown" // 404 — org is not a known/active tenant
  | "forbidden" // 401/403/503 — no session, insufficient RBAC role, or guard not wired
  | "error"; // anything else

// ssoErrorText pulls the human error string out of an ApiError: the JSON
// `{ error }` field when present, else a string body (Fiber's plain-text
// default 404), else the ApiError message. Returns "" for non-ApiErrors.
export function ssoErrorText(err: unknown): string {
  if (!(err instanceof ApiError)) return "";
  if (
    err.body &&
    typeof err.body === "object" &&
    "error" in err.body &&
    typeof (err.body as { error: unknown }).error === "string"
  ) {
    return (err.body as { error: string }).error;
  }
  if (typeof err.body === "string") return err.body;
  return err.message ?? "";
}

// classifySsoError maps a failed GET to the UI state. The 404 split is the
// crux: a JSON body that says "no IdP configuration" is a licensed-but-empty
// org (show the create form), "unknown or inactive org" is an unprovisioned
// org, and anything else at 404 (Fiber's plain-text "Cannot GET …") is an OSS
// binary with the route absent → locked, same as a 403. A 401/403/503 maps to
// "forbidden": the SSO config writes are RBAC-gated on the caller's session
// role (sso:manage), so no session / insufficient role / an unwired guard all
// land on the "requires the admin role" notice.
export function classifySsoError(err: unknown): SsoErrorState {
  if (!(err instanceof ApiError)) return "error";
  switch (err.status) {
    case 401:
    case 403:
    case 503:
      return "forbidden";
    case 404: {
      const msg = ssoErrorText(err);
      if (/no IdP configuration/i.test(msg)) return "unconfigured";
      if (/unknown or inactive org/i.test(msg)) return "org-unknown";
      return "locked";
    }
    default:
      return "error";
  }
}

// parseList splits a comma / whitespace / newline separated free-text field
// (scopes, allowed_domains) into a trimmed, de-duplicated, non-empty list.
export function parseList(input: string): string[] {
  const seen = new Set<string>();
  const out: string[] = [];
  for (const raw of input.split(/[\s,]+/)) {
    const v = raw.trim();
    if (v && !seen.has(v)) {
      seen.add(v);
      out.push(v);
    }
  }
  return out;
}

// formatList renders a list back into the comma-separated text the input shows.
export function formatList(items: string[] | undefined | null): string {
  return (items ?? []).join(", ");
}

// rejectsAllLogins is true when the allowed-domains list is empty. The server
// fails closed on an empty allow-list (every login rejected), so the control
// surfaces this as a warning rather than letting it pass silently.
export function rejectsAllLogins(domains: string[]): boolean {
  return domains.length === 0;
}
