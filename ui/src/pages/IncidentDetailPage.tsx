import { Link, useParams } from "react-router-dom";
import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  Brain,
  CheckCircle2,
  ChevronRight,
  Clock,
  PhoneCall,
  Sparkles,
  UserPlus,
  XCircle,
} from "lucide-react";
import clsx from "clsx";
import { api } from "@/lib/api";
import { fmtAbs, fmtRel, incidentTitle } from "@/lib/format";
import { severityFromContent } from "@/lib/severity";
import { TopBar } from "@/components/TopBar";
import { PageHeader } from "@/components/PageHeader";
import { SeverityBadge } from "@/components/SeverityBadge";
import { Pill, SourceBadge } from "@/components/Pill";
import { ChannelIcon } from "@/components/ChannelIcon";
import { ConfirmDialog } from "@/components/ConfirmDialog";
import { AssignDialog } from "@/components/AssignDialog";
import { AnalysisCard } from "@/components/AnalysisCard";
import { SkCard } from "@/components/Skeleton";
import { RetryableError } from "@/components/RetryableError";
import { useToast } from "@/components/Toast";
import { Spinner } from "@/components/feedback";

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

// IncidentDetailPage — the triage workhorse (UX_REDESIGN §2.3b). Sticky
// PageHeader carries identity (severity, title, status) and the three
// actions (Assign / Run analysis / Resolve). The body is a two-column grid
// on lg; on mobile the right-rail STATE strip leads, ordered with flex
// `order-*` utilities: STATE → SUMMARY → SUGGESTIONS → AI → NOTIFIED →
// TIMELINE → SAMPLE LOG → PAYLOAD.
export function IncidentDetailPage() {
  const { id = "" } = useParams<{ id: string }>();
  // justRan flips true only after the operator runs a fresh analysis in
  // this page session. It is intentionally NOT persisted: on reload we
  // fall back to a link to the analyses list instead of re-rendering the
  // full result inline.
  const [justRan, setJustRan] = useState(false);
  const [assignOpen, setAssignOpen] = useState(false);
  const { data, isLoading, isError, error, refetch, isFetching } = useQuery({
    queryKey: ["incident", id],
    queryFn: () => api.getIncident(id),
    enabled: !!id,
  });

  const content: Content = (data?.content ?? {}) as Content;

  const alertName = pickString(content, "AlertName", "alertname", "alert_name");
  const summary = pickString(content, "Summary", "summary", "description");
  const severity = severityFromContent(content);
  const category = pickString(content, "Category", "category");
  const confidence = pickNumber(content, "Confidence", "confidence");
  const suggestions = pickList(content, "Suggestions", "suggestions");
  const logs = pickString(content, "Logs", "logs", "message");
  const contentStatus = pickString(content, "Status", "status");

  const patternID = pickString(content, "PatternID", "pattern_id");
  const patternTemplate = pickString(content, "PatternTemplate", "pattern_template");
  const frequency = pickNumber(content, "Frequency", "frequency");
  const baseline = pickNumber(content, "Baseline", "baseline");
  const verdict = pickString(content, "Verdict", "verdict");
  const serviceName = pickString(content, "ServiceName", "Service", "service");

  const isAgent = !!patternID || (data?.source ?? "").startsWith("agent:");

  const channels = data?.channels_notified ?? [];

  const statusPill = data ? (
    data.resolved ? (
      <Pill tone="good">resolved</Pill>
    ) : data.acked_at ? (
      <Pill tone="accent">acknowledged</Pill>
    ) : (
      <Pill tone="bad">open</Pill>
    )
  ) : null;

  return (
    <>
      {/* Same "#short-id" fallback as the list rows — the entity must not
          change its displayed identity mid-navigation. */}
      <TopBar
        title="Incident"
        subtitle={data ? incidentTitle(data) : incidentTitle({ id })}
      />

      {isLoading && (
        // Full-page skeleton mirroring the real layout — header strip +
        // two card stacks. Never a lone spinner (§2.4).
        <>
          <div
            aria-hidden
            className="shrink-0 border-b border-ink-600 bg-surface-sunken/95 px-4 py-3 lg:px-6"
          >
            <div className="sk h-3 w-20" />
            <div className="sk mt-2 h-5 w-72 max-w-full" />
            <div className="sk mt-2 h-3 w-44" />
          </div>
          <main className="flex-1 overflow-auto p-4 lg:p-6">
            <div className="flex flex-col gap-4 lg:grid lg:grid-cols-[minmax(0,1fr),320px] lg:items-start">
              <div className="flex min-w-0 flex-col gap-4">
                <SkCard lines={4} />
                <SkCard lines={3} />
                <SkCard lines={3} />
              </div>
              <div className="flex flex-col gap-4">
                <SkCard lines={2} />
                <SkCard lines={2} />
                <SkCard lines={2} />
                <SkCard lines={2} />
              </div>
            </div>
          </main>
        </>
      )}

      {isError && (
        <main className="flex-1 overflow-auto p-4 lg:p-6">
          <RetryableError
            context="Couldn't load incident"
            error={error}
            onRetry={() => refetch()}
            retrying={isFetching}
          />
        </main>
      )}

      {data && (
        <>
          <PageHeader
            back={{ to: "/incidents", label: "Incidents" }}
            title={alertName || incidentTitle(data)}
            meta={
              <>
                <SeverityBadge severity={severity} />
                {statusPill}
              </>
            }
            subtitle={
              <span className="flex flex-wrap items-center gap-x-1.5 gap-y-0.5">
                <span>{data.service || "—"}</span>
                <span aria-hidden>·</span>
                <span>via {data.source || "unknown"}</span>
                <span aria-hidden>·</span>
                <span title={fmtAbs(data.created_at)}>
                  {fmtRel(data.created_at)}
                </span>
              </span>
            }
            actions={
              <>
                <button className="btn" onClick={() => setAssignOpen(true)}>
                  <UserPlus size={12} />
                  Assign
                </button>
                <RunAnalysisButton
                  incidentID={id}
                  onRan={() => setJustRan(true)}
                />
                {!data.resolved && <ResolveButton incidentID={data.id} />}
              </>
            }
          />

          <main className="flex-1 overflow-auto p-4 lg:p-6">
            {/* Mobile-first interleave: both column wrappers are
                display:contents below lg so every card participates in the
                outer flex column and is sequenced by its order-N class; at
                lg they become real flex columns inside the 2-col grid. */}
            <div className="flex flex-col gap-4 lg:grid lg:grid-cols-[minmax(0,1fr),320px] lg:items-start">
              {/* ---------- LEFT column (lg) ---------- */}
              <div className="contents lg:flex lg:min-w-0 lg:flex-col lg:gap-4">
                {/* SUMMARY — first thing an operator reads */}
                <div className="card order-2">
                  <div className="card-header gap-2">
                    <span className="card-title">Summary</span>
                    <div className="flex flex-wrap items-center justify-end gap-1.5">
                      {category && <Pill tone="accent">{category}</Pill>}
                      {contentStatus && <Pill>{contentStatus}</Pill>}
                    </div>
                  </div>
                  <div className="card-body space-y-3 text-xs text-ink-100">
                    {summary ? (
                      <p className="whitespace-pre-wrap leading-relaxed">
                        {summary}
                      </p>
                    ) : (
                      <p className="text-ink-400">No summary provided.</p>
                    )}
                    {(confidence !== undefined || serviceName) && (
                      <div className="flex flex-wrap items-center gap-x-4 gap-y-1 border-t border-ink-600 pt-3 text-2xs text-ink-300">
                        {serviceName && (
                          <span>
                            Service{" "}
                            <span className="font-mono text-ink-100">
                              {serviceName}
                            </span>
                          </span>
                        )}
                        {confidence !== undefined && (
                          <span>
                            Confidence{" "}
                            <span className="font-mono text-ink-100">
                              {(
                                confidence * (confidence <= 1 ? 100 : 1)
                              ).toFixed(0)}
                              %
                            </span>
                          </span>
                        )}
                      </div>
                    )}
                  </div>
                </div>

                {/* SUGGESTIONS — agent incidents carry AI remediation hints.
                    Webhook incidents never have them, so an empty card here
                    was permanent noise — render only when there's content. */}
                {suggestions.length > 0 && (
                  <div className="card order-3">
                    <div className="card-header">
                      <span className="card-title flex items-center gap-1.5">
                        <Sparkles size={12} className="text-link" />
                        Suggested next steps
                      </span>
                    </div>
                    <div className="card-body">
                      <ol className="list-decimal space-y-1.5 pl-5 text-xs leading-relaxed text-ink-100">
                        {suggestions.map((s, i) => (
                          <li key={i}>{s}</li>
                        ))}
                      </ol>
                    </div>
                  </div>
                )}

                {/* AI ANALYSIS — existing AnalysisPanel mechanics; loading
                    renders a SkCard so the slot never pops in. */}
                <AnalysisPanel
                  incidentID={id}
                  justRan={justRan}
                  className="order-4"
                />

                {/* TIMELINE */}
                <div className="card order-6">
                  <div className="card-header">
                    <span className="card-title">Timeline</span>
                  </div>
                  {/* Connector lives on a wrapper div, not inside the <ol> —
                      an <ol> only permits <li> children, and a span in the
                      first-child slot also fed space-y an extra margin. */}
                  <div className="card-body relative">
                    {/* Through the icon column: card-body p-4 (16px) + time
                        w-20 (80px) + gap-2.5 (10px) + half a 12px glyph
                        = 112px = left-28. */}
                    <span
                      aria-hidden
                      className="absolute bottom-7 left-28 top-7 w-px -translate-x-1/2 bg-ink-600"
                    />
                  <ol className="space-y-1.5">
                    <TimelineRow
                      at={data.created_at}
                      icon={<Clock size={12} className="text-ink-300" />}
                      label={
                        <>
                          created
                          {data.source ? (
                            <span className="text-ink-300"> ({data.source})</span>
                          ) : null}
                        </>
                      }
                    />
                    {channels.map((c) => (
                      <TimelineRow
                        key={c}
                        icon={
                          data.notify_status === "failed" ? (
                            <XCircle size={12} className="text-sev-critical" />
                          ) : (
                            <CheckCircle2 size={12} className="text-sev-ok" />
                          )
                        }
                        label={`notified ${c}`}
                      />
                    ))}
                    {data.notify_status === "failed" && (
                      <TimelineRow
                        icon={<XCircle size={12} className="text-sev-critical" />}
                        label="notify failed"
                        detail={data.notify_error}
                      />
                    )}
                    <TimelineRow
                      at={data.acked_at ?? undefined}
                      icon={
                        data.acked_at ? (
                          <CheckCircle2 size={12} className="text-sev-info" />
                        ) : (
                          <Clock size={12} className="text-ink-500" />
                        )
                      }
                      label={
                        <span className={data.acked_at ? undefined : "text-ink-300"}>
                          acked
                        </span>
                      }
                    />
                    <TimelineRow
                      at={data.resolved_at ?? undefined}
                      icon={
                        data.resolved ? (
                          <CheckCircle2 size={12} className="text-sev-ok" />
                        ) : (
                          <Clock size={12} className="text-ink-500" />
                        )
                      }
                      label={
                        <span className={data.resolved ? undefined : "text-ink-300"}>
                          resolved
                        </span>
                      }
                    />
                  </ol>
                  </div>
                </div>

                {/* SAMPLE LOG — collapsed, 0px until asked */}
                <details className="card group order-7">
                  <summary className="flex cursor-pointer select-none list-none items-center gap-1.5 px-4 py-3 text-sm font-semibold text-ink-50 [&::-webkit-details-marker]:hidden">
                    <ChevronRight
                      size={14}
                      className="text-ink-400 transition-transform group-open:rotate-90"
                      aria-hidden
                    />
                    Sample log
                  </summary>
                  <div className="border-t border-ink-600 p-4">
                    {logs ? (
                      <pre className="max-h-48 overflow-auto whitespace-pre-wrap break-words rounded-md bg-surface-sunken p-3 font-mono text-2xs leading-snug text-ink-100">
                        {logs}
                      </pre>
                    ) : (
                      <p className="text-xs text-ink-400">
                        No log sample in payload.
                      </p>
                    )}
                  </div>
                </details>

                {/* RAW PAYLOAD — collapsed, always last */}
                <details className="card group order-8">
                  <summary className="flex cursor-pointer select-none list-none items-center gap-1.5 px-4 py-3 text-sm font-semibold text-ink-50 [&::-webkit-details-marker]:hidden">
                    <ChevronRight
                      size={14}
                      className="text-ink-400 transition-transform group-open:rotate-90"
                      aria-hidden
                    />
                    Raw payload
                  </summary>
                  <div className="border-t border-ink-600 p-4">
                    <pre className="max-h-96 overflow-auto whitespace-pre-wrap break-words rounded-md bg-surface-sunken p-3 font-mono text-xs leading-snug text-ink-100">
                      {JSON.stringify(content, null, 2)}
                    </pre>
                  </div>
                </details>
              </div>

              {/* ---------- RIGHT column (lg); STATE leads on mobile ---------- */}
              <div className="contents lg:flex lg:flex-col lg:gap-4">
                {/* STATE — first card on mobile (order-1) */}
                <div className="card order-1">
                  <div className="card-header">
                    {/* Resolve lives in the TopBar (desktop) and the mobile
                        action bar — one primary CTA per screen, not three. */}
                    <span className="card-title">State</span>
                  </div>
                  <div className="card-body space-y-2.5 text-xs">
                    <div className="flex flex-wrap items-center gap-2">
                      {statusPill}
                      {!data.resolved && !data.acked_at && (
                        <span className="text-2xs text-ink-400">not acked</span>
                      )}
                      {!data.resolved && data.acked_at && (
                        <span
                          className="text-2xs text-ink-400"
                          title={fmtAbs(data.acked_at)}
                        >
                          acked {fmtRel(data.acked_at)}
                        </span>
                      )}
                    </div>
                    <dl className="space-y-1.5">
                      <StateRow label="Created">
                        <span title={fmtAbs(data.created_at)}>
                          {fmtRel(data.created_at)}
                        </span>
                      </StateRow>
                      <StateRow label="Notified">
                        {data.notify_status === "sent" ? (
                          <span className="inline-flex items-center gap-1 text-sev-ok">
                            <CheckCircle2 size={11} aria-hidden /> sent
                          </span>
                        ) : data.notify_status === "failed" ? (
                          <span className="inline-flex items-center gap-1 text-sev-critical">
                            <XCircle size={11} aria-hidden /> failed
                          </span>
                        ) : data.notify_status ? (
                          <Pill>{data.notify_status}</Pill>
                        ) : (
                          "—"
                        )}
                      </StateRow>
                      <StateRow label="Acked">
                        {data.acked_at ? (
                          <span title={fmtAbs(data.acked_at)}>
                            {fmtRel(data.acked_at)}
                          </span>
                        ) : (
                          "—"
                        )}
                      </StateRow>
                      <StateRow label="Resolved">
                        {data.resolved_at ? (
                          <span title={fmtAbs(data.resolved_at)}>
                            {fmtRel(data.resolved_at)}
                          </span>
                        ) : data.resolved ? (
                          "yes"
                        ) : (
                          "—"
                        )}
                      </StateRow>
                    </dl>
                  </div>
                </div>

                {/* NOTIFIED — per channel, failure reason inline (never
                    tooltip-only) */}
                <div className="card order-5">
                  <div className="card-header">
                    <span className="card-title">Notified</span>
                  </div>
                  <div className="card-body">
                    {channels.length === 0 && (
                      <p className="text-xs text-ink-400">
                        None enabled at the time.
                      </p>
                    )}
                    <ul className="space-y-1.5">
                      {channels.map((c) => (
                        <li
                          key={c}
                          className="flex items-center gap-2 text-xs text-ink-100"
                        >
                          <ChannelIcon id={c} size={13} />
                          <span className="min-w-0 flex-1 truncate">{c}</span>
                          {data.notify_status === "failed" ? (
                            <span className="inline-flex items-center gap-1 text-sev-critical">
                              <XCircle size={12} aria-hidden /> failed
                            </span>
                          ) : data.notify_status === "sent" ? (
                            <span className="inline-flex items-center gap-1 text-sev-ok">
                              <CheckCircle2 size={12} aria-hidden /> sent
                            </span>
                          ) : data.notify_status ? (
                            <Pill>{data.notify_status}</Pill>
                          ) : null}
                        </li>
                      ))}
                    </ul>
                    {data.notify_status === "failed" && data.notify_error && (
                      <p className="mt-2 break-all rounded-md bg-sev-critical/10 p-2 font-mono text-2xs leading-snug text-sev-critical">
                        {data.notify_error}
                      </p>
                    )}
                  </div>
                </div>

                {/* ON-CALL */}
                <div className="card order-9">
                  <div className="card-header">
                    <span className="card-title">On-call</span>
                  </div>
                  <div className="card-body text-xs">
                    {data.oncall_triggered ? (
                      <span className="inline-flex items-center gap-1.5 text-sev-warn">
                        <PhoneCall size={12} aria-hidden />
                        escalation triggered
                      </span>
                    ) : (
                      <p className="text-ink-400">Not triggered.</p>
                    )}
                  </div>
                </div>

                {/* ASSIGNED */}
                <AssignedCard
                  className="order-10"
                  teamID={data.assigned_team_id}
                  memberIDs={data.assigned_member_ids}
                  onEdit={() => setAssignOpen(true)}
                />

                {/* FACTS */}
                <div className="card order-11">
                  <div className="card-header">
                    <span className="card-title">Facts</span>
                  </div>
                  <div className="card-body grid grid-cols-2 gap-x-4 gap-y-3 text-xs">
                    <Fact
                      k="ID"
                      v={<span className="break-all font-mono">{data.id}</span>}
                    />
                    <Fact k="Service" v={data.service || "—"} />
                    <Fact
                      k="Source"
                      v={data.source ? <SourceBadge source={data.source} /> : "—"}
                    />
                    <Fact k="Team" v={data.team_id || "—"} />
                  </div>
                </div>

                {/* Agent context — populated for agent-emitted incidents;
                    shows empty rows for everything else (kept from the old
                    page for layout consistency). */}
                <div className="card order-12">
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
                            to={`/agent/logs/${patternID}`}
                            className="font-mono text-link hover:underline"
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
                        <pre className="mt-1 max-h-32 overflow-auto whitespace-pre-wrap break-words rounded-md bg-surface-sunken p-2 font-mono text-2xs leading-snug text-ink-100">
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
          </main>

          {/* Mobile bottom action bar — 44px+ targets, safe-area padded;
              same flows as the header actions. */}
          <div className="flex shrink-0 items-center gap-2 border-t border-ink-600 bg-surface-sunken px-4 pb-[max(8px,env(safe-area-inset-bottom))] pt-2 lg:hidden">
            <button
              className="btn min-h-11 flex-1 justify-center"
              onClick={() => setAssignOpen(true)}
            >
              <UserPlus size={12} />
              Assign
            </button>
            <RunAnalysisButton
              incidentID={id}
              onRan={() => setJustRan(true)}
              className="min-h-11 flex-1 justify-center"
            />
            {!data.resolved && (
              <ResolveButton
                incidentID={data.id}
                className="min-h-11 flex-1 justify-center"
              />
            )}
          </div>

          {assignOpen && (
            <AssignDialog
              incidentID={data.id}
              initialTeamID={data.assigned_team_id}
              initialMemberIDs={data.assigned_member_ids}
              onClose={() => setAssignOpen(false)}
            />
          )}
        </>
      )}
    </>
  );
}

