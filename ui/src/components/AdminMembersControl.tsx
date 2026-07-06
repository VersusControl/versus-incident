import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  AlertCircle,
  Loader2,
  ShieldCheck,
  ShieldOff,
  Users,
} from "lucide-react";
import {
  ApiError,
  api,
  type BootstrapAdminStatus,
  type MemberRole,
  type MembersEnvelope,
  type MemberView,
} from "@/lib/api";
import {
  ASSIGNABLE_ROLES,
  adminGateState,
  bootstrapAdminAction,
  roleLabel,
  type AssignableRole,
} from "@/lib/role";
import { isNoOtherAdminError } from "@/lib/localAdmin";
import { useEffectiveRole } from "@/lib/useEffectiveRole";
import { AdminAccessNotice } from "@/components/AdminAccessNotice";
import { EnterpriseLockedBody } from "@/components/EnterpriseLocked";
import { ConfirmDialog } from "@/components/ConfirmDialog";
import { useToast } from "@/components/toastContext";

// AdminMembersControl — the operator surface for Enterprise RBAC membership.
// It lists provisioned members joined with their EFFECTIVE role and lets
// an admin assign a direct role to a member, plus manage the deployment's
// "default admin user" (the built-in non-SSO root account created on first
// licensed boot).
//
// Every request rides the SSO session cookie and is authorized by the caller's
// RBAC role (roles:manage, held by admin/owner).
// The panel is gated on the caller's effective role (useEffectiveRole):
//   not enterprise        → locked Enterprise upsell (no panel)
//   no SSO session         → "sign in to manage" notice
//   viewer / responder     → read-only "requires the admin role" notice
//   admin / owner          → the live members + default-admin panel
//
// Two no-lockout / no-escalation guards live here on top of the server's:
//   1. the caller cannot change their OWN role (the select is disabled for the
//      self row) — so an admin can never accidentally strand themselves.
//   2. "Disable default admin" is only offered when the server reports it is
//      safe (can_disable); the server still refuses (422 no_other_admin) if the
//      last admin would be lost.
export function AdminMembersControl() {
  const qc = useQueryClient();
  const toast = useToast();
  const access = useEffectiveRole();
  const org = access.org ?? "";
  const gate = adminGateState({
    loading: access.loading,
    enterprise: access.enterprise,
    hasSession: access.hasSession,
    isAdmin: access.isAdmin,
  });
  // The caller's own subject — used to refuse self role-changes (no lockout).
  const selfSubject = access.session.data?.subject ?? "";

  const members = useQuery<MembersEnvelope>({
    queryKey: ["rbac-members", org],
    queryFn: () => api.listRbacMembers(org),
    // Only an admin issues the privileged GET (fail closed for viewers).
    enabled: gate === "admin" && !!org,
    retry: (count, err) => {
      if (err instanceof ApiError && [401, 403, 404, 503].includes(err.status)) {
        return false;
      }
      return count < 1;
    },
  });

  const bootstrap = useQuery<BootstrapAdminStatus>({
    queryKey: ["rbac-bootstrap-admin", org],
    queryFn: () => api.getBootstrapAdmin(org),
    enabled: gate === "admin" && !!org,
    retry: (count, err) => {
      if (err instanceof ApiError && [401, 403, 404, 503].includes(err.status)) {
        return false;
      }
      return count < 1;
    },
  });

  const [pendingSubject, setPendingSubject] = useState<string | null>(null);
  const [confirmDisable, setConfirmDisable] = useState(false);

  const setRole = useMutation({
    mutationFn: (vars: { subject: string; role: MemberRole }) =>
      api.setMemberRole(org, vars.subject, vars.role),
    onMutate: (vars) => setPendingSubject(vars.subject),
    onSuccess: (_data, vars) => {
      toast.push({
        title: `Role updated to ${roleLabel(vars.role)}`,
        tone: "ok",
      });
      qc.invalidateQueries({ queryKey: ["rbac-members", org] });
      qc.invalidateQueries({ queryKey: ["rbac-bootstrap-admin", org] });
    },
    onError: (err) => {
      toast.push({
        title: "Couldn't change the role",
        description: err instanceof Error ? err.message : String(err),
        tone: "error",
      });
    },
    onSettled: () => setPendingSubject(null),
  });

  const disableAdmin = useMutation({
    mutationFn: () => api.disableBootstrapAdmin(org),
    onSuccess: () => {
      toast.push({ title: "Default admin user disabled", tone: "ok" });
      setConfirmDisable(false);
      qc.invalidateQueries({ queryKey: ["rbac-bootstrap-admin", org] });
      qc.invalidateQueries({ queryKey: ["rbac-members", org] });
    },
    onError: (err) => {
      // The no-lockout refusal (422 no_other_admin) is surfaced IN THE DOM on
      // the card (G3) as well as the toast — keep the dialog open so the in-card
      // message and the dialog error both explain the block.
      toast.push({
        title: "Couldn't disable the default admin",
        description: err instanceof Error ? err.message : String(err),
        tone: "error",
      });
    },
  });

  const enableAdmin = useMutation({
    mutationFn: () => api.enableBootstrapAdmin(org),
    onSuccess: () => {
      toast.push({ title: "Default admin user enabled", tone: "ok" });
      qc.invalidateQueries({ queryKey: ["rbac-bootstrap-admin", org] });
      qc.invalidateQueries({ queryKey: ["rbac-members", org] });
    },
    onError: (err) => {
      toast.push({
        title: "Couldn't enable the default admin",
        description: err instanceof Error ? err.message : String(err),
        tone: "error",
      });
    },
  });

  // ----- role gate (SSO session + RBAC role) ----------
  if (gate === "loading") {
    return (
      <MembersShell>
        <div className="flex items-center gap-2 text-xs text-ink-400">
          <Loader2 size={14} className="animate-spin" />
          Checking access…
        </div>
      </MembersShell>
    );
  }
  if (gate === "locked") {
    return (
      <MembersShell>
        <EnterpriseLockedBody title="Member & role management is an Enterprise capability">
          Assign roles to the people who sign in via SSO and manage the
          deployment's default admin. Available on Versus Enterprise.
        </EnterpriseLockedBody>
      </MembersShell>
    );
  }
  if (gate === "sign-in") {
    return (
      <MembersShell>
        <AdminAccessNotice reason="sign-in" />
      </MembersShell>
    );
  }
  if (gate === "read-only") {
    return (
      <MembersShell>
        <AdminAccessNotice reason="role" />
      </MembersShell>
    );
  }

  return (
    <MembersShell>
      <div className="flex flex-col gap-5">
        <BootstrapAdminCard
          query={bootstrap}
          onDisable={() => setConfirmDisable(true)}
          disabling={disableAdmin.isPending}
          disableError={disableAdmin.error}
          onEnable={() => enableAdmin.mutate()}
          enabling={enableAdmin.isPending}
        />

        <div>
          <div className="mb-2 flex items-center gap-1.5 text-2xs uppercase tracking-wider text-ink-400">
            <Users size={12} aria-hidden />
            Members
          </div>
          {members.isPending ? (
            <div className="flex items-center gap-2 text-xs text-ink-400">
              <Loader2 size={14} className="animate-spin" />
              Reading members…
            </div>
          ) : members.isError || !members.data ? (
            <div className="flex items-center justify-between gap-3 text-xs">
              <span className="flex items-center gap-1.5 text-sev-critical">
                <AlertCircle size={13} />
                {members.error instanceof Error
                  ? members.error.message
                  : "Couldn't read members."}
              </span>
              <button className="btn" onClick={() => members.refetch()}>
                Retry
              </button>
            </div>
          ) : (
            <MembersTable
              members={members.data.members}
              selfSubject={selfSubject}
              pendingSubject={pendingSubject}
              onChangeRole={(subject, role) => setRole.mutate({ subject, role })}
            />
          )}
        </div>
      </div>

      {confirmDisable && (
        <ConfirmDialog
          title="Disable default admin user"
          tone="danger"
          confirmLabel="Disable default admin"
          busy={disableAdmin.isPending}
          error={
            disableAdmin.error instanceof Error ? disableAdmin.error : null
          }
          message={
            <>
              The built-in default admin
              {bootstrap.data?.username ? (
                <>
                  {" "}
                  (<span className="font-mono text-ink-100">{bootstrap.data.username}</span>)
                </>
              ) : null}{" "}
              will no longer be able to sign in. Roles already assigned to
              other members are unaffected. This is only allowed while another
              owner or admin exists.
            </>
          }
          onConfirm={() => disableAdmin.mutate()}
          onClose={() => {
            if (!disableAdmin.isPending) setConfirmDisable(false);
          }}
        />
      )}
    </MembersShell>
  );
}

