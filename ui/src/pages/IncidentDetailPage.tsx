import { Link, useParams } from "react-router-dom";
import { useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ArrowLeft, Brain, CheckCircle2, FileText, Sparkles, UserPlus } from "lucide-react";
import { api } from "@/lib/api";
import { fmtAbs, fmtRel } from "@/lib/format";
import { TopBar } from "@/components/TopBar";
import { Pill } from "@/components/Pill";
import { ErrorBox, Spinner } from "@/components/feedback";
import { ConfirmDialog } from "@/components/ConfirmDialog";

// Stable keys written into Incident.content by the backend. Agent-emitted
// incidents (services.CreateIncidentFromFinding) set most of these; manual
// API callers may also include any subset.
type Content = Record<string, unknown>;

function pickString(c: Content, ...keys: string[]): string {
  for (const k of keys) {
    const v = c[k];
    if (typeof v === "string" && v.length > 0) return v;
  }
  return "";
}

function pickNumber(c: Content, ...keys: string[]): number | undefined {
  for (const k of keys) {
    const v = c[k];
    if (typeof v === "number" && Number.isFinite(v)) return v;
  }
  return undefined;
}

function pickList(c: Content, ...keys: string[]): string[] {
  for (const k of keys) {
    const v = c[k];
    if (Array.isArray(v)) {
      return v.filter((x): x is string => typeof x === "string" && x.length > 0);
    }
  }
  return [];
}

function severityTone(sev: string): "good" | "warn" | "bad" | "accent" {
  switch (sev.toLowerCase()) {
    case "critical":
    case "high":
      return "bad";
    case "medium":
    case "moderate":
      return "warn";
    case "low":
    case "info":
      return "good";
    default:
      return "accent";
  }
}