function Fact({ k, v }: { k: string; v: React.ReactNode }) {
  return (
    <div>
      <div className="text-2xs uppercase tracking-wider text-ink-400">{k}</div>
      <div className="text-ink-100">{v}</div>
    </div>
  );
}

// StateRow — one compact label/value line in the STATE card.
function StateRow({
  label,
  children,
}: {
  label: string;
  children: React.ReactNode;
}) {
  return (
    <div className="flex items-center justify-between gap-3">
      <dt className="text-2xs uppercase tracking-wider text-ink-400">{label}</dt>
      <dd className="text-right text-ink-100">{children}</dd>
    </div>
  );
}

// TimelineRow — time (fmtRel + fmtAbs title, "—" when missing) + icon +
// label; failure detail renders inline, never tooltip-only.
function TimelineRow({
  at,
  icon,
  label,
  detail,
}: {
  at?: string;
  icon: React.ReactNode;
  label: React.ReactNode;
  detail?: string;
}) {
  return (
    <li className="flex items-start gap-2.5 py-1 text-xs">
      <span
        className="w-20 shrink-0 pt-px text-right font-mono text-2xs text-ink-300"
        title={at ? fmtAbs(at) : undefined}
      >
        {at ? fmtRel(at) : "—"}
      </span>
      {/* bg-surface ring so the timeline connector ends at the glyph edge
          instead of running through the stroke icons. p-0.5/-ml-0.5 cancel
          out, keeping the glyph center on the connector's 112px line. */}
      <span
        aria-hidden
        className="relative -ml-0.5 mt-0.5 shrink-0 rounded-full bg-surface p-0.5"
      >
        {icon}
      </span>
      <span className="min-w-0 flex-1 text-ink-100">
        {label}
        {detail && (
          <span className="mt-0.5 block break-all font-mono text-2xs leading-snug text-sev-critical">
            {detail}
          </span>
        )}
      </span>
    </li>
  );
}

