import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Info } from "lucide-react";
import { Dropdown } from "@/components/Dropdown";
import { useToast } from "@/components/toastContext";
import { useRetryUnlessForbidden } from "@/lib/access";
import { api, ApiError } from "@/lib/api";
import {
  correctionFor,
  correctionMoved,
  correctionText,
  UNGROUPED_ID,
  type CorrectionKind,
  type GroupCorrection,
  type TopologyNode,
} from "@/lib/awsEstate";

// The side panel of a group: rename it, merge it into another, or move a
// resource into it. Every edit is a stored correction that survives
// re-reads. An edit starts from the correction the user saw; if someone
// else changed it since, the edit is refused and the panel asks for a
// reload rather than overwrite their change.

const LADDER =
  "Groups come from the first rule that fits: your corrections, an app tag, a CloudFormation stack or AppRegistry application, a cluster and namespace or Auto Scaling group, a shared name prefix, then the VPC. The rest is Ungrouped.";

type Edit = { kind: CorrectionKind; key: { group_id?: string; member_arn?: string }; before: GroupCorrection | undefined };

/** The server refused the write because the correction it started from changed or is gone. */
function isConflict(e: unknown): boolean {
  if (!(e instanceof ApiError) || e.status !== 409) return false;
  const code = e.body && typeof e.body === "object" && "code" in e.body ? (e.body as { code: unknown }).code : undefined;
  return code === "correction_conflict";
}