// BootstrapAdminCard — the "default admin user" panel. It reports whether the
// built-in default admin is configured and offers the no-lockout disable
// action only when the server says it is safe (can_disable). The server's
// no-lockout refusal (422 no_other_admin) is surfaced IN THE DOM here (G3), not
// only as a toast. A disabled admin can be re-enabled (owner break-glass).
function BootstrapAdminCard({
  query,
  onDisable,
  disabling,
  disableError,
  onEnable,
  enabling,
}: {
  query: ReturnType<typeof useQuery<BootstrapAdminStatus>>;
  onDisable: () => void;
  disabling: boolean;
  disableError: unknown;
  onEnable: () => void;
  enabling: boolean;
}) {
  if (query.isPending) {
    return (
      <div
        data-testid="builtin-admin-card"
        className="rounded-control border border-ink-600/60 bg-surface-sunken/40 p-3"
      >
        <div className="flex items-center gap-2 text-xs text-ink-400">
          <Loader2 size={14} className="animate-spin" />
          Reading default admin…
        </div>
      </div>
    );
  }
  if (query.isError || !query.data) {
    return (
      <div
        data-testid="builtin-admin-card"
        className="rounded-control border border-ink-600/60 bg-surface-sunken/40 p-3"
      >
        <div className="flex items-center justify-between gap-3 text-xs">
          <span className="flex items-center gap-1.5 text-sev-critical">
            <AlertCircle size={13} />
            Couldn't read the default admin status.
          </span>
          <button className="btn" onClick={() => query.refetch()}>
            Retry
          </button>
        </div>
      </div>
    );
  }

  const status = query.data;
  const action = bootstrapAdminAction(status);

  if (action === "absent") {
    return (
      <div
        data-testid="builtin-admin-card"
        className="rounded-control border border-ink-600/60 bg-surface-sunken/40 p-3"
      >
        <div className="mb-1 flex items-center gap-1.5 text-2xs uppercase tracking-wider text-ink-400">
          <ShieldCheck size={12} aria-hidden />
          Default admin user
        </div>
        <p className="text-xs text-ink-300">
          No built-in default admin is configured for this deployment.
        </p>
      </div>
    );
  }

  // The no-lockout block is shown in the DOM (not just a toast) when disabling
  // is unsafe — either proactively (the server reported can_disable=false) or
  // after a 422 no_other_admin refusal from a disable attempt.
  const blockedNoOtherAdmin =
    action === "locked" || isNoOtherAdminError(disableError);

  return (
    <div
      data-testid="builtin-admin-card"
      className="rounded-control border border-ink-600/60 bg-surface-sunken/40 p-3"
    >
      <div className="mb-1 flex items-center gap-1.5 text-2xs uppercase tracking-wider text-ink-400">
        <ShieldCheck size={12} aria-hidden />
        Default admin user
      </div>
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div className="text-xs text-ink-200">
          <span className="font-mono text-ink-100">{status.username}</span>
          <span className="ml-2 inline-flex items-center rounded-full border px-2 py-0.5 text-2xs font-medium">
            {action === "disabled" ? (
              <span className="text-ink-400">Disabled</span>
            ) : (
              <span className="text-sev-ok">Active</span>
            )}
          </span>
        </div>
        {action === "disabled" ? (
          <button
            className="btn inline-flex items-center gap-1.5"
            disabled={enabling}
            title="Re-enable the built-in default admin"
            onClick={onEnable}
          >
            {enabling ? (
              <Loader2 size={13} className="animate-spin" aria-hidden />
            ) : (
              <ShieldCheck size={13} aria-hidden />
            )}
            Enable default admin user
          </button>
        ) : (
          <button
            data-testid="builtin-admin-disable"
            className="btn btn-danger inline-flex items-center gap-1.5"
            disabled={action === "locked" || disabling}
            title={
              action === "locked"
                ? "Add another owner or admin first — disabling now would lock everyone out."
                : "Disable the built-in default admin"
            }
            onClick={onDisable}
          >
            <ShieldOff size={13} aria-hidden />
            Disable default admin user
          </button>
        )}
      </div>
      {blockedNoOtherAdmin && action !== "disabled" && (
        <p
          data-testid="builtin-admin-no-other-admin"
          role="alert"
          className="mt-2 text-2xs text-sev-warn"
        >
          Disabling is blocked: no other owner or admin exists yet. Grant the
          admin or owner role to another member first.
        </p>
      )}
    </div>
  );
}