// RunAnalysisButton fires the analyze mutation (wired to the analyze
// agent). Disabled with an explanation while AI is off (agent.ai.enable);
// outcomes always surface via toast — never silent.
function RunAnalysisButton({
  incidentID,
  onRan,
  className,
}: {
  incidentID: string;
  onRan: () => void;
  className?: string;
}) {
  const cfg = useQuery({
    queryKey: ["agent-config"],
    queryFn: () => api.getAgentConfig(),
    staleTime: 60_000,
  });
  const qc = useQueryClient();
  const toast = useToast();
  const m = useMutation({
    mutationFn: () => api.runAnalysis(incidentID),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["analyses", incidentID] });
      toast.push({ tone: "ok", title: "Analysis complete" });
      onRan();
    },
    onError: (err) => {
      toast.push({
        tone: "error",
        title: "Analysis failed",
        description: err instanceof Error ? err.message : String(err),
        action: { label: "Retry", onClick: () => m.mutate() },
      });
    },
  });

  const aiOff = cfg.isSuccess && !cfg.data.ai?.enable;
  return (
    <span className="inline-flex items-center gap-2">
      <button
        className={clsx("btn", className)}
        disabled={m.isPending || cfg.isLoading || aiOff}
        onClick={() => m.mutate()}
        aria-label={
          aiOff
            ? "Run AI analysis — unavailable: AI is not enabled (agent.ai.enable)"
            : "Run AI analysis"
        }
        title={
          aiOff
            ? "AI is not enabled (agent.ai.enable) — configure it to run analyses."
            : "Run a fresh analysis. Past analyses stay available below."
        }
      >
        {m.isPending ? (
          <>
            <Spinner /> Analysing…
          </>
        ) : (
          <>
            <Sparkles size={11} /> Run analysis
          </>
        )}
      </button>
      {aiOff && (
        <span className="hidden text-2xs text-ink-400 sm:inline">
          AI not enabled (agent.ai.enable)
        </span>
      )}
    </span>
  );
}