export function GroupPanel({
  account,
  region,
  group,
  groups,
  canWrite,
}: {
  account: string;
  /** The Region on screen; the member lists read the same scope as the map. */
  region?: string;
  /** The group card; `group` carries the raw group ID the corrections use. */
  group: TopologyNode;
  groups: TopologyNode[];
  canWrite: boolean;
}) {
  const qc = useQueryClient();
  const toast = useToast();
  const retry = useRetryUnlessForbidden();
  const corrections = useQuery({
    queryKey: ["aws-group-corrections", account],
    queryFn: () => api.awsGroupCorrections(account),
    retry,
  });
  const list = corrections.data?.corrections ?? [];
  const [edit, setEdit] = useState<Edit | null>(null);
  const [name, setName] = useState(group.name);
  const [mergeInto, setMergeInto] = useState("");
  const [member, setMember] = useState("");
  const [outMember, setOutMember] = useState("");
  const [moveTarget, setMoveTarget] = useState("");
  const [conflict, setConflict] = useState(false);
  const [failure, setFailure] = useState<string | null>(null);
  const groupId = group.group ?? "";
  const ungrouped = groupId === UNGROUPED_ID;
  // Resources move between a group and Ungrouped. Both member lists are
  // read from the stores, like the map: Ungrouped's, and this group's own.
  const pool = useQuery({
    queryKey: ["aws-topology", { level: "groups", account, region, group: UNGROUPED_ID }],
    queryFn: () => api.awsTopology({ level: "groups", account, region, group: UNGROUPED_ID }),
    retry,
  });
  const own = useQuery({
    queryKey: ["aws-topology", { level: "groups", account, region, group: groupId }],
    queryFn: () => api.awsTopology({ level: "groups", account, region, group: groupId }),
    enabled: !ungrouped && groupId !== "",
    retry,
  });

  const start = (kind: CorrectionKind, key: Edit["key"]) => {
    setConflict(false);
    setFailure(null);
    setEdit({ kind, key, before: correctionFor(list, { kind, ...key }) });
  };

  const save = useMutation({
    mutationFn: async (body: { kind: CorrectionKind; key: Edit["key"]; target_group_id?: string; name?: string; before: GroupCorrection | undefined }) => {
      // Compare and swap: read the stored correction again, and refuse when
      // it is not the one this edit started from.
      const fresh = await api.awsGroupCorrections(account);
      const now = correctionFor(fresh.corrections ?? [], { kind: body.kind, ...body.key });
      if (correctionMoved(body.before, now)) throw new ApiError(409, "changed by someone else; reload", { code: "correction_conflict" });
      return api.saveAwsGroupCorrection({
        account_id: account,
        kind: body.kind,
        ...body.key,
        target_group_id: body.target_group_id,
        name: body.name,
        // The server checks it when set; with none read, the check above stands alone.
        ...(body.before ? { expected_created_at: body.before.created_at } : {}),
      });
    },
    onSuccess: () => {
      toast.push({ tone: "ok", title: "Saved. It applies on every read" });
      setEdit(null);
      setMergeInto("");
      setMember("");
      setOutMember("");
      setMoveTarget("");
      void qc.invalidateQueries({ queryKey: ["aws-group-corrections", account] });
      void qc.invalidateQueries({ queryKey: ["aws-topology"] });
    },
    onError: (e) => {
      if (isConflict(e)) setConflict(true);
      else setFailure(e instanceof ApiError ? e.message : "Couldn't save the change");
    },
  });

  const remove = useMutation({
    mutationFn: (c: GroupCorrection) =>
      api.deleteAwsGroupCorrection({ account_id: c.account_id, kind: c.kind, group_id: c.group_id, member_arn: c.member_arn }),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ["aws-group-corrections", account] });
      void qc.invalidateQueries({ queryKey: ["aws-topology"] });
    },
    onError: (e) => setFailure(e instanceof ApiError ? e.message : "Couldn't remove the correction"),
  });

  const reload = () => {
    setConflict(false);
    setEdit(null);
    void qc.invalidateQueries({ queryKey: ["aws-group-corrections", account] });
    void qc.invalidateQueries({ queryKey: ["aws-topology"] });
  };

  const mergeOptions = groups
    .filter((g) => g.group && g.group !== groupId && g.group !== UNGROUPED_ID)
    .map((g) => ({ value: g.group as string, label: g.name }));
  const membersOf = (nodes: TopologyNode[] | undefined, id: string) =>
    (nodes ?? []).filter((n) => n.kind === "resource" && n.group === id).map((n) => ({ value: n.id, label: n.name }));
  const movable = membersOf(pool.data?.nodes, UNGROUPED_ID);
  const ownMembers = membersOf(own.data?.nodes, groupId);
  const targets = groups.filter((g) => g.group && g.group !== UNGROUPED_ID).map((g) => ({ value: g.group as string, label: g.name }));
  const stale = list.filter((c) => c.status === "no_longer_matches");
  const busy = save.isPending || !corrections.isSuccess;
  const denied = canWrite ? undefined : "Your role can't change groups";

  return (
    <aside className="card min-w-0 self-start" aria-label={`Group ${group.name}`} data-group-panel={groupId}>
      <h2 className="truncate border-b border-ink-500/40 px-4 py-3 text-sm font-semibold text-ink-50" title={group.name}>
        {ungrouped ? "Ungrouped" : group.name}
      </h2>
      <div className="space-y-4 px-4 py-4">
        {conflict && (
          <div role="alert" className="flex items-center justify-between gap-3 rounded border border-sev-warn/50 bg-sev-warn/10 px-3 py-2 text-xs text-ink-100" data-conflict>
            Changed by someone else.
            <button type="button" className="font-semibold text-link hover:underline" onClick={reload}>
              Reload
            </button>
          </div>
        )}
        {failure && (
          <p role="alert" className="text-xs text-sev-critical" data-failure>
            {failure}
          </p>
        )}
        {!ungrouped &&
          (edit?.kind === "rename" ? (
            <form
              className="flex items-center gap-2"
              onSubmit={(e) => {
                e.preventDefault();
                if (name.trim()) save.mutate({ kind: "rename", key: edit.key, name: name.trim(), before: edit.before });
              }}
            >
              <input className="input h-8 min-w-0 flex-1 text-xs" aria-label="Group name" maxLength={128} value={name} onChange={(e) => setName(e.target.value)} autoFocus />
              <button type="submit" className="btn btn-primary-strong" disabled={busy || !name.trim()}>
                Save
              </button>
              <button type="button" className="btn" onClick={() => setEdit(null)}>
                Cancel
              </button>
            </form>
          ) : (
            <div className="flex flex-wrap items-center gap-2">
              <button
                type="button"
                className="btn"
                disabled={!canWrite || busy}
                title={denied}
                onClick={() => {
                  setName(group.name);
                  start("rename", { group_id: groupId });
                }}
              >
                Rename
              </button>
              <Dropdown
                value={mergeInto}
                options={[{ value: "", label: "Merge with" }, ...mergeOptions]}
                onChange={(v) => {
                  setMergeInto(v);
                  if (v) start("merge", { group_id: groupId });
                }}
                disabled={!canWrite || busy || mergeOptions.length === 0}
                aria-label="Merge with"
                className="w-40"
              />
              {mergeInto && edit?.kind === "merge" && (
                <button
                  type="button"
                  className="btn btn-primary-strong"
                  disabled={busy}
                  onClick={() => save.mutate({ kind: "merge", key: edit.key, target_group_id: mergeInto, before: edit.before })}
                >
                  Merge
                </button>
              )}
            </div>
          ))}
        {ungrouped ? (
          <div data-move-out-of-ungrouped>
            <p className="mb-1.5 text-xs text-ink-300">Move a resource to a group</p>
            <div className="flex flex-wrap items-center gap-2">
              <Dropdown
                value={member}
                options={[{ value: "", label: movable.length > 0 ? "Choose a resource" : "No resource to move" }, ...movable]}
                onChange={(v) => {
                  setMember(v);
                  if (v) start("move", { member_arn: v });
                }}
                disabled={!canWrite || busy || movable.length === 0}
                aria-label="Resource to move"
                wrapOptions
                className="min-w-0 flex-1"
              />
              <Dropdown
                value={moveTarget}
                options={[{ value: "", label: "Group" }, ...targets]}
                onChange={setMoveTarget}
                disabled={!canWrite || busy || targets.length === 0}
                aria-label="Group to move to"
                className="w-36"
              />
              <button
                type="button"
                className="btn btn-primary-strong"
                disabled={!canWrite || busy || !member || !moveTarget || edit?.kind !== "move"}
                title={denied}
                onClick={() => edit && save.mutate({ kind: "move", key: edit.key, target_group_id: moveTarget, before: edit.before })}
              >
                Move
              </button>
            </div>
          </div>
        ) : (
          <>
            <div>
              <p className="mb-1.5 text-xs text-ink-300">Move a resource here</p>
              <div className="flex items-center gap-2">
                <Dropdown
                  value={member}
                  options={[{ value: "", label: movable.length > 0 ? "Choose a resource" : "No resource to move" }, ...movable]}
                  onChange={(v) => {
                    setMember(v);
                    setOutMember("");
                    if (v) start("move", { member_arn: v });
                  }}
                  disabled={!canWrite || busy || movable.length === 0}
                  aria-label="Resource to move"
                  wrapOptions
                  className="min-w-0 flex-1"
                />
                <button
                  type="button"
                  className="btn btn-primary-strong"
                  disabled={!canWrite || busy || !member || edit?.kind !== "move"}
                  title={denied}
                  onClick={() => edit && save.mutate({ kind: "move", key: edit.key, target_group_id: groupId, before: edit.before })}
                >
                  Move
                </button>
              </div>
            </div>
            <div data-move-to-ungrouped>
              <p className="mb-1.5 text-xs text-ink-300">Move a resource to Ungrouped</p>
              <div className="flex items-center gap-2">
                <Dropdown
                  value={outMember}
                  options={[{ value: "", label: ownMembers.length > 0 ? "Choose a resource" : "No resource to move" }, ...ownMembers]}
                  onChange={(v) => {
                    setOutMember(v);
                    setMember("");
                    if (v) start("move", { member_arn: v });
                  }}
                  disabled={!canWrite || busy || ownMembers.length === 0}
                  aria-label="Resource to move to Ungrouped"
                  wrapOptions
                  className="min-w-0 flex-1"
                />
                <button
                  type="button"
                  className="btn"
                  disabled={!canWrite || busy || !outMember || edit?.kind !== "move"}
                  title={denied}
                  onClick={() => edit && save.mutate({ kind: "move", key: edit.key, target_group_id: UNGROUPED_ID, before: edit.before })}
                >
                  Move to Ungrouped
                </button>
              </div>
            </div>
          </>
        )}
        {stale.length > 0 && (
          <ul className="space-y-1.5 border-t border-ink-500/40 pt-3" aria-label="Corrections that no longer match" data-no-longer-matches>
            {stale.map((c) => (
              <li key={`${c.kind}:${c.group_id ?? c.member_arn}`} className="flex min-w-0 items-center gap-2 text-xs">
                <span className="min-w-0 flex-1 truncate text-ink-200" title={correctionText(c)}>
                  {correctionText(c)}
                </span>
                <span className="shrink-0 rounded border border-dashed border-ink-500 px-1.5 text-2xs text-ink-300">No longer matches</span>
                {!c.inherited && (
                  <button type="button" className="shrink-0 text-link hover:underline disabled:opacity-50" disabled={!canWrite || remove.isPending} onClick={() => remove.mutate(c)}>
                    Remove
                  </button>
                )}
              </li>
            ))}
          </ul>
        )}
        <p className="flex items-center gap-1.5 border-t border-ink-500/40 pt-3 text-xs">
          <span className="text-link" title={LADDER}>
            How groups are made
          </span>
          <Info size={12} aria-label={LADDER} className="text-link" />
        </p>
      </div>
    </aside>
  );
}
