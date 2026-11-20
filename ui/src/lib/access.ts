import { useQueryClient } from "@tanstack/react-query";
import { ApiError } from "@/lib/api";
import { useEffectiveRole } from "@/lib/useEffectiveRole";

// Which pages the signed-in caller can open, from the permissions the
// Enterprise session whoami lists. The server decides every request; this
// only keeps the shell from offering pages, or sending requests, the role
// is refused. When the session lists no permissions (an OSS binary, a
// gateway-secret operator, an older server) nothing is hidden.

/** The permission each area of the app needs, as the Enterprise RBAC names it. */
export const PERMISSION = {
  incidents: "incidents:view",
  infrastructure: "infrastructure:view",
  finops: "finops:view",
} as const;

/**
 * An area of the app, by the permission that opens it. `chat` stays open to
 * every signed-in role: the server limits which tools a turn may use.
 */
export type Area = "incidents" | "infrastructure" | "finops" | "chat";

/** The area a route belongs to. Anything not listed reads incidents. */
export function pathArea(pathname: string): Area {
  const p = pathname.toLowerCase();
  const under = (base: string) => p === base || p.startsWith(`${base}/`);
  if (under("/agent/chat")) return "chat";
  if (under("/aws/finops")) return "finops";
  if (under("/aws") || under("/agent/kubernetes")) return "infrastructure";
  return "incidents";
}

/**
 * The permissions the session lists, or null when it lists none. Roles and
 * permissions both empty mean the server could not read the grants, which
 * is not the same as "no access": nothing is hidden then.
 */
export function sessionPermissions(session: { roles?: string[] | null; permissions?: string[] | null } | undefined): string[] | null {
  if (!session || !Array.isArray(session.permissions)) return null;
  if (session.permissions.length === 0 && (!Array.isArray(session.roles) || session.roles.length === 0)) return null;
  return session.permissions;
}

/** Whether `permissions` open `area`; null permissions open everything. */
export function canOpen(area: Area, permissions: readonly string[] | null): boolean {
  if (permissions === null || area === "chat") return true;
  const has = (p: string) => permissions.includes(p);
  switch (area) {
    case "finops":
      // Every role that reads infrastructure reads cost too.
      return has(PERMISSION.finops) || has(PERMISSION.infrastructure);
    case "infrastructure":
      return has(PERMISSION.infrastructure);
    default:
      return has(PERMISSION.incidents);
  }
}

/**
 * Whether the caller may list chat sessions. Sessions are shared, so the
 * list needs a permission beyond cost: a finance-only role chats without
 * it. Null permissions (nothing listed) allow it; the server still decides.
 */
export function mayListChatSessions(permissions: readonly string[] | null): boolean {
  return permissions === null || canOpen("incidents", permissions) || canOpen("infrastructure", permissions);
}

/** The first page a caller lands on: Now when it can read incidents, else FinOps, else chat. */
export function landingPath(permissions: readonly string[] | null): string {
  if (canOpen("incidents", permissions)) return "/now";
  if (canOpen("finops", permissions)) return "/aws/finops";
  return "/agent/chat";
}

/** The role the badge names: the most privileged one the session lists, else `role`. */
export function badgeRole(session: { role?: string | null; roles?: string[] | null } | undefined): string | null {
  const first = session?.roles?.find((r) => r.trim() !== "");
  return first ?? session?.role ?? null;
}

/** Any 403: the caller's role is refused this route. A final answer, never retried. */
export function isForbidden(error: unknown): boolean {
  return error instanceof ApiError && error.status === 403;
}

/** A react-query `retry` that never retries a 403 and retries anything else once. */
export function retryUnlessForbidden(failureCount: number, error: unknown): boolean {
  return !isForbidden(error) && failureCount < 1;
}

/**
 * A react-query `retry` that never retries a 403 and otherwise follows the
 * client's default, so a page keeps the app's retry policy for real errors.
 */
export function useRetryUnlessForbidden(): (failureCount: number, error: unknown) => boolean {
  const fallback = useQueryClient().getDefaultOptions().queries?.retry;
  return (failureCount, error) => {
    if (isForbidden(error)) return false;
    if (typeof fallback === "function") return fallback(failureCount, error as Error);
    if (typeof fallback === "number") return failureCount < fallback;
    return fallback === undefined ? failureCount < 3 : fallback;
  };
}

export interface AccessState {
  /** False while the deployment org or the session is still resolving. */
  ready: boolean;
  /** The session's permissions, or null when nothing is hidden. */
  permissions: string[] | null;
  /** Whether the caller can open `area`. True while unresolved. */
  can: (area: Area) => boolean;
  /**
   * Whether a background read of `area` may run now: resolved and allowed,
   * so a role is never sent a request the server will refuse.
   */
  mayRead: (area: Area) => boolean;
}

export function useAccess(): AccessState {
  const role = useEffectiveRole();
  const permissions = sessionPermissions(role.session.data);
  const ready = !role.loading;
  return {
    ready,
    permissions,
    can: (area) => canOpen(area, permissions),
    mayRead: (area) => ready && canOpen(area, permissions),
  };
}