// AnalysisPanel decides what the AI ANALYSIS slot shows:
//   - loading: a SkCard placeholder (no layout pop-in).
//   - just after a run in this session (justRan): the newest analysis
//     rendered in full, one time, plus a link to the full history.
//   - on a fresh load when prior analyses exist: only a link card (the
//     full result is NOT re-rendered).
//   - nothing at all when no analysis has ever been run.
function AnalysisPanel({
  incidentID,
  justRan,
  className,
}: {
  incidentID: string;
  justRan: boolean;
  className?: string;
}) {
  const {
    data: analyses,
    isLoading,
    isError,
    error,
    refetch,
    isFetching,
  } = useQuery({
    queryKey: ["analyses", incidentID],
    queryFn: () => api.listAnalyses(incidentID),
    enabled: !!incidentID,
  });

  if (isLoading) return <SkCard lines={2} className={className} />;
  if (isError) {
    return (
      <div className={className}>
        <RetryableError
          context="Couldn't load analyses"
          error={error}
          onRetry={() => refetch()}
          retrying={isFetching}
        />
      </div>
    );
  }

  const list = analyses ?? [];
  if (list.length === 0) return null;

  // Backend returns newest first.
  const latest = list[0];

  if (!justRan) {
    // Reload / first visit: surface a link to the analyses list instead
    // of the full inline result.
    return (
      <Link
        to={`/analyses?incident=${incidentID}`}
        className={clsx(
          "card flex items-center justify-between gap-3 p-3 text-xs hover:border-accent/40",
          className,
        )}
      >
        <span className="flex items-center gap-2">
          <Brain size={14} className="text-link" />
          <span className="text-ink-100">
            {list.length === 1
              ? "1 analysis available"
              : `${list.length} analyses available`}
          </span>
          <span className="text-2xs text-ink-400" title={fmtAbs(latest.requested_at)}>
            latest {fmtRel(latest.requested_at)}
          </span>
        </span>
        <span className="flex items-center gap-1.5 text-link">
          View analysis
          <ChevronRight size={13} />
        </span>
      </Link>
    );
  }

  // Immediately after a fresh run: show the result once, plus a link to
  // the full history.
  return (
    <div className={clsx("space-y-3", className)}>
      <AnalysisCard rec={latest} title="Latest analysis" />
      <Link
        to={`/analyses?incident=${incidentID}`}
        className="inline-flex items-center gap-1.5 text-xs text-link hover:underline"
      >
        View all analyses ({list.length})
        <ChevronRight size={12} />
      </Link>
    </div>
  );
}

