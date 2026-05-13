import { Link, useParams } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { ArrowLeft, Brain, FileText, Sparkles } from "lucide-react";
import { api } from "@/lib/api";
import { fmtAbs, fmtRel } from "@/lib/format";
import { TopBar } from "@/components/TopBar";
import { Pill } from "@/components/Pill";
import { ErrorBox, Spinner } from "@/components/feedback";

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

              <div className="card">
                <div className="card-header">
                  <span className="card-title">Status</span>
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
