import { useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "@/lib/api";
import { ErrorBox } from "@/components/feedback";
import { Modal } from "./Modal";
import { useToast } from "./Toast";

// AssignDialog — set/clear the team and members on an incident. Rebased on
// the accessible Modal; success now confirms via toast (audit S3 class:
// silent outcomes).
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
  const toast = useToast();
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
      toast.push({ title: "Assignment saved", tone: "ok" });
      onClose();
    },
  });

  const toggleMember = (id: string) => {
    setMemberIDs((cur) =>
      cur.includes(id) ? cur.filter((x) => x !== id) : [...cur, id],
    );
  };

  return (
    <Modal
      title="Assign incident"
      onClose={onClose}
      closeDisabled={save.isPending}
      size="lg"
      footer={
        <>
          <button className="btn" onClick={onClose} disabled={save.isPending}>
            Cancel
          </button>
          <button
            className="btn btn-primary"
            onClick={() => save.mutate()}
            disabled={save.isPending}
          >
            {save.isPending ? "Saving…" : "Save"}
          </button>
        </>
      }
    >
      <div className="space-y-3">
        <div className="text-2xs text-ink-400">
          Incident <span className="font-mono">{incidentID.slice(0, 8)}</span>
        </div>
        <div>
          <label className="field-label" htmlFor="assign-team">
            Team
          </label>
          <select
            id="assign-team"
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
            <span className="field-label mb-0">Members</span>
            <span className="text-2xs text-ink-400">
              {memberIDs.length} selected
            </span>
          </div>
          {membersQ.isLoading ? (
            <div aria-hidden className="space-y-1 rounded-control border border-ink-600 p-2">
              <div className="sk h-6" />
              <div className="sk h-6" />
            </div>
          ) : (membersQ.data ?? []).length === 0 ? (
            <p className="text-2xs text-ink-400">
              No members yet — add some from the People page.
            </p>
          ) : (
            <div className="max-h-56 space-y-1 overflow-auto rounded-control border border-ink-600 bg-surface-sunken/60 p-2">
              {(membersQ.data ?? []).map((m) => (
                <label
                  key={m.id}
                  className="flex min-h-8 cursor-pointer items-center gap-2 rounded px-2 py-1 hover:bg-ink-700"
                >
                  <input
                    type="checkbox"
                    checked={memberIDs.includes(m.id)}
                    onChange={() => toggleMember(m.id)}
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
        {(initialMemberIDs ?? []).some((id) => !memberById.has(id)) && (
          <p className="text-2xs text-sev-warn">
            Some previously assigned members no longer exist in the roster.
          </p>
        )}
        {save.isError && <ErrorBox error={save.error} />}
      </div>
    </Modal>
  );
}