// ResolveButton posts to /api/admin/incidents/:id/resolve and refreshes
// both the detail view and the incidents list. Uses ConfirmDialog (not
// window.confirm); outcomes confirm via toast as well as the dialog.
function ResolveButton({
  incidentID,
  className,
}: {
  incidentID: string;
  className?: string;
}) {
  const qc = useQueryClient();
  const toast = useToast();
  const [open, setOpen] = useState(false);
  const m = useMutation({
    mutationFn: () => api.resolveIncident(incidentID),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["incident", incidentID] });
      qc.invalidateQueries({ queryKey: ["incidents"] });
      toast.push({ tone: "ok", title: "Incident resolved" });
      setOpen(false);
    },
    onError: (err) => {
      toast.push({
        tone: "error",
        title: "Resolve failed",
        description: err instanceof Error ? err.message : String(err),
      });
    },
  });
  return (
    <>
      <button
        className={clsx("btn", className)}
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

// AssignedCard renders the team + members currently assigned to the
// incident; the edit flow lives in the shared AssignDialog (opened from
// here or from the page header). Team and member references that no
// longer exist in the roster fall back to their raw id so we don't lie
// to the operator about who is on the hook.
function AssignedCard({
  teamID,
  memberIDs,
  onEdit,
  className,
}: {
  teamID?: string;
  memberIDs?: string[];
  onEdit: () => void;
  className?: string;
}) {
  const teamsQ = useQuery({ queryKey: ["teams"], queryFn: api.listTeams });
  const membersQ = useQuery({
    queryKey: ["members"],
    queryFn: api.listMembers,
  });

  const memberById = new Map(
    (membersQ.data ?? []).map((m) => [m.id, m.name] as const),
  );
  const teamName = teamID
    ? ((teamsQ.data ?? []).find((t) => t.id === teamID)?.name ?? teamID)
    : null;

  const hasAssignment = !!teamID || (memberIDs ?? []).length > 0;
  const loading = teamsQ.isLoading || membersQ.isLoading;
  const failed = teamsQ.isError || membersQ.isError;

  return (
    <div className={clsx("card", className)}>
      <div className="card-header">
        <span className="card-title">Assigned</span>
        <button className="btn" onClick={onEdit}>
          <UserPlus size={11} />
          {hasAssignment ? "Change" : "Assign"}
        </button>
      </div>
      <div className="card-body space-y-2 text-xs">
        {loading ? (
          <div aria-hidden className="space-y-2">
            <div className="sk h-3 w-1/2" />
            <div className="sk h-3 w-2/3" />
          </div>
        ) : failed ? (
          <RetryableError
            context="Couldn't load roster"
            error={teamsQ.error ?? membersQ.error}
            onRetry={() => {
              if (teamsQ.isError) teamsQ.refetch();
              if (membersQ.isError) membersQ.refetch();
            }}
            retrying={teamsQ.isFetching || membersQ.isFetching}
          />
        ) : (
          <>
            <div>
              <div className="text-2xs uppercase tracking-wider text-ink-400">
                Team
              </div>
              <div className="mt-1">
                {teamID ? (
                  <Pill tone="accent">{teamName}</Pill>
                ) : (
                  <span className="text-ink-400">—</span>
                )}
              </div>
            </div>
            <div>
              <div className="text-2xs uppercase tracking-wider text-ink-400">
                Members
              </div>
              <div className="mt-1 flex flex-wrap gap-1">
                {(memberIDs ?? []).length === 0 && (
                  <span className="text-ink-400">—</span>
                )}
                {(memberIDs ?? []).map((id) => (
                  <Pill key={id}>{memberById.get(id) ?? id.slice(0, 8)}</Pill>
                ))}
              </div>
            </div>
          </>
        )}
      </div>
    </div>
  );
}
