import { useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { X } from "lucide-react";
import { api } from "@/lib/api";
import { ErrorBox } from "@/components/feedback";

// AssignDialog is a modal used by the incidents list and detail pages to
// set/clear the team and members assigned to an incident. On save it
// invalidates both the incidents list and the specific incident detail
// query so any open view refreshes immediately.
export function AssignDialog({
  incidentID,
  initialTeamID,
  initialMemberIDs,
  onClose,
}: {
  incidentID: string;
  initialTeamID?: string;
  initialMemberIDs?: string[];
  onClose: () => void;
}) {
  const qc = useQueryClient();
  const teamsQ = useQuery({ queryKey: ["teams"], queryFn: api.listTeams });
  const membersQ = useQuery({
    queryKey: ["members"],
    queryFn: api.listMembers,
  });

  const [team, setTeam] = useState(initialTeamID ?? "");
  const [memberIDs, setMemberIDs] = useState<string[]>(initialMemberIDs ?? []);

  const memberById = useMemo(() => {
    const m = new Map<string, string>();
    for (const x of membersQ.data ?? []) m.set(x.id, x.name);
    return m;
  }, [membersQ.data]);

  const save = useMutation({
    mutationFn: () =>
      api.assignIncident(incidentID, {
        team_id: team || null,
        member_ids: memberIDs,
      }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["incidents"] });
      qc.invalidateQueries({ queryKey: ["incident", incidentID] });
      onClose();
    },
  });

  const toggleMember = (id: string) => {
    setMemberIDs((cur) =>
      cur.includes(id) ? cur.filter((x) => x !== id) : [...cur, id],
    );
  };

  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center bg-ink-900/40 p-4"
      onClick={onClose}
    >
      <div
        className="w-full max-w-lg rounded-lg bg-white shadow-xl"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="flex items-center justify-between border-b border-ink-100 px-4 py-3">
          <h2 className="text-sm font-semibold text-ink-900">
            Assign incident
          </h2>
          <button
            className="rounded p-1 text-ink-500 hover:bg-ink-50"
            onClick={onClose}
          >
            <X size={14} />
          </button>
        </div>
        <div className="space-y-3 p-4">
          <div className="text-2xs text-ink-400">
            Incident{" "}
            <span className="font-mono">{incidentID.slice(0, 8)}</span>
          </div>
          <div>
            <label className="field-label">Team</label>
            <select
              className="input"
              value={team}
              onChange={(e) => setTeam(e.target.value)}
            >
              <option value="">— None —</option>
              {(teamsQ.data ?? []).map((t) => (
                <option key={t.id} value={t.id}>
                  {t.name}
                </option>
              ))}
            </select>
          </div>
          <div>
            <div className="mb-1 flex items-center justify-between">
              <label className="field-label mb-0">Members</label>
              <span className="text-2xs text-ink-400">
                {memberIDs.length} selected
              </span>
            </div>
            {(membersQ.data ?? []).length === 0 ? (
              <p className="text-2xs text-ink-400">
                No members yet — add some from the Members page.
              </p>
            ) : (
              <div className="max-h-56 space-y-1 overflow-auto rounded-md border border-ink-100 bg-ink-50/40 p-2">
                {(membersQ.data ?? []).map((m) => (
                  <label
                    key={m.id}
                    className="flex cursor-pointer items-center gap-2 rounded px-2 py-1 hover:bg-white"
                  >
                    <input
                      type="checkbox"
                      checked={memberIDs.includes(m.id)}
                      onChange={() => toggleMember(m.id)}
                    />
                    <span className="flex-1 text-xs text-ink-800">
                      {m.name}
                    </span>
                    <span className="font-mono text-2xs text-ink-400">
                      {m.alias}
                    </span>
                  </label>
                ))}
              </div>
            )}
          </div>
          {/* Surface stale-reference noise when the original record points
              at a member that has since been deleted from the roster. */}
          {(initialMemberIDs ?? []).some((id) => !memberById.has(id)) && (
            <p className="text-2xs text-warn">
              Some previously assigned members no longer exist in the roster.
            </p>
          )}
          {save.isError && <ErrorBox error={save.error} />}
        </div>
        <div className="flex justify-end gap-2 border-t border-ink-100 px-4 py-3">
          <button
            className="btn"
            onClick={onClose}
            disabled={save.isPending}
          >
            Cancel
          </button>
          <button
            className="btn btn-primary"
            onClick={() => save.mutate()}
            disabled={save.isPending}
          >
            {save.isPending ? "Saving…" : "Save"}
          </button>
        </div>
      </div>
    </div>
  );
}
