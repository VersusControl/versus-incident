// Effective-role helpers for the enterprise admin surface. The SPA gates every
// privileged control on the caller's RBAC role (carried by the SSO session
// whoami). Only admin/owner — the "admin user" roles — may manage enterprise
// configuration; viewer and responder are read-only. Kept pure (no React, no
// fetch) so the gating logic is unit-testable in isolation.

// ADMIN_ROLES are the roles that may manage enterprise configuration (runtime
// mode, AI settings, SSO connections/policy, members). They correspond to the
// server permission set (runtime:manage / sso:manage / roles:manage), which
// both admin and owner hold.
const ADMIN_ROLES = new Set(["admin", "owner"]);

// isAdminRole reports whether a role string may manage enterprise config.
// Unknown / empty / non-admin roles are read-only — fail closed, never assume
// privilege the server did not grant.
export function isAdminRole(role: string | null | undefined): boolean {
  return role != null && ADMIN_ROLES.has(role.trim().toLowerCase());
}

// roleLabel is the human label for a role chip. An empty / unknown role reads
// "Viewer" (least privilege) so the UI never implies more access than the
// server granted.
export function roleLabel(role: string | null | undefined): string {
  switch ((role ?? "").trim().toLowerCase()) {
    case "owner":
      return "Owner";
    case "admin":
      return "Admin";
    case "responder":
      return "Responder";
    default:
      return "Viewer";
  }
}

// AdminGateState is how a privileged enterprise control routes itself off the
// caller's session + role. It is the SINGLE decision the runtime-mode, AI,
// SSO-connections and members controls share, so the gating is identical and
// unit-testable in isolation (no DOM):
//
//   "loading"   — the deployment org / session is still resolving.
//   "locked"    — not an enterprise binary (community / OSS): render the upsell.
//   "sign-in"   — enterprise, but no live SSO session (a gateway-secret/data-
//                 plane operator): managing requires an org sign-in.
//   "read-only" — signed in, but the role is viewer/responder: show the
//                 "requires the admin role" notice, never a control.
//   "admin"     — admin/owner: render the live control.
export type AdminGateState =
  | "loading"
  | "locked"
  | "sign-in"
  | "read-only"
  | "admin";

// adminGateState is the pure decision the privileged controls run. It fails
// closed at every step — an unresolved/absent session or a non-admin role can
// never reach "admin", so a control is only ever live for a verified admin.
export function adminGateState(input: {
  // loading is true while the deployment org or the session whoami resolves.
  loading: boolean;
  // enterprise is true once the license-issued deployment org resolves (the
  // binary is licensed); false on a community / OSS binary.
  enterprise: boolean;
  // hasSession is true once a live SSO session whoami resolves.
  hasSession: boolean;
  // isAdmin is true when the resolved role may manage enterprise config.
  isAdmin: boolean;
}): AdminGateState {
  if (input.loading) return "loading";
  if (!input.enterprise) return "locked";
  if (!input.hasSession) return "sign-in";
  if (!input.isAdmin) return "read-only";
  return "admin";
}

// ASSIGNABLE_ROLES are the roles the Members surface lets an admin assign,
// ordered least- to most-privileged. Mirrors the server's MemberRole set
// (viewer / responder / admin / owner) one-for-one.
export const ASSIGNABLE_ROLES = [
  "viewer",
  "responder",
  "admin",
  "owner",
] as const;
export type AssignableRole = (typeof ASSIGNABLE_ROLES)[number];

// BootstrapAdminAction is the state of the "default admin user" disable control
// on the Members surface, derived purely from the server status so the
// no-lockout guard is testable without a DOM:
//
//   "absent"   — no built-in default admin is configured: nothing to render.
//   "disabled" — the built-in default admin is already turned off.
//   "locked"   — configured + active, but disabling now would strand the
//                deployment (the server reports no OTHER owner/admin), so the
//                action is blocked client-side too. Never offer a lockout.
//   "ready"    — configured + active + safe to disable.
export type BootstrapAdminAction = "absent" | "disabled" | "locked" | "ready";

export function bootstrapAdminAction(status: {
  configured: boolean;
  disabled?: boolean;
  can_disable?: boolean;
}): BootstrapAdminAction {
  if (!status.configured) return "absent";
  if (status.disabled) return "disabled";
  if (!status.can_disable) return "locked";
  return "ready";
}

// normalizeEmail lower-cases and trims an email so the People page can match a
// roster row to the signed-in session WITHOUT mismatching on case or stray
// whitespace. It mirrors the server's normalizeRosterEmail exactly, so the UI
// gating and the server enforcement agree on what "your own row" means.
export function normalizeEmail(s: string | null | undefined): string {
  return (s ?? "").trim().toLowerCase();
}

// MemberAffordances is what the People → Members surface may do with one roster
// row, derived purely so it is unit-testable without a DOM. It encodes the
// enterprise RBAC contract while leaving OSS behavior untouched:
//
//   community / OSS (rbacActive=false) — fully editable, exactly as today, and
//     no role column (canManageRole is always false off the enterprise path).
//   admin / owner — edit + delete any row, and change the row's role (only when
//     the row maps to an RBAC subject the assignRole call can target).
//   normal signed-in user — edit ONLY their own row's info; never delete, never
//     change a role (not even their own).
export interface MemberAffordances {
  canEdit: boolean;
  canDelete: boolean;
  canManageRole: boolean;
}

export function memberAffordances(input: {
  // rbacActive is true only on a licensed binary with a live session — the
  // single switch between OSS (open) and enterprise (gated) behavior.
  rbacActive: boolean;
  // isAdmin is true for admin/owner (may manage everyone).
  isAdmin: boolean;
  // isSelf is true when this row is the signed-in user's own row.
  isSelf: boolean;
  // hasSubject is true when the row maps to an RBAC subject, so a role change
  // has a target. A roster member who never signed in via SSO has none.
  hasSubject: boolean;
}): MemberAffordances {
  if (!input.rbacActive) {
    return { canEdit: true, canDelete: true, canManageRole: false };
  }
  if (input.isAdmin) {
    return { canEdit: true, canDelete: true, canManageRole: input.hasSubject };
  }
  return { canEdit: input.isSelf, canDelete: false, canManageRole: false };
}

// canAddMember / canManageTeams gate the People-page CREATE and team-management
// affordances. On OSS they are always open; on an enterprise binary with a live
// session only admin/owner may add roster members or manage teams (a normal
// user has no "own team", so team management is admin-only). Fail closed.
export function canAddMember(rbacActive: boolean, isAdmin: boolean): boolean {
  return !rbacActive || isAdmin;
}

export function canManageTeams(rbacActive: boolean, isAdmin: boolean): boolean {
  return !rbacActive || isAdmin;
}
