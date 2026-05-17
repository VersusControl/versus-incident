import { useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Pencil, Plus, Search, Trash2 } from "lucide-react";
import {
  api,
  type Member,
  type Team,
  type TeamInput,
} from "@/lib/api";
import { TopBar } from "@/components/TopBar";
import { EmptyState, ErrorBox, Spinner } from "@/components/feedback";
import { Pill } from "@/components/Pill";
import { Modal } from "./MembersPage";

// TeamsPage lets operators group members into named teams. Teams hold
// an ordered MemberIDs list — the order is preserved by the backend and
// reflected in the picker below. Teams are surfaced on the incident
// detail page so an incident can be assigned to a team plus an explicit
// subset of members.
export function TeamsPage() {
  const qc = useQueryClient();
  const teamsQ = useQuery({ queryKey: ["teams"], queryFn: api.listTeams });
  const membersQ = useQuery({
    queryKey: ["members"],
    queryFn: api.listMembers,
  });

  const [q, setQ] = useState("");
  const [editing, setEditing] = useState<Team | "new" | null>(null);

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

  const del = useMutation({
    mutationFn: (id: string) => api.deleteTeam(id),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["teams"] }),
  });

  return (
    <>
      <TopBar
        title="Teams"
        subtitle={teamsQ.data ? `${teamsQ.data.length} configured` : undefined}
        actions={
          <button className="btn" onClick={() => setEditing("new")}>
            <Plus size={12} /> Add team
          </button>
        }
      />

      <main className="flex-1 overflow-auto p-6">
        <div className="mb-3 flex flex-wrap items-center gap-2">
          <div className="relative max-w-md flex-1">
            <Search
              size={12}
              className="absolute left-2.5 top-1/2 -translate-y-1/2 text-ink-300"
            />
            <input
              className="input pl-7"
              placeholder="Search by name, alias, or description…"
              value={q}
              onChange={(e) => setQ(e.target.value)}
            />
          </div>
        </div>

        {teamsQ.isError && <ErrorBox error={teamsQ.error} />}

        <div className="card overflow-hidden">
          <div className="max-h-[calc(100vh-220px)] overflow-auto">
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
                {teamsQ.isLoading && (
                  <tr>
                    <td colSpan={4} className="py-8 text-center">
                      <Spinner />
                    </td>
                  </tr>
                )}
                {!teamsQ.isLoading && filtered.length === 0 && (
                  <tr>
                    <td colSpan={4}>
                      <EmptyState
                        title="No teams yet"
                        hint="Group members into teams so you can assign incidents to a whole team."
                      />
                    </td>
                  </tr>
                )}
                {filtered.map((t) => (
                  <tr key={t.id}>
                    <td>
                      <div className="font-medium text-ink-900">{t.name}</div>
                      {t.description && (
                        <div className="text-2xs text-ink-500">
                          {t.description}
                        </div>
                      )}
                    </td>
                    <td className="font-mono text-2xs text-ink-600">
                      {t.alias}
                    </td>
                    <td>
                      {t.member_ids.length === 0 ? (
                        <span className="text-ink-300">—</span>
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
                        <button
                          className="btn"
                          title="Edit"
                          onClick={() => setEditing(t)}
                        >
                          <Pencil size={11} />
                        </button>
                        <button
                          className="btn"
                          title="Delete"
                          disabled={del.isPending}
                          onClick={() => {
                            if (confirm(`Delete team "${t.name}"?`)) {
                              del.mutate(t.id);
                            }
                          }}
                        >
                          <Trash2 size={11} />
                        </button>
                      </div>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </div>
      </main>

      {editing && (
        <TeamEditor
          team={editing === "new" ? null : editing}
          members={membersQ.data ?? []}
          onClose={() => setEditing(null)}
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
  const isNew = team === null;
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
      onClose();
    },
  });

  return (
    <Modal title={isNew ? "Add team" : `Edit ${team!.name}`} onClose={onClose}>
      <div className="space-y-3">
        <div>
          <label className="field-label">Name</label>
          <input
            className="input"
            placeholder="e.g. Platform Team"
            value={name}
            autoFocus
            onChange={(e) => setName(e.target.value)}
          />
        </div>
        <div>
          <div className="flex items-center justify-between">
            <label className="field-label">Alias</label>
            {!aliasTouched && (
              <span className="text-2xs text-ink-400">auto from name</span>
            )}
          </div>
          <input
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
          <label className="field-label">Description (optional)</label>
          <input
            className="input"
            placeholder="Owns ingestion, agent, and on-call routing."
            value={description}
            onChange={(e) => setDescription(e.target.value)}
          />
        </div>

        <div className="border-t border-ink-100 pt-3">
          <div className="mb-2 flex items-center justify-between">
            <div className="text-2xs uppercase tracking-wider text-ink-500">
              Members
            </div>
            <div className="text-2xs text-ink-400">
              {memberIDs.length} selected · order preserved
            </div>
          </div>
          {members.length === 0 ? (
            <p className="text-2xs text-ink-400">
              No members yet. Add a few from the Members page first.
            </p>
          ) : (
            <div className="max-h-64 space-y-1 overflow-auto rounded-md border border-ink-100 bg-ink-50/40 p-2">
              {members.map((m) => (
                <label
                  key={m.id}
                  className="flex cursor-pointer items-center gap-2 rounded px-2 py-1 hover:bg-white"
                >
                  <input
                    type="checkbox"
                    checked={selected.has(m.id)}
                    onChange={() => toggle(m.id)}
                  />
                  <span className="flex-1 text-xs text-ink-800">{m.name}</span>
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

      <div className="mt-4 flex justify-end gap-2">
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
      </div>
    </Modal>
  );
}