// MembersTable — one row per provisioned member with a role selector. The
// selector is disabled for the caller's own row so an admin can never demote
// themselves into a lockout.
function MembersTable({
  members,
  selfSubject,
  pendingSubject,
  onChangeRole,
}: {
  members: MemberView[];
  selfSubject: string;
  pendingSubject: string | null;
  onChangeRole: (subject: string, role: MemberRole) => void;
}) {
  if (members.length === 0) {
    return (
      <p className="text-xs text-ink-400">
        No members yet. People appear here after they first sign in via SSO.
      </p>
    );
  }
  return (
    <div className="overflow-x-auto">
      <table className="w-full text-left text-xs">
        <thead>
          <tr className="border-b border-ink-700 text-2xs uppercase tracking-wider text-ink-400">
            <th className="py-2 pr-3 font-medium">Email</th>
            <th className="py-2 pr-3 font-medium">Name</th>
            <th className="py-2 pr-3 font-medium">Connection</th>
            <th className="py-2 pr-3 font-medium">Role</th>
          </tr>
        </thead>
        <tbody>
          {members.map((m) => {
            const isSelf = m.subject === selfSubject;
            const busy = pendingSubject === m.subject;
            const current = normalizeRole(m.role);
            return (
              <tr
                key={m.subject}
                data-testid={`member-row-${m.subject}`}
                className="border-b border-ink-800/60"
              >
                <td className="py-2 pr-3 align-middle text-ink-100">
                  {m.email || <span className="text-ink-400">—</span>}
                </td>
                <td className="py-2 pr-3 align-middle text-ink-200">
                  {m.name || <span className="text-ink-400">—</span>}
                </td>
                <td className="py-2 pr-3 align-middle text-ink-300">
                  {m.connection || <span className="text-ink-400">—</span>}
                </td>
                <td className="py-2 pr-3 align-middle">
                  <div className="flex items-center gap-2">
                    <select
                      className="input h-7 py-0 text-xs"
                      data-testid={`member-role-${m.subject}`}
                      aria-label={`Role for ${m.email || m.subject}`}
                      value={current}
                      disabled={isSelf || busy}
                      onChange={(e) =>
                        onChangeRole(m.subject, e.target.value as MemberRole)
                      }
                    >
                      {ASSIGNABLE_ROLES.map((r) => (
                        <option key={r} value={r}>
                          {roleLabel(r)}
                        </option>
                      ))}
                    </select>
                    {busy && (
                      <Loader2 size={13} className="animate-spin text-ink-400" />
                    )}
                    {isSelf && (
                      <span className="text-2xs text-ink-400">you</span>
                    )}
                  </div>
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}

// normalizeRole maps an empty / unknown effective role to "viewer" (least
// privilege) so the selector always reflects a concrete, server-known value.
function normalizeRole(role: string | undefined): AssignableRole {
  const r = (role ?? "").trim().toLowerCase();
  return (ASSIGNABLE_ROLES as readonly string[]).includes(r)
    ? (r as AssignableRole)
    : "viewer";
}

// MembersShell — the consistent card chrome every state renders inside.
function MembersShell({ children }: { children: React.ReactNode }) {
  return (
    <div className="card mb-4" data-testid="members-control">
      <div className="card-header">
        <h2 className="card-title">Members &amp; roles</h2>
        <span className="text-2xs text-ink-400">Enterprise control</span>
      </div>
      <div className="card-body">{children}</div>
    </div>
  );
}
