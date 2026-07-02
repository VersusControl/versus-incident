import { useId, useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Pencil, Plus, Search, Trash2 } from "lucide-react";
import {
  api,
  type Member,
  type Team,
  type TeamInput,
} from "@/lib/api";
import { canManageTeams } from "@/lib/role";
import { useEffectiveRole } from "@/lib/useEffectiveRole";
import { usePagination } from "@/lib/pagination";
import { EmptyState, ErrorBox } from "@/components/feedback";
import { Pill } from "@/components/Pill";
import { Modal } from "@/components/Modal";
import { ConfirmDialog } from "@/components/ConfirmDialog";
import { Pagination } from "@/components/Pagination";
import { RetryableError } from "@/components/RetryableError";
import { SkRows } from "@/components/Skeleton";
import { useToast } from "@/components/toastContext";

// TeamsPanel lets operators group members into named teams. Teams hold
// an ordered MemberIDs list — the order is preserved by the backend and
// reflected in the picker below. Teams are surfaced on the incident
// detail page so an incident can be assigned to a team plus an explicit
// subset of members. Exported as a panel so PeoplePage can compose it
// as the Teams tab.
export function TeamsPanel() {
  const qc = useQueryClient();
  const toast = useToast();
  const teamsQ = useQuery({ queryKey: ["teams"], queryFn: api.listTeams });
  const membersQ = useQuery({
    queryKey: ["members"],
    queryFn: api.listMembers,
  });

  // Enterprise RBAC: on a licensed binary with a live session only admin/owner
  // may manage teams (a normal user has no "own team"). Off the enterprise path
  // teams are fully editable, exactly as today.
  const access = useEffectiveRole();
  const rbacActive = access.enterprise && access.hasSession;
  const canManage = canManageTeams(rbacActive, access.isAdmin);

  const [q, setQ] = useState("");
  const [editing, setEditing] = useState<Team | "new" | null>(null);
  const [deleting, setDeleting] = useState<Team | null>(null);

  const memberById = useMemo(() => {
    const m = new Map<string, Member>();
    for (const x of membersQ.data ?? []) m.set(x.id, x);
    return m;
  }, [membersQ.data]);

  const filtered = useMemo(() => {
    if (!teamsQ.data) return [];
    const needle = q.trim().toLowerCase();
    if (!needle) return teamsQ.data;
    return teamsQ.data.filter(
      (t) =>
        t.name.toLowerCase().includes(needle) ||
        t.alias.toLowerCase().includes(needle) ||
        (t.description ?? "").toLowerCase().includes(needle),
    );
  }, [teamsQ.data, q]);

  // Paginate at 100/page AFTER search; reset to page 1 when the search changes.
  const pg = usePagination(filtered, { resetKey: q });

  const del = useMutation({
    mutationFn: (t: Team) => api.deleteTeam(t.id),
    onSuccess: (_res, t) => {
      qc.invalidateQueries({ queryKey: ["teams"] });
      setDeleting(null);
      toast.push({ tone: "ok", title: `Deleted team "${t.name}"` });
    },
    onError: (err, t) => {
      toast.push({
        tone: "error",
        title: `Couldn't delete team "${t.name}"`,
        description: err.message,
        action: { label: "Retry", onClick: () => del.mutate(t) },
      });
    },
  });

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
            aria-label="Search teams"
            placeholder="Search by name, alias, or description…"
            value={q}
            onChange={(e) => setQ(e.target.value)}
          />
        </div>
        {canManage && (
          <button
            className="btn"
            data-testid="teams-add"
            onClick={() => setEditing("new")}
          >
            <Plus size={12} /> Add team
          </button>
        )}
      </div>

      {teamsQ.isError && (
        <div className="mb-3">
          <RetryableError
            error={teamsQ.error}
            onRetry={() => teamsQ.refetch()}
            retrying={teamsQ.isRefetching}
            context="Couldn't load teams"
          />
        </div>
      )}
      {membersQ.isError && (
        <div className="mb-3">
          <RetryableError
            error={membersQ.error}
            onRetry={() => membersQ.refetch()}
            retrying={membersQ.isRefetching}
            context="Couldn't load members — team rosters show raw ids"
          />
        </div>
      )}

      {(!teamsQ.isError || teamsQ.data) && (
        <div className="card overflow-hidden">
          <div className="max-h-[calc(100vh-260px)] overflow-auto">
            <table className="ddt">
              <thead>
                <tr>
                  <th className="w-56">Name</th>
                  <th className="w-40">Alias</th>
                  <th>Members</th>
                  <th className="w-24" />
                </tr>
              </thead>
              <tbody>
                {teamsQ.isLoading && <SkRows rows={5} cols={4} />}
                {!teamsQ.isLoading &&
                  !teamsQ.isError &&
                  filtered.length === 0 && (
                    <tr>
                      <td colSpan={4}>
                        {q.trim() ? (
                          <EmptyState
                            title="No teams match"
                            hint="Try a different search."
                          />
                        ) : (
                          <EmptyState
                            title="No teams yet"
                            hint="Group members into teams so you can assign incidents to a whole team."
                          />
                        )}
                      </td>
                    </tr>
                  )}
                {pg.pageItems.map((t) => (
                  <tr key={t.id}>
                    <td className="py-2.5">
                      <div className="font-medium text-ink-50">{t.name}</div>
                      {t.description && (
                        <div className="text-2xs text-ink-300">
                          {t.description}
                        </div>
                      )}
                    </td>
                    <td className="font-mono text-2xs text-ink-300">
                      {t.alias}
                    </td>
                    <td>
                      {t.member_ids.length === 0 ? (
                        <span className="text-ink-400">—</span>
                      ) : (
                        <div className="flex flex-wrap gap-1">
                          {t.member_ids.map((id) => {
                            const m = memberById.get(id);
                            return (
                              <Pill key={id}>
                                {m ? m.name : id.slice(0, 8)}
                              </Pill>
                            );
                          })}
                        </div>
                      )}
                    </td>
                    <td>
                      <div className="flex justify-end gap-1">
                        {canManage && (
                          <>
                            <button
                              className="btn"
                              data-testid={`team-edit-${t.id}`}
                              aria-label={`Edit team ${t.name}`}
                              title="Edit"
                              onClick={() => setEditing(t)}
                            >
                              <Pencil size={11} />
                            </button>
                            <button
                              className="btn"
                              data-testid={`team-delete-${t.id}`}
                              aria-label={`Delete team ${t.name}`}
                              title="Delete"
                              disabled={del.isPending}
                              onClick={() => {
                                del.reset();
                                setDeleting(t);
                              }}
                            >
                              <Trash2 size={11} />
                            </button>
                          </>
                        )}
                      </div>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          <Pagination state={pg} />
        </div>
      )}

      {editing && (
        <TeamEditor
          team={editing === "new" ? null : editing}
          members={membersQ.data ?? []}
          onClose={() => setEditing(null)}
        />
      )}

      {deleting && (
        <ConfirmDialog
          title={`Delete team "${deleting.name}"?`}
          message={
            <>
              Members stay in the roster — only the grouping is removed. This
              can't be undone.
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


// deriveAlias mirrors pkg/teams.DeriveAlias — kept in sync with the
// matching helper in MembersPage. Duplicated rather than exported to
// keep page files independent.
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

function TeamEditor({
  team,
  members,
  onClose,
}: {
  team: Team | null;
  members: Member[];
  onClose: () => void;
}) {
  const qc = useQueryClient();
  const toast = useToast();
  const isNew = team === null;
  const nameId = useId();
  const aliasId = useId();
  const descriptionId = useId();
  const [name, setName] = useState(team?.name ?? "");
  const [aliasTouched, setAliasTouched] = useState(
    !!team && team.alias !== deriveAlias(team.name),
  );
  const [alias, setAlias] = useState(team?.alias ?? "");
  const [description, setDescription] = useState(team?.description ?? "");
  const [memberIDs, setMemberIDs] = useState<string[]>(team?.member_ids ?? []);

  const effectiveAlias = aliasTouched ? alias : deriveAlias(name);

  const selected = new Set(memberIDs);
  const toggle = (id: string) => {
    if (selected.has(id)) {
      setMemberIDs(memberIDs.filter((x) => x !== id));
    } else {
      setMemberIDs([...memberIDs, id]);
    }
  };

  const save = useMutation({
    mutationFn: async () => {
      const body: TeamInput = {
        name: name.trim() || undefined,
        alias: aliasTouched ? alias.trim() : effectiveAlias,
        description: description.trim(),
        member_ids: memberIDs,
      };
      if (isNew) return api.createTeam(body);
      return api.updateTeam(team!.id, body);
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["teams"] });
      toast.push({
        tone: "ok",
        title: isNew
          ? `Created team "${name.trim()}"`
          : `Saved team "${name.trim()}"`,
      });
      onClose();
    },
    onError: (err) => {
      toast.push({
        tone: "error",
        title: isNew ? "Couldn't create team" : "Couldn't save team",
        description: err.message,
      });
    },
  });

  return (
    <Modal
      title={isNew ? "Add team" : `Edit ${team!.name}`}
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
            placeholder="e.g. Platform Team"
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
            placeholder="platform-team"
            value={effectiveAlias}
            onChange={(e) => {
              setAliasTouched(true);
              setAlias(e.target.value);
            }}
          />
        </div>
        <div>
          <label className="field-label" htmlFor={descriptionId}>
            Description (optional)
          </label>
          <input
            id={descriptionId}
            className="input"
            placeholder="Owns ingestion, agent, and on-call routing."
            value={description}
            onChange={(e) => setDescription(e.target.value)}
          />
        </div>

        <div className="border-t border-ink-600 pt-3">
          <div className="mb-2 flex items-center justify-between">
            <div className="text-2xs uppercase tracking-wider text-ink-300">
              Members
            </div>
            <div className="text-2xs text-ink-400">
              {memberIDs.length} selected · order preserved
            </div>
          </div>
          {members.length === 0 ? (
            <p className="text-2xs text-ink-400">
              No members yet. Add a few from the Members tab first.
            </p>
          ) : (
            <div className="max-h-64 space-y-1 overflow-auto rounded-md border border-ink-600 bg-surface-sunken p-2">
              {members.map((m) => (
                <label
                  key={m.id}
                  className="flex cursor-pointer items-center gap-2 rounded px-2 py-1 hover:bg-ink-700"
                >
                  <input
                    type="checkbox"
                    checked={selected.has(m.id)}
                    onChange={() => toggle(m.id)}
                  />
                  <span className="flex-1 text-xs text-ink-100">{m.name}</span>
                  <span className="font-mono text-2xs text-ink-400">
                    {m.alias}
                  </span>
                </label>
              ))}
            </div>
          )}
        </div>

        {save.isError && <ErrorBox error={save.error} />}
      </div>
    </Modal>
  );
}