// IncidentDetailPage shows the full persisted record including the raw
// content payload that drove the alert templates plus a structured
// breakdown of the most useful fields (alert summary, AI analysis if
// present, sample logs, and the raw payload).
export function IncidentDetailPage() {
  const { id = "" } = useParams<{ id: string }>();
  const { data, isLoading, isError, error } = useQuery({
    queryKey: ["incident", id],
    queryFn: () => api.getIncident(id),
    enabled: !!id,
  });

  const content: Content = (data?.content ?? {}) as Content;

  const alertName = pickString(content, "AlertName", "alertname", "alert_name");
  const summary = pickString(content, "Summary", "summary", "description");
  const severity = pickString(content, "Severity", "severity");
  const category = pickString(content, "Category", "category");
  const confidence = pickNumber(content, "Confidence", "confidence");
  const suggestions = pickList(content, "Suggestions", "suggestions");
  const logs = pickString(content, "Logs", "logs", "message");
  const status = pickString(content, "Status", "status");

  const patternID = pickString(content, "PatternID", "pattern_id");
  const patternTemplate = pickString(content, "PatternTemplate", "pattern_template");
  const frequency = pickNumber(content, "Frequency", "frequency");
  const baseline = pickNumber(content, "Baseline", "baseline");
  const verdict = pickString(content, "Verdict", "verdict");
  const serviceName = pickString(content, "ServiceName", "Service", "service");

  const isAgent =
    !!patternID || (data?.source ?? "").startsWith("agent:");

  return (
    <>
      <TopBar
        title="Incident"
        subtitle={data?.title || data?.id?.slice(0, 8)}
        actions={
          <Link to="/incidents" className="btn">
            <ArrowLeft size={12} />
            Back
          </Link>
        }
      />

      <main className="flex-1 overflow-auto p-6">
        {isLoading && <Spinner />}
        {isError && <ErrorBox error={error} />}

        {data && (
          <>
            {/* AI action panel — reserved for upcoming features. Each
                action explains what it will do once shipped. Sits above
                the two-column grid so it spans the full width. */}
            <div className="mb-4 grid gap-3 sm:grid-cols-2">
              <AiActionCard
                icon={<Brain size={14} />}
                label="Analysis"
                description="Run a deep AI investigation on this incident: correlate logs, recent deploys, and similar past patterns to surface a likely root cause."
              />
              <AiActionCard
                icon={<FileText size={14} />}
                label="Auto Post Mortem"
                description="Generate a draft post-mortem document (timeline, impact, root cause, action items) you can edit and share with the team."
              />
            </div>

            <div className="grid items-start gap-4 lg:grid-cols-[2fr,1fr]">
              <div className="min-w-0 space-y-4">
                {/* Summary card — always shown, falls back to title/id */}
              <div className="card">
                <div className="card-header">
                  <span className="card-title">
                    {alertName || data.title || "Alert"}
                  </span>
                  <div className="flex items-center gap-1.5">
                    {severity && (
                      <Pill tone={severityTone(severity)}>{severity}</Pill>
                    )}
                    {category && <Pill tone="accent">{category}</Pill>}
                    {status && <Pill>{status}</Pill>}
                  </div>
                </div>
                <div className="card-body space-y-3 text-xs text-ink-800">
                  {summary ? (
                    <p className="whitespace-pre-wrap leading-relaxed">
                      {summary}
                    </p>
                  ) : (
                    <p className="text-ink-400">No summary provided.</p>
                  )}
                  {(confidence !== undefined || serviceName) && (
                    <div className="flex flex-wrap items-center gap-x-4 gap-y-1 border-t border-ink-100 pt-3 text-2xs text-ink-500">
                      {serviceName && (
                        <span>
                          Service{" "}
                          <span className="font-mono text-ink-800">
                            {serviceName}
                          </span>
                        </span>
                      )}
                      {confidence !== undefined && (
                        <span>
                          Confidence{" "}
                          <span className="font-mono text-ink-800">
                            {(confidence * (confidence <= 1 ? 100 : 1)).toFixed(0)}
                            %
                          </span>
                        </span>
                      )}
                    </div>
                  )}
                </div>
              </div>

              {/* Suggestions — always rendered for layout consistency.
                  Agent incidents populate this with AI remediation hints;
                  manual incidents show an empty state. */}
              <div className="card">
                <div className="card-header">
                  <span className="card-title flex items-center gap-1.5">
                    <Sparkles size={12} className="text-accent" />
                    Suggested next steps
                  </span>
                </div>
                <div className="card-body">
                  {suggestions.length > 0 ? (
                    <ol className="list-decimal space-y-1.5 pl-5 text-xs leading-relaxed text-ink-800">
                      {suggestions.map((s, i) => (
                        <li key={i}>{s}</li>
                      ))}
                    </ol>
                  ) : (
                    <p className="text-xs text-ink-400">
                      No suggestions for this incident.
                    </p>
                  )}
                </div>
              </div>

              {/* Sample log — first matching signal for agent incidents;
                  free-form message field for everything else. */}
              <div className="card">
                <div className="card-header">
                  <span className="card-title">Sample log</span>
                </div>
                <div className="card-body">
                  {logs ? (
                    <pre className="max-h-48 overflow-auto whitespace-pre-wrap break-words rounded-md bg-ink-50 p-3 font-mono text-2xs leading-snug text-ink-800">
                      {logs}
                    </pre>
                  ) : (
                    <p className="text-xs text-ink-400">
                      No log sample in payload.
                    </p>
                  )}
                </div>
              </div>

              {/* Raw payload — always last so the structured cards lead */}
              <div className="card">
                <div className="card-header">
                  <span className="card-title">Raw payload</span>
                </div>
                <div className="card-body">
                  <pre className="max-h-96 overflow-auto whitespace-pre-wrap break-words rounded-md bg-ink-50 p-3 font-mono text-xs leading-snug text-ink-800">
                    {JSON.stringify(content, null, 2)}
                  </pre>
                </div>
              </div>
            </div>

            <div className="space-y-4">
              <div className="card">
                <div className="card-header">
                  <span className="card-title">Facts</span>
                </div>
                <div className="card-body grid grid-cols-2 gap-x-4 gap-y-3 text-xs">
                  <Fact k="ID" v={<span className="font-mono">{data.id}</span>} />
                  <Fact k="Service" v={data.service || "—"} />
                  <Fact k="Source" v={data.source || "—"} />
                  <Fact k="Team" v={data.team_id || "—"} />
                  <Fact
                    k="Created"
                    v={
                      <span title={fmtAbs(data.created_at)}>
                        {fmtRel(data.created_at)}
                      </span>
                    }
                  />
                  <Fact
                    k="Acked"
                    v={
                      data.acked_at ? (
                        <span title={fmtAbs(data.acked_at)}>
                          {fmtRel(data.acked_at)}
                        </span>
                      ) : (
                        "—"
                      )
                    }
                  />
                  <Fact
                    k="Resolved"
                    v={data.resolved ? "yes" : "no"}
                  />
                  <Fact
                    k="On-call"
                    v={data.oncall_triggered ? "triggered" : "—"}
                  />
                  <Fact
                    k="Notify"
                    v={
                      data.notify_status === "sent" ? (
                        <Pill tone="good">sent</Pill>
                      ) : data.notify_status === "failed" ? (
                        <span title={data.notify_error}>
                          <Pill tone="bad">failed</Pill>
                        </span>
                      ) : data.notify_status ? (
                        <Pill tone="accent">{data.notify_status}</Pill>
                      ) : (
                        "—"
                      )
                    }
                  />
                  {data.notify_status === "failed" && data.notify_error && (
                    <Fact
                      k="Notify error"
                      v={
                        <span className="break-all font-mono text-2xs text-rose-700">
                          {data.notify_error}
                        </span>
                      }
                    />
                  )}
                </div>
              </div>

              <div className="card">
                <div className="card-header">
                  <span className="card-title">Channels notified</span>
                </div>
                <div className="card-body flex flex-wrap gap-1.5">
                  {(data.channels_notified ?? []).length === 0 && (
                    <span className="text-xs text-ink-400">
                      None enabled at the time.
                    </span>
                  )}
                  {(data.channels_notified ?? []).map((c) => (
                    <Pill key={c} tone="accent">
                      {c}
                    </Pill>
                  ))}
                </div>
              </div>

              <AssignmentCard
                incidentID={data.id}
                teamID={data.assigned_team_id}
                memberIDs={data.assigned_member_ids}
              />

              <div className="card">
                <div className="card-header">
                  <span className="card-title">Status</span>
                  {!data.resolved && <ResolveButton incidentID={data.id} />}
                </div>
                <div className="card-body text-xs">
                  {data.resolved && (
                    <Pill tone="good">resolved</Pill>
                  )}
                  {!data.resolved && data.acked_at && (
                    <Pill tone="accent">acknowledged</Pill>
                  )}
                  {!data.resolved && !data.acked_at && (
                    <Pill tone="bad">open</Pill>
                  )}
                </div>
              </div>

              {/* Agent context — always rendered for layout consistency.
                  Populated for agent-emitted incidents; shows an empty
                  state row for everything else. */}
              <div className="card">
                <div className="card-header">
                  <span className="card-title">Agent context</span>
                  {isAgent && <Pill tone="accent">agent</Pill>}
                </div>
                <div className="card-body grid grid-cols-2 gap-x-4 gap-y-3 text-xs">
                  <Fact
                    k="Pattern"
                    v={
                      patternID ? (
                        <Link
                          to={`/patterns/${patternID}`}
                          className="font-mono text-accent hover:underline"
                        >
                          {patternID.slice(0, 12)}
                        </Link>
                      ) : (
                        "—"
                      )
                    }
                  />
                  <Fact
                    k="Verdict"
                    v={verdict ? <Pill tone="accent">{verdict}</Pill> : "—"}
                  />
                  <Fact
                    k="Frequency"
                    v={
                      frequency !== undefined ? (
                        <span className="font-mono">{frequency}</span>
                      ) : (
                        "—"
                      )
                    }
                  />
                  <Fact
                    k="Baseline"
                    v={
                      baseline !== undefined ? (
                        <span className="font-mono">{baseline}</span>
                      ) : (
                        "—"
                      )
                    }
                  />
                  <div className="col-span-2">
                    <div className="text-2xs uppercase tracking-wider text-ink-400">
                      Template
                    </div>
                    {patternTemplate ? (
                      <pre className="mt-1 max-h-32 overflow-auto whitespace-pre-wrap break-words rounded-md bg-ink-50 p-2 font-mono text-2xs leading-snug text-ink-800">
                        {patternTemplate}
                      </pre>
                    ) : (
                      <p className="mt-1 text-ink-400">—</p>
                    )}
                  </div>
                </div>
              </div>
            </div>
          </div>
          </>
        )}
      </main>
    </>
  );
}

