import { describe, it, expect } from "vitest";
import {
  ASSIGNABLE_ROLES,
  adminGateState,
  bootstrapAdminAction,
  canAddMember,
  canManageTeams,
  isAdminRole,
  memberAffordances,
  normalizeEmail,
  roleLabel,
} from "@/lib/role";

// Pure-logic tests for the RBAC role helpers the enterprise admin SPA gates on.
// The console has no DOM test harness, so every privileged-gating decision is
// kept pure here and pinned. The contracts that matter:
//   1. only admin/owner may manage; everything else (incl. unknown) is closed.
//   2. adminGateState fails closed at every step — a non-admin / sessionless /
//      community caller can never reach the live "admin" state.
//   3. the default-admin disable action is never offered when it would lock the
//      deployment out (bootstrapAdminAction "locked").

describe("isAdminRole", () => {
  it("admits owner and admin (case / whitespace insensitive)", () => {
    expect(isAdminRole("owner")).toBe(true);
    expect(isAdminRole("admin")).toBe(true);
    expect(isAdminRole("  Owner  ")).toBe(true);
    expect(isAdminRole("ADMIN")).toBe(true);
  });

  it("denies read-only and unknown / empty roles (fail closed)", () => {
    expect(isAdminRole("viewer")).toBe(false);
    expect(isAdminRole("responder")).toBe(false);
    expect(isAdminRole("")).toBe(false);
    expect(isAdminRole("superuser")).toBe(false);
    expect(isAdminRole(null)).toBe(false);
    expect(isAdminRole(undefined)).toBe(false);
  });
});

describe("roleLabel", () => {
  it("maps each role to its human label", () => {
    expect(roleLabel("owner")).toBe("Owner");
    expect(roleLabel("admin")).toBe("Admin");
    expect(roleLabel("responder")).toBe("Responder");
    expect(roleLabel("viewer")).toBe("Viewer");
  });

  it("reads an empty / unknown role as Viewer (least privilege)", () => {
    expect(roleLabel("")).toBe("Viewer");
    expect(roleLabel(null)).toBe("Viewer");
    expect(roleLabel(undefined)).toBe("Viewer");
    expect(roleLabel("nonsense")).toBe("Viewer");
  });
});

describe("adminGateState", () => {
  const base = {
    loading: false,
    enterprise: true,
    hasSession: true,
    isAdmin: true,
  };

  it("is 'loading' until everything resolves", () => {
    expect(adminGateState({ ...base, loading: true })).toBe("loading");
    // loading wins even if other flags look closed.
    expect(
      adminGateState({
        loading: true,
        enterprise: false,
        hasSession: false,
        isAdmin: false,
      }),
    ).toBe("loading");
  });

  it("is 'locked' on a non-enterprise (community / OSS) binary", () => {
    expect(adminGateState({ ...base, enterprise: false })).toBe("locked");
  });

  it("is 'sign-in' when enterprise but no live SSO session", () => {
    expect(adminGateState({ ...base, hasSession: false })).toBe("sign-in");
  });

  it("is 'read-only' for a signed-in viewer / responder", () => {
    expect(adminGateState({ ...base, isAdmin: false })).toBe("read-only");
  });

  it("is 'admin' only for a resolved, signed-in admin/owner", () => {
    expect(adminGateState(base)).toBe("admin");
  });

  it("never reaches 'admin' without a session, even if isAdmin is set", () => {
    expect(
      adminGateState({ ...base, hasSession: false, isAdmin: true }),
    ).toBe("sign-in");
  });
});

describe("bootstrapAdminAction", () => {
  it("is 'absent' when no default admin is configured", () => {
    expect(bootstrapAdminAction({ configured: false })).toBe("absent");
  });

  it("is 'disabled' once the built-in admin is turned off", () => {
    expect(
      bootstrapAdminAction({ configured: true, disabled: true, can_disable: false }),
    ).toBe("disabled");
  });

  it("is 'locked' when disabling would strand the deployment", () => {
    expect(
      bootstrapAdminAction({ configured: true, disabled: false, can_disable: false }),
    ).toBe("locked");
  });

  it("is 'ready' only when configured, active, and safe to disable", () => {
    expect(
      bootstrapAdminAction({ configured: true, disabled: false, can_disable: true }),
    ).toBe("ready");
  });
});

describe("ASSIGNABLE_ROLES", () => {
  it("lists the four server roles least- to most-privileged", () => {
    expect([...ASSIGNABLE_ROLES]).toEqual([
      "viewer",
      "responder",
      "admin",
      "owner",
    ]);
  });
});

describe("normalizeEmail", () => {
  it("lower-cases and trims so the self match never mismatches on case", () => {
    expect(normalizeEmail("  Alice@Example.com ")).toBe("alice@example.com");
    expect(normalizeEmail("BOB@ACME.TEST")).toBe("bob@acme.test");
  });

  it("maps null / undefined / blank to the empty string (no false match)", () => {
    expect(normalizeEmail(null)).toBe("");
    expect(normalizeEmail(undefined)).toBe("");
    expect(normalizeEmail("   ")).toBe("");
  });
});

// The People → Members affordances are the load-bearing contract: a normal user
// edits only their own row, an admin edits everyone, and OSS is untouched.
describe("memberAffordances", () => {
  it("leaves OSS / community fully editable, with no role control", () => {
    const aff = memberAffordances({
      rbacActive: false,
      isAdmin: false,
      isSelf: false,
      hasSubject: false,
    });
    expect(aff).toEqual({ canEdit: true, canDelete: true, canManageRole: false });
  });

  it("lets an admin/owner edit, delete, and (with a subject) change any role", () => {
    expect(
      memberAffordances({
        rbacActive: true,
        isAdmin: true,
        isSelf: false,
        hasSubject: true,
      }),
    ).toEqual({ canEdit: true, canDelete: true, canManageRole: true });
    // No RBAC subject on the row → role change has no target, so it is off.
    expect(
      memberAffordances({
        rbacActive: true,
        isAdmin: true,
        isSelf: false,
        hasSubject: false,
      }),
    ).toEqual({ canEdit: true, canDelete: true, canManageRole: false });
  });

  it("lets a normal user edit ONLY their own row — never delete, never role", () => {
    // Own row: edit only.
    expect(
      memberAffordances({
        rbacActive: true,
        isAdmin: false,
        isSelf: true,
        hasSubject: true,
      }),
    ).toEqual({ canEdit: true, canDelete: false, canManageRole: false });
    // Someone else's row: fully view-only.
    expect(
      memberAffordances({
        rbacActive: true,
        isAdmin: false,
        isSelf: false,
        hasSubject: true,
      }),
    ).toEqual({ canEdit: false, canDelete: false, canManageRole: false });
  });
});

describe("canAddMember / canManageTeams", () => {
  it("are open on OSS and admin-only on an enterprise binary", () => {
    // OSS: always open regardless of role.
    expect(canAddMember(false, false)).toBe(true);
    expect(canManageTeams(false, false)).toBe(true);
    // Enterprise: only admin/owner.
    expect(canAddMember(true, false)).toBe(false);
    expect(canAddMember(true, true)).toBe(true);
    expect(canManageTeams(true, false)).toBe(false);
    expect(canManageTeams(true, true)).toBe(true);
  });
});
