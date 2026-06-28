import { useId, useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Pencil, Plus, Search, Trash2 } from "lucide-react";
import clsx from "clsx";
import {
  ApiError,
  api,
  type Member,
  type MemberInput,
  type MemberMeta,
  type MemberRole,
  type MembersEnvelope,
} from "@/lib/api";
import {
  ASSIGNABLE_ROLES,
  canAddMember,
  isAdminRole,
  memberAffordances,
  normalizeEmail,
  roleLabel,
} from "@/lib/role";
import { useEffectiveRole } from "@/lib/useEffectiveRole";
import { EmptyState, ErrorBox } from "@/components/feedback";
import { Pill } from "@/components/Pill";
import { Modal } from "@/components/Modal";
import { ConfirmDialog } from "@/components/ConfirmDialog";
import { RetryableError } from "@/components/RetryableError";
import { SkRows } from "@/components/Skeleton";
import { useToast } from "@/components/toastContext";

// RbacBinding is what the enterprise RBAC members surface contributes to one
// roster row: the member's effective role and the subject the role-change
// control targets. Keyed by normalized email since the OSS roster carries an
// email but no IdP subject.
interface RbacBinding {
  role?: string;
  subject: string;
}

// MembersPanel is the operator's roster of people that can be assigned
// to incidents. Each member has a free-form name, an editable alias
// (auto-derived from the name until the operator changes it), and a
// typed meta block of per-channel identifiers. Exported as a panel so
// PeoplePage can compose it as the Members tab.
export function MembersPanel() {
  const qc = useQueryClient();
  const toast = useToast();
  const { data, isLoading, isError, error, refetch, isRefetching } = useQuery({
    queryKey: ["members"],
    queryFn: api.listMembers,
  });

  // Enterprise RBAC overlay. rbacActive is true only on a licensed binary with
  // a live session — off it, the panel is the OSS roster exactly as today (no
  // role column, fully editable). The self match keys on the session EMAIL vs
  // each row's meta.email (normalized), since the OSS roster has no IdP subject.
  const access = useEffectiveRole();
  const rbacActive = access.enterprise && access.hasSession;
  const isAdmin = access.isAdmin;
  const selfEmail = normalizeEmail(access.session.data?.email);

  const rbacQ = useQuery<MembersEnvelope>({
    queryKey: ["rbac-members", access.org],
    queryFn: () => api.listRbacMembers(access.org as string),
    enabled: rbacActive && !!access.org,
    retry: (count, err) => {
      if (err instanceof ApiError && [401, 403, 404, 503].includes(err.status)) {
        return false;
      }
      return count < 1;
    },
    staleTime: 30_000,
  });

  // Join the RBAC members (role + subject) to the OSS roster by normalized
  // email, so each roster row can surface its role and (for an admin) a
  // role-change control that targets the right subject.
  const rbacByEmail = useMemo(() => {
    const m = new Map<string, RbacBinding>();
    for (const r of rbacQ.data?.members ?? []) {
      const key = normalizeEmail(r.email);
      if (key) m.set(key, { role: r.role, subject: r.subject });
    }
    return m;
  }, [rbacQ.data]);

  const [q, setQ] = useState("");
  const [editing, setEditing] = useState<Member | "new" | null>(null);
  const [deleting, setDeleting] = useState<Member | null>(null);
  const [pendingRoleSubject, setPendingRoleSubject] = useState<string | null>(
    null,
  );

  const filtered = useMemo(() => {
    if (!data) return [];
    const needle = q.trim().toLowerCase();
    if (!needle) return data;
    return data.filter(
      (m) =>
        m.name.toLowerCase().includes(needle) ||
        m.alias.toLowerCase().includes(needle) ||
        Object.values(m.meta || {}).some((v) =>
          (v ?? "").toString().toLowerCase().includes(needle),
        ),
    );
  }, [data, q]);

  const del = useMutation({
    mutationFn: (m: Member) => api.deleteMember(m.id),
    onSuccess: (_res, m) => {
      qc.invalidateQueries({ queryKey: ["members"] });
      qc.invalidateQueries({ queryKey: ["teams"] });
      setDeleting(null);
      toast.push({ tone: "ok", title: `Deleted ${m.name}` });
    },
    onError: (err, m) => {
      toast.push({
        tone: "error",
        title: `Couldn't delete ${m.name}`,
        description: err.message,
        action: { label: "Retry", onClick: () => del.mutate(m) },
      });
    },
  });

  const setRole = useMutation({
    mutationFn: (vars: { subject: string; role: MemberRole }) =>
      api.setMemberRole(access.org as string, vars.subject, vars.role),
    onMutate: (vars) => setPendingRoleSubject(vars.subject),
    onSuccess: (_res, vars) => {
      qc.invalidateQueries({ queryKey: ["rbac-members", access.org] });
      toast.push({ tone: "ok", title: `Role updated to ${roleLabel(vars.role)}` });
    },
    onError: (err) => {
      toast.push({
        tone: "error",
        title: "Couldn't change the role",
        description: err instanceof Error ? err.message : String(err),
      });
    },
    onSettled: () => setPendingRoleSubject(null),
  });

  const showAdd = canAddMember(rbacActive, isAdmin);
  const colCount = rbacActive ? 5 : 4;

  return (
    <>
      <div className="mb-3 flex flex-wrap items-center gap-2">
        <div className="relative max-w-md flex-1">
          <Search
            size={12}
            aria-hidden
            className="absolute left-2.5 top-1/2 -translate-y-1/2 text-ink-400"
          />
          <input
            className="input pl-7"
            data-page-search
            aria-label="Search members"
            placeholder="Search by name, alias, or channel id…"
            value={q}
            onChange={(e) => setQ(e.target.value)}
          />
        </div>
        {showAdd && (
          <button
            className="btn"
            data-testid="members-add"
            onClick={() => setEditing("new")}
          >
            <Plus size={12} /> Add member
          </button>
        )}
      </div>

      {isError && (
        <div className="mb-3">
          <RetryableError
            error={error}
            onRetry={() => refetch()}
            retrying={isRefetching}
            context="Couldn't load members"
          />
        </div>
      )}

      {(!isError || data) && (
        <div className="card overflow-hidden">
          <div className="max-h-[calc(100vh-260px)] overflow-auto">
            <table className="ddt">
              <thead>
                <tr>
                  <th className="w-56">Name</th>
                  <th className="w-40">Alias</th>
                  <th>Channel identifiers</th>
                  {rbacActive && <th className="w-40">Role</th>}
                  <th className="w-24" />
                </tr>
              </thead>
              <tbody>
                {isLoading && <SkRows rows={6} cols={colCount} />}
                {!isLoading && !isError && filtered.length === 0 && (
                  <tr>
                    <td colSpan={colCount}>
                      {q.trim() ? (
                        <EmptyState
                          title="No members match"
                          hint="Try a different search."
                        />
                      ) : (
                        <EmptyState
                          title="No members yet"
                          hint="Add operators here so you can assign them to incidents."
                          action={
                            showAdd ? (
                              <button
                                className="btn"
                                onClick={() => setEditing("new")}
                              >
                                <Plus size={12} /> Add member
                              </button>
                            ) : undefined
                          }
                        />
                      )}
                    </td>
                  </tr>
                )}
                {filtered.map((m) => {
                  const emailKey = normalizeEmail(m.meta?.email);
                  const binding = emailKey
                    ? rbacByEmail.get(emailKey)
                    : undefined;
                  const isSelf = !!selfEmail && emailKey === selfEmail;
                  const aff = memberAffordances({
                    rbacActive,
                    isAdmin,
                    isSelf,
                    hasSubject: !!binding?.subject,
                  });
                  return (
                    <tr key={m.id} data-testid={`member-row-${m.id}`}>
                      <td className="py-2.5 font-medium text-ink-50">
                        <span>{m.name}</span>
                        {rbacActive && isSelf && (
                          <span
                            data-testid={`member-self-${m.id}`}
                            className="ml-2 inline-flex items-center rounded-full bg-ink-100/10 px-1.5 py-0.5 text-2xs font-medium uppercase tracking-wide text-ink-300"
                          >
                            You
                          </span>
                        )}
                      </td>
                      <td className="font-mono text-2xs text-ink-300">
                        {m.alias}
                      </td>
                      <td>
                        <MetaPills meta={m.meta} />
                      </td>
                      {rbacActive && (
                        <td>
                          <MemberRoleCell
                            memberId={m.id}
                            role={binding?.role}
                            subject={binding?.subject}
                            canManage={aff.canManageRole}
                            pending={
                              !!binding?.subject &&
                              pendingRoleSubject === binding.subject
                            }
                            onChange={(role) =>
                              binding?.subject &&
                              setRole.mutate({ subject: binding.subject, role })
                            }
                          />
                        </td>
                      )}
                      <td>
                        <div className="flex justify-end gap-1">
                          {aff.canEdit && (
                            <button
                              className="btn"
                              data-testid={`member-edit-${m.id}`}
                              aria-label={`Edit ${m.name}`}
                              title="Edit"
                              onClick={() => setEditing(m)}
                            >
                              <Pencil size={11} />
                            </button>
                          )}
                          {aff.canDelete && (
                            <button
                              className="btn"
                              data-testid={`member-delete-${m.id}`}
                              aria-label={`Delete ${m.name}`}
                              title="Delete"
                              disabled={del.isPending}
                              onClick={() => {
                                del.reset();
                                setDeleting(m);
                              }}
                            >
                              <Trash2 size={11} />
                            </button>
                          )}
                        </div>
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        </div>
      )}

      {editing && (
        <MemberEditor
          member={editing === "new" ? null : editing}
          onClose={() => setEditing(null)}
        />
      )}

      {deleting && (
        <ConfirmDialog
          title={`Delete ${deleting.name}?`}
          message={
            <>
              They will be removed from every team they belong to. This can't
              be undone.
            </>
          }
          confirmLabel="Delete"
          tone="danger"
          busy={del.isPending}
          error={del.isError ? del.error : null}
          onConfirm={() => del.mutate(deleting)}
          onClose={() => setDeleting(null)}
        />
      )}
    </>
  );
}

// MemberRoleCell renders one roster row's RBAC role on the enterprise People
// page: a read-only pill for everyone, or — for an admin/owner on a row that
// maps to an RBAC subject — a live <select> that reassigns the role through the
// SAME assignRole path the Admin surface uses. A row with no RBAC subject (a
// roster member who never signed in via SSO) shows a muted "No SSO role".
function MemberRoleCell({
  memberId,
  role,
  subject,
  canManage,
  pending,
  onChange,
}: {
  memberId: string;
  role?: string;
  subject?: string;
  canManage: boolean;
  pending: boolean;
  onChange: (role: MemberRole) => void;
}) {
  if (canManage && subject) {
    return (
      <select
        data-testid={`member-role-${memberId}`}
        aria-label={`Role for ${subject}`}
        className="input h-8 py-0 text-xs"
        value={(role ?? "viewer").toLowerCase()}
        disabled={pending}
        onChange={(e) => onChange(e.target.value as MemberRole)}
      >
        {ASSIGNABLE_ROLES.map((r) => (
          <option key={r} value={r}>
            {roleLabel(r)}
          </option>
        ))}
      </select>
    );
  }
  if (!subject) {
    return (
      <span
        data-testid={`member-role-${memberId}`}
        className="text-2xs text-ink-400"
      >
        No SSO role
      </span>
    );
  }
  return (
    <span
      data-testid={`member-role-${memberId}`}
      className={clsx(
        "inline-flex items-center rounded-full px-1.5 py-0.5 text-2xs font-medium uppercase tracking-wide",
        isAdminRole(role)
          ? "bg-accent-subtle text-accent"
          : "bg-ink-100/10 text-ink-300",
      )}
    >
      {roleLabel(role)}
    </span>
  );
}


// MetaPills renders only the channel ids that are set, with a hover
// title spelling the channel out (the field name is too cryptic on
// its own — e.g. "msteams_upn").
function MetaPills({ meta }: { meta?: MemberMeta }) {
  if (!meta) return <span className="text-ink-400">—</span>;
  const entries: { k: keyof MemberMeta; label: string }[] = [
    { k: "email", label: "email" },
    { k: "slack_id", label: "slack" },
    { k: "telegram_id", label: "telegram" },
    { k: "msteams_upn", label: "msteams" },
    { k: "lark_id", label: "lark" },
    { k: "viber_id", label: "viber" },
    { k: "pagerduty_user_id", label: "pagerduty" },
    { k: "awsim_contact_arn", label: "awsim" },
    { k: "phone", label: "phone" },
  ];
  const set = entries.filter(({ k }) => !!meta[k]);
  if (set.length === 0) return <span className="text-ink-400">—</span>;
  return (
    <div className="flex flex-wrap gap-1">
      {set.map(({ k, label }) => (
        <span key={k} title={`${label}: ${meta[k]}`}>
          <Pill>{label}</Pill>
        </span>
      ))}
    </div>
  );
}

// deriveAlias mirrors pkg/teams.DeriveAlias so the live preview in the
// modal matches what the backend would compute when alias is left
// blank.
function deriveAlias(name: string): string {
  let out = "";
  let prevDash = true;
  for (const r of name.toLowerCase().trim()) {
    if ((r >= "a" && r <= "z") || (r >= "0" && r <= "9")) {
      out += r;
      prevDash = false;
    } else if (r === "-" || r === "_") {
      out += r;
      prevDash = false;
    } else {
      if (!prevDash) {
        out += "-";
        prevDash = true;
      }
    }
  }
  return out.replace(/^-+|-+$/g, "");
}

function MemberEditor({
  member,
  onClose,
}: {
  member: Member | null;
  onClose: () => void;
}) {
  const qc = useQueryClient();
  const toast = useToast();
  const isNew = member === null;
  const nameId = useId();
  const aliasId = useId();
  const [name, setName] = useState(member?.name ?? "");
  // Track whether the operator has edited the alias. While untouched
  // we keep it in sync with the auto-derived form.
  const [aliasTouched, setAliasTouched] = useState(
    !!member && member.alias !== deriveAlias(member.name),
  );
  const [alias, setAlias] = useState(member?.alias ?? "");
  const [meta, setMeta] = useState<MemberMeta>(member?.meta ?? {});

  const effectiveAlias = aliasTouched ? alias : deriveAlias(name);

  const save = useMutation({
    mutationFn: async () => {
      const trimmedName = name.trim();
      const body: MemberInput = {
        name: trimmedName || undefined,
        alias: aliasTouched ? alias.trim() : effectiveAlias,
        meta,
      };
      if (isNew) return api.createMember(body);
      return api.updateMember(member!.id, body);
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["members"] });
      toast.push({
        tone: "ok",
        title: isNew ? `Created ${name.trim()}` : `Saved ${name.trim()}`,
      });
      onClose();
    },
    onError: (err) => {
      toast.push({
        tone: "error",
        title: isNew ? "Couldn't create member" : "Couldn't save member",
        description: err.message,
      });
    },
  });

  return (
    <Modal
      title={isNew ? "Add member" : `Edit ${member!.name}`}
      onClose={onClose}
      size="lg"
      closeDisabled={save.isPending}
      footer={
        <>
          <button className="btn" onClick={onClose} disabled={save.isPending}>
            Cancel
          </button>
          <button
            className="btn btn-primary"
            onClick={() => save.mutate()}
            disabled={save.isPending || !name.trim()}
          >
            {save.isPending ? "Saving…" : isNew ? "Create" : "Save"}
          </button>
        </>
      }
    >
      <div className="max-h-[60vh] space-y-3 overflow-y-auto pr-1">
        <div>
          <label className="field-label" htmlFor={nameId}>
            Name
          </label>
          <input
            id={nameId}
            className="input"
            placeholder="e.g. Alice Cooper"
            value={name}
            autoFocus
            onChange={(e) => setName(e.target.value)}
          />
        </div>
        <div>
          <div className="flex items-center justify-between">
            <label className="field-label" htmlFor={aliasId}>
              Alias
            </label>
            {!aliasTouched && (
              <span className="text-2xs text-ink-400">auto from name</span>
            )}
          </div>
          <input
            id={aliasId}
            className="input font-mono"
            placeholder="alice-cooper"
            value={effectiveAlias}
            onChange={(e) => {
              setAliasTouched(true);
              setAlias(e.target.value);
            }}
          />
        </div>

        <div className="border-t border-ink-600 pt-3">
          <div className="mb-2 text-2xs uppercase tracking-wider text-ink-300">
            Channel identifiers
          </div>
          <div className="grid grid-cols-2 gap-3">
            <MetaField
              label="Email"
              hint="alice@example.com"
              value={meta.email}
              onChange={(v) => setMeta({ ...meta, email: v })}
            />
            <MetaField
              label="Slack ID"
              hint="U0123ABC (not @handle)"
              value={meta.slack_id}
              onChange={(v) => setMeta({ ...meta, slack_id: v })}
            />
            <MetaField
              label="Telegram ID"
              hint="numeric user id"
              value={meta.telegram_id}
              onChange={(v) => setMeta({ ...meta, telegram_id: v })}
            />
            <MetaField
              label="MS Teams UPN"
              hint="alice@contoso.com"
              value={meta.msteams_upn}
              onChange={(v) => setMeta({ ...meta, msteams_upn: v })}
            />
            <MetaField
              label="Lark ID"
              hint="open_id or union_id"
              value={meta.lark_id}
              onChange={(v) => setMeta({ ...meta, lark_id: v })}
            />
            <MetaField
              label="Viber ID"
              value={meta.viber_id}
              onChange={(v) => setMeta({ ...meta, viber_id: v })}
            />
            <MetaField
              label="PagerDuty user id"
              value={meta.pagerduty_user_id}
              onChange={(v) => setMeta({ ...meta, pagerduty_user_id: v })}
            />
            <MetaField
              label="AWS IM contact ARN"
              hint="arn:aws:ssm-contacts:…"
              value={meta.awsim_contact_arn}
              onChange={(v) => setMeta({ ...meta, awsim_contact_arn: v })}
            />
            <MetaField
              label="Phone"
              hint="E.164 (+15555550123)"
              value={meta.phone}
              onChange={(v) => setMeta({ ...meta, phone: v })}
            />
          </div>
        </div>

        {save.isError && <ErrorBox error={save.error} />}
      </div>
    </Modal>
  );
}

function MetaField({
  label,
  hint,
  value,
  onChange,
}: {
  label: string;
  hint?: string;
  value?: string;
  onChange: (v: string) => void;
}) {
  const id = useId();
  return (
    <div>
      <label className="field-label" htmlFor={id}>
        {label}
      </label>
      <input
        id={id}
        className="input"
        placeholder={hint}
        value={value ?? ""}
        onChange={(e) => onChange(e.target.value)}
      />
    </div>
  );
}