function Fact({ k, v }: { k: string; v: React.ReactNode }) {
  return (
    <div>
      <div className="text-2xs uppercase tracking-wider text-ink-400">{k}</div>
      <div className="text-ink-800">{v}</div>
    </div>
  );
}

// AiActionCard is a disabled call-to-action that explains what an
// upcoming AI feature will do once it ships. Renders the action label,
// a one-line description, and a "Coming soon" pill so the user
// understands the feature is acknowledged but not yet implemented.
function AiActionCard({
  icon,
  label,
  description,
}: {
  icon: React.ReactNode;
  label: string;
  description: string;
}) {
  return (
    <div
      className="card cursor-not-allowed opacity-80"
      aria-disabled="true"
      title="This AI feature will be implemented in the future."
    >
      <div className="card-body flex items-start gap-3">
        <span className="mt-0.5 inline-flex h-7 w-7 flex-none items-center justify-center rounded-md bg-accent/10 text-accent">
          {icon}
        </span>
        <div className="min-w-0 flex-1 space-y-1">
          <div className="flex items-center gap-1.5">
            <span className="text-sm font-semibold text-ink-900">{label}</span>
            <Pill tone="accent">coming soon</Pill>
          </div>
          <p className="text-2xs leading-relaxed text-ink-600">
            {description}
          </p>
        </div>
      </div>
    </div>
  );
}

// ResolveButton posts to /api/admin/incidents/:id/resolve and refreshes
// both the detail view and the incidents list. Uses ConfirmDialog (not
// window.confirm) so the prompt matches the rest of the admin UI.
function ResolveButton({ incidentID }: { incidentID: string }) {
  const qc = useQueryClient();
  const [open, setOpen] = useState(false);
  const m = useMutation({
    mutationFn: () => api.resolveIncident(incidentID),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["incident", incidentID] });
      qc.invalidateQueries({ queryKey: ["incidents"] });
      setOpen(false);
    },
  });
  return (
    <>
      <button
        className="btn"
        aria-label="Mark incident resolved"
        title="Mark this incident as resolved"
        disabled={m.isPending}
        onClick={() => setOpen(true)}
      >
        <CheckCircle2 size={11} />
        {m.isPending ? "Resolving…" : "Resolve"}
      </button>
      {open && (
        <ConfirmDialog
          title="Resolve incident"
          message={
            <>
              Mark this incident as resolved? This stamps a resolved-at
              timestamp and cannot be undone from the UI today.
            </>
          }
          confirmLabel="Resolve"
          busy={m.isPending}
          error={m.isError ? m.error : undefined}
          onConfirm={() => m.mutate()}
          onClose={() => {
            if (!m.isPending) setOpen(false);
          }}
        />
      )}
    </>
  );
}


// AssignmentCard renders the team + members currently assigned to the
// incident and a small inline editor that posts to
// /api/admin/incidents/:id/assign. Team and member references that no
// longer exist in the roster fall back to their raw id so we don't lie
// to the operator about who is on the hook.
function AssignmentCard({
  incidentID,
  teamID,
  memberIDs,
}: {
  incidentID: string;
  teamID?: string;
  memberIDs?: string[];
}) {
  const qc = useQueryClient();
  const teamsQ = useQuery({ queryKey: ["teams"], queryFn: api.listTeams });
  const membersQ = useQuery({
    queryKey: ["members"],
    queryFn: api.listMembers,
  });

  const [editing, setEditing] = useState(false);
  const [draftTeam, setDraftTeam] = useState(teamID ?? "");
  const [draftMembers, setDraftMembers] = useState<string[]>(memberIDs ?? []);

  const memberById = useMemo(() => {
    const m = new Map<string, string>();
    for (const x of membersQ.data ?? []) m.set(x.id, x.name);
    return m;
  }, [membersQ.data]);

  const teamById = useMemo(() => {
    const m = new Map<string, string>();
    for (const x of teamsQ.data ?? []) m.set(x.id, x.name);
    return m;
  }, [teamsQ.data]);

  const save = useMutation({
    mutationFn: () =>
      api.assignIncident(incidentID, {
        team_id: draftTeam || null,
        member_ids: draftMembers,
      }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["incident", incidentID] });
      setEditing(false);
    },
  });

  const toggleMember = (id: string) => {
    setDraftMembers((cur) =>
      cur.includes(id) ? cur.filter((x) => x !== id) : [...cur, id],
    );
  };

  if (!editing) {
    return (
      <div className="card">
        <div className="card-header">
          <span className="card-title">Assigned</span>
          <button
            className="btn"
            onClick={() => {
              setDraftTeam(teamID ?? "");
              setDraftMembers(memberIDs ?? []);
              setEditing(true);
            }}
          >
            <UserPlus size={11} />
            {teamID || (memberIDs && memberIDs.length > 0)
              ? "Change"
              : "Assign"}
          </button>
        </div>
        <div className="card-body space-y-2 text-xs">
          <div>
            <div className="text-2xs uppercase tracking-wider text-ink-400">
              Team
            </div>
            <div className="mt-1">
              {teamID ? (
                <Pill tone="accent">{teamById.get(teamID) ?? teamID}</Pill>
              ) : (
                <span className="text-ink-300">—</span>
              )}
            </div>
          </div>
          <div>
            <div className="text-2xs uppercase tracking-wider text-ink-400">
              Members
            </div>
            <div className="mt-1 flex flex-wrap gap-1">
              {(memberIDs ?? []).length === 0 && (
                <span className="text-ink-300">—</span>
              )}
              {(memberIDs ?? []).map((id) => (
                <Pill key={id}>{memberById.get(id) ?? id.slice(0, 8)}</Pill>
              ))}
            </div>
          </div>
        </div>
      </div>
    );
  }

  return (
    <div className="card">
      <div className="card-header">
        <span className="card-title">Assign</span>
      </div>
      <div className="card-body space-y-3 text-xs">
        <div>
          <label className="field-label">Team</label>
          <select
            className="input"
            value={draftTeam}
            onChange={(e) => setDraftTeam(e.target.value)}
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
              {draftMembers.length} selected
            </span>
          </div>
          {(membersQ.data ?? []).length === 0 ? (
            <p className="text-2xs text-ink-400">
              No members yet — add some from the Members page.
            </p>
          ) : (
            <div className="max-h-48 space-y-1 overflow-auto rounded-md border border-ink-100 bg-ink-50/40 p-2">
              {(membersQ.data ?? []).map((m) => (
                <label
                  key={m.id}
                  className="flex cursor-pointer items-center gap-2 rounded px-2 py-1 hover:bg-white"
                >
                  <input
                    type="checkbox"
                    checked={draftMembers.includes(m.id)}
                    onChange={() => toggleMember(m.id)}
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
        <div className="flex justify-end gap-2 pt-1">
          <button
            className="btn"
            onClick={() => setEditing(false)}
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
