import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useParams } from "react-router-dom";
import { useState } from "react";
import {
  ArrowLeft,
  ArrowRight,
  Clock,
  Eye,
  LineChart,
  Lock,
  ScrollText,
  ShieldAlert,
} from "lucide-react";
import { api, ApiError, type ServiceIncidentRecent, type ServiceOverride, type ServiceOverrideSource, type ServicePattern } from "@/lib/api";
import { fmtAbs, fmtRel, formatDuration, incidentTitle } from "@/lib/format";
import {
  learnExcludeGate,
  metricExcluded,
  toggleMetricExclusion,
  type LearnExclusions,
} from "@/lib/learnExclude";
import { signalOverrideGate } from "@/lib/serviceOverride";
import { useEffectiveRole } from "@/lib/useEffectiveRole";
import { useToast } from "@/components/toastContext";
import { TopBar } from "@/components/TopBar";
import { Pill, VerdictPill } from "@/components/Pill";
import { EmptyState, Spinner } from "@/components/feedback";
import { RetryableError } from "@/components/RetryableError";
import { SkCard, SkRows } from "@/components/Skeleton";
import { PeekPanel, PeekField } from "@/components/PeekPanel";
import { PatternBaselines } from "@/components/PatternBaselines";

// ServiceDetailPage — the per-service drill-down reached from the Services list.
// It stitches together four sections:
//   • Overview        — first-seen + grace status (OSS)
//   • Logs & patterns — the log-pattern catalog scoped to this service (OSS)
//   • Incidents       — a bounded recent-incident summary (OSS)
//   • Metrics & Traces — the Enterprise /intel half
//
// The Metrics & Traces section degrades PURELY off HTTP status: 403 (licensed
// but feature-off) or 404 (OSS binary — endpoint absent) renders the locked
// upsell card, never real data. No enterprise dependency lives here — the lock
// is driven by the status code alone, so OSS-only builds stay green.
//
// The Disable-Learn controls ride the SAME HTTP-status degrade: the
// "Ignore this service" toggle (Overview) and the per-metric "Ignore"
// checkboxes (Metrics section) render ONLY when the
// /intel probe returned 200 (licensed). Their editable/read-only split is the
// caller's RBAC runtime:manage role (admin/owner); a viewer sees the state
// read-only. In OSS / licensed-off they are absent entirely.

// LEARN_EXCLUSIONS_KEY is the single react-query key both Disable-Learn controls
// read, so the toggle and the per-metric checkboxes share ONE GET of the policy
// and one cache entry — a mutation in either refetches both.
const LEARN_EXCLUSIONS_KEY = ["learn-exclusions"] as const;

// useServiceIntelQuery is the shared /intel probe. It is run by BOTH the
// Metrics section and the Disable-Learn toggle; react-query dedupes them by key
// so there is exactly one network call. A 403 (unlicensed) / 404 (OSS — route
// absent) is terminal, never retried — it is how both surfaces learn to degrade.
function useServiceIntelQuery(name: string) {
  return useQuery({
    queryKey: ["service-intel", name],
    queryFn: () => api.getServiceIntel(name),
    enabled: !!name,
    retry: (count, err) => {
      if (
        err instanceof ApiError &&
        (err.status === 403 || err.status === 404)
      )
        return false;
      return count < 1;
    },
  });
}

// useLearnExclusionsQuery reads the org's Disable-Learn policy — the state
// source for the toggle + checkboxes. It is enabled ONLY once the surface is
// known licensed (the /intel probe returned 200), and a community / wrong-role
// answer (401/403/404/503) is terminal, never retried.
function useLearnExclusionsQuery(enabled: boolean) {
  return useQuery({
    queryKey: LEARN_EXCLUSIONS_KEY,
    queryFn: () => api.getLearnExclusions(),
    enabled,
    retry: (count, err) => {
      if (
        err instanceof ApiError &&
        [401, 403, 404, 503].includes(err.status)
      )
        return false;
      return count < 1;
    },
  });
}

function sevTone(sev: string): "bad" | "warn" | "good" | "default" {
  switch (sev.toLowerCase()) {
    case "critical":
    case "high":
      return "bad";
    case "medium":
      return "warn";
    case "low":
      return "good";
    default:
      return "default";
  }
}

function RecentIncidentRow({ inc }: { inc: ServiceIncidentRecent }) {
  return (
    <tr>
      <td>
        <Link className="link font-mono text-2xs" to={`/incidents/${inc.id}`}>
          {incidentTitle(inc)}
        </Link>
      </td>
      <td className="w-28">
        <Pill tone={sevTone(inc.severity)}>{inc.severity}</Pill>
      </td>
      <td
        className="w-48 text-2xs text-ink-300"
        title={fmtAbs(inc.created_at)}
      >
        {fmtRel(inc.created_at)}
      </td>
    </tr>
  );
}

// ServiceLearnToggle is the Overview "Ignore this service" switch.
// It shares the /intel probe (licensed?) and the policy GET (current
// state) with the Metrics section. It renders nothing on an unlicensed surface,
// read-only (disabled) without runtime:manage, and live for an admin/owner —
// toggling POSTs/DELETEs the one service and refetches the shared policy.
function ServiceLearnToggle({ name }: { name: string }) {
  const qc = useQueryClient();
  const toast = useToast();
  const access = useEffectiveRole();
  const intel = useServiceIntelQuery(name);
  const licensed = intel.isSuccess;
  const gate = learnExcludeGate({ licensed, canManage: access.isAdmin });

  const ex = useLearnExclusionsQuery(licensed);

  const toggle = useMutation({
    mutationFn: (excluded: boolean) =>
      api.setServiceLearnExclusion(name, excluded),
    onSuccess: (data) => {
      // Adopt the authoritative post-change lists, then refetch so any other
      // mounted control reflects the new state (takes effect next worker tick).
      qc.setQueryData(LEARN_EXCLUSIONS_KEY, data);
      qc.invalidateQueries({ queryKey: LEARN_EXCLUSIONS_KEY });
    },
    onError: (err) =>
      toast.push({
        title: "Couldn't update learning policy",
        description: err instanceof Error ? err.message : String(err),
        tone: "error",
      }),
  });

  // Unlicensed (OSS / feature-off) → no control at all, same as the Metrics card.
  if (gate === "absent") return null;

  const readOnly = gate === "readonly";
  const excluded = ex.data?.services.includes(name) ?? false;
  const disabled = readOnly || ex.isPending || toggle.isPending;

  return (
    <div className="mt-3 flex flex-wrap items-start gap-3 border-t border-ink-700 pt-3">
      <button
        type="button"
        role="switch"
        aria-checked={excluded}
        aria-label="Ignore this service"
        disabled={disabled}
        onClick={() => toggle.mutate(!excluded)}
        className={`relative inline-flex h-5 w-9 shrink-0 items-center rounded-full transition ${
          excluded ? "bg-link" : "bg-ink-600"
        } ${disabled ? "cursor-not-allowed opacity-50" : ""}`}
      >
        <span
          className={`inline-block h-4 w-4 transform rounded-full bg-white transition ${
            excluded ? "translate-x-4" : "translate-x-0.5"
          }`}
        />
      </button>
      <div className="min-w-0">
        <div className="text-xs font-semibold text-ink-100">
          Ignore this service
        </div>
        <div className="text-2xs text-ink-400">
          {excluded
            ? "The agent fully ignores this service across training, shadow and detect — it is never learned, evaluated, or alerted on."
            : "The agent watches this service in every mode. Turn on to fully ignore it — no learning, evaluation, or alerts."}
          {readOnly && " Requires the admin role to change."}
        </div>
      </div>
    </div>
  );
}

// SignalReassignCell renders the per-row "Reassign" control for a metric/trace
// baseline. It re-points every future signal matching this baseline's name to
// the chosen service (a metric/trace attribution override), or clears an
// existing override. Read-only callers see the current target only; the whole
// control is absent on an unlicensed surface (the parent never renders it).
function SignalReassignCell({
  source,
  signal,
  current,
  services,
  editable,
}: {
  source: ServiceOverrideSource;
  signal: string;
  current?: ServiceOverride;
  services: string[];
  editable: boolean;
}) {
  const qc = useQueryClient();
  const toast = useToast();

  const reassign = useMutation({
    mutationFn: (service: string) =>
      api.createServiceOverride({ source_type: source, match: signal, service }),
    onSuccess: (_d, service) => {
      qc.invalidateQueries({ queryKey: ["service-overrides"] });
      toast.push({ tone: "ok", title: `Reassigned to "${service}"` });
    },
    onError: (err) =>
      toast.push({
        tone: "error",
        title: "Couldn't reassign",
        description: err instanceof Error ? err.message : String(err),
      }),
  });
  const clear = useMutation({
    mutationFn: (id: string) => api.deleteServiceOverride(id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["service-overrides"] });
      toast.push({ tone: "ok", title: "Reassignment cleared" });
    },
    onError: (err) =>
      toast.push({
        tone: "error",
        title: "Couldn't clear reassignment",
        description: err instanceof Error ? err.message : String(err),
      }),
  });

  if (!editable) {
    return (
      <span className="text-2xs text-ink-300">
        {current ? current.service : "—"}
      </span>
    );
  }
  if (current) {
    return (
      <span className="inline-flex items-center gap-1 text-2xs">
        <span className="font-mono text-ink-100">{current.service}</span>
        <button
          className="btn"
          disabled={clear.isPending}
          aria-label={`Clear reassignment for ${signal}`}
          onClick={() => clear.mutate(current.id)}
        >
          {clear.isPending ? <Spinner /> : "Clear"}
        </button>
      </span>
    );
  }
  return (
    <select
      className="input py-0.5 text-2xs"
      value=""
      disabled={reassign.isPending}
      aria-label={`Reassign ${signal}`}
      onChange={(e) => {
        if (e.target.value) reassign.mutate(e.target.value);
      }}
    >
      <option value="">Reassign…</option>
      {services.map((n) => (
        <option key={n} value={n}>
          {n}
        </option>
      ))}
    </select>
  );
}

// MetricsTracesSection isolates the Enterprise /intel fetch so its locked /
// loading / error states never block the OSS sections above it.
function MetricsTracesSection({ name }: { name: string }) {
  const qc = useQueryClient();
  const toast = useToast();
  const access = useEffectiveRole();
  const { data, isLoading, isError, error, refetch, isRefetching } =
    useServiceIntelQuery(name);

  const locked =
    isError &&
    error instanceof ApiError &&
    (error.status === 403 || error.status === 404);

  // The per-metric exclude checkboxes share the policy GET with the Overview
  // toggle; both are gated identically. Once `data` is present the surface is
  // licensed, so the gate only ever resolves to read-only / editable here.
  const licensed = !!data && !locked;
  const gate = learnExcludeGate({ licensed, canManage: access.isAdmin });
  const ex = useLearnExclusionsQuery(licensed);

  const putEx = useMutation({
    mutationFn: (input: LearnExclusions) => api.setLearnExclusions(input),
    onSuccess: (next) => {
      qc.setQueryData(LEARN_EXCLUSIONS_KEY, next);
      qc.invalidateQueries({ queryKey: LEARN_EXCLUSIONS_KEY });
    },
    onError: (err) =>
      toast.push({
        title: "Couldn't update learning policy",
        description: err instanceof Error ? err.message : String(err),
        tone: "error",
      }),
  });

  const excludeDisabled =
    gate !== "editable" || ex.isPending || putEx.isPending || !ex.data;

  // Attribution-override plumbing (metric/trace reassign). The gate rides the
  // SAME /intel-licensed + RBAC signal as the exclude checkboxes, so it only
  // resolves to read-only / editable on this licensed surface. The service list
  // and override rules feed the per-row Reassign control.
  const overrideGate = signalOverrideGate({
    licensed,
    canManage: access.isAdmin,
  });
  const svcQuery = useQuery({
    queryKey: ["services"],
    queryFn: api.listServices,
    enabled: licensed,
  });
  const overridesQuery = useQuery({
    queryKey: ["service-overrides"],
    queryFn: api.listServiceOverrides,
    enabled: licensed,
  });
  const overrideServices = svcQuery.data
    ? Object.keys(svcQuery.data)
        .filter((n) => n !== "_unknown")
        .sort((a, b) => a.localeCompare(b))
    : [];
  const overrideEditable = overrideGate === "editable";
  const findOverride = (source: ServiceOverrideSource, signal: string) =>
    overridesQuery.data?.find(
      (o) => o.source_type === source && o.match === signal,
    );

  return (
    <section className="card p-4">
      <div className="mb-3 flex items-center gap-2">
        <LineChart size={14} className="text-ink-300" />
        <h2 className="text-sm font-semibold text-ink-50">Metrics & Traces</h2>
      </div>

      {isLoading && <SkRows rows={2} cols={1} />}

      {locked && (
        <div className="mx-auto flex max-w-md flex-col items-center gap-3 py-6 text-center">
          <div className="rounded-full bg-accent-subtle p-3 text-link">
            <Lock size={20} />
          </div>
          <h3 className="text-sm font-semibold text-ink-50">
            Metrics & Traces learning is an Enterprise capability
          </h3>
          <p className="text-xs text-ink-300">
            The agent learns what's normal for this service's request rate,
            errors, latency and trace operations so it can catch problems
            automatically — available in Versus Enterprise.
          </p>
          <a
            className="btn btn-primary mt-1"
            href="https://versusincident.com/enterprise"
            target="_blank"
            rel="noreferrer"
          >
            Learn about Enterprise
          </a>
        </div>
      )}

      {isError && !locked && (
        <RetryableError
          error={error}
          onRetry={() => refetch()}
          retrying={isRefetching}
          context="Couldn't load metrics & traces"
        />
      )}

      {data && !locked && (
        <div className="flex flex-col gap-3 text-xs text-ink-200">
          <div className="flex gap-4">
            <span>
              <span className="text-ink-50">{data.metrics?.length ?? 0}</span>{" "}
              learned metric signals
            </span>
            <span>
              <span className="text-ink-50">{data.traces?.length ?? 0}</span>{" "}
              learned trace signals
            </span>
          </div>

          {data.metrics && data.metrics.length > 0 && (
            <div className="overflow-hidden rounded border border-ink-700">
              <table className="ddt">
                <thead>
                  <tr>
                    <th>Metric signal</th>
                    <th className="w-24">Kind</th>
                    <th className="w-44">Ignore</th>
                    <th className="w-44">Service</th>
                  </tr>
                </thead>
                <tbody>
                  {data.metrics.map((m) => {
                    const checked = metricExcluded(
                      m.signal,
                      ex.data?.metrics,
                    );
                    return (
                      <tr key={`${m.signal}:${m.kind}`}>
                        <td
                          className="font-mono text-2xs text-ink-100"
                          title={m.signal}
                        >
                          {m.signal}
                        </td>
                        <td className="text-2xs text-ink-300">{m.kind}</td>
                        <td>
                          <label className="inline-flex items-center gap-2 text-2xs text-ink-300">
                            <input
                              type="checkbox"
                              checked={checked}
                              disabled={excludeDisabled}
                              aria-label={`Ignore ${m.signal}`}
                              onChange={(e) =>
                                putEx.mutate({
                                  services: ex.data?.services ?? [],
                                  metrics: toggleMetricExclusion(
                                    ex.data?.metrics ?? [],
                                    m.signal,
                                    e.target.checked,
                                  ),
                                  patterns: ex.data?.patterns ?? [],
                                })
                              }
                            />
                            {checked ? "Ignored" : "Active"}
                          </label>
                        </td>
                        <td>
                          <SignalReassignCell
                            source="metric"
                            signal={m.signal}
                            current={findOverride("metric", m.signal)}
                            services={overrideServices}
                            editable={overrideEditable}
                          />
                        </td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
            </div>
          )}

          {data.traces && data.traces.length > 0 && (
            <div className="overflow-hidden rounded border border-ink-700">
              <table className="ddt">
                <thead>
                  <tr>
                    <th>Trace signal</th>
                    <th className="w-24">Kind</th>
                    <th className="w-44">Service</th>
                  </tr>
                </thead>
                <tbody>
                  {data.traces.map((t) => (
                    <tr key={`${t.signal}:${t.kind}`}>
                      <td
                        className="font-mono text-2xs text-ink-100"
                        title={t.signal}
                      >
                        {t.signal}
                      </td>
                      <td className="text-2xs text-ink-300">{t.kind}</td>
                      <td>
                        <SignalReassignCell
                          source="trace"
                          signal={t.signal}
                          current={findOverride("trace", t.signal)}
                          services={overrideServices}
                          editable={overrideEditable}
                        />
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}

          {gate === "readonly" && (
            <p className="text-2xs text-ink-400">
              Ignore rules and service reassignment are read-only — the admin
              role (runtime:manage) is required to change them.
            </p>
          )}

          <Link
            className="link inline-flex items-center gap-1"
            to="/agent/metrics"
          >
            Open the learned-signals view <ArrowRight size={12} />
          </Link>
        </div>
      )}
    </section>
  );
}

export function ServiceDetailPage() {
  const { name = "" } = useParams();
  const { data, isLoading, isError, error, refetch, isRefetching } = useQuery({
    queryKey: ["service-detail", name],
    queryFn: () => api.getServiceDetail(name),
    enabled: !!name,
    retry: (count, err) => {
      // Unknown service (404) is terminal — show the not-found state.
      if (err instanceof ApiError && err.status === 404) return false;
      return count < 1;
    },
  });

  // Peek state for the Logs & patterns table — the eye opens a slide-out with
  // the redacted sample lines and the learned baselines, without leaving the
  // service page. The row's link to the full pattern page stays.
  const [peekPattern, setPeekPattern] = useState<ServicePattern | null>(null);

  const notFound =
    isError && error instanceof ApiError && error.status === 404;

  return (
    <>
      <TopBar
        title="Services"
        subtitle={name}
        actions={
          <Link className="btn" to="/agent/services">
            <ArrowLeft size={12} /> All services
          </Link>
        }
      />

      <main className="flex-1 overflow-auto p-4 lg:p-6">
        {isLoading && <SkCard lines={5} />}

        {notFound && (
          <div className="card p-6">
            <EmptyState
              title={`No service named "${name}".`}
              hint="It may not have been discovered yet, or the name is misspelled."
            />
          </div>
        )}

        {isError && !notFound && (
          <RetryableError
            error={error}
            onRetry={() => refetch()}
            retrying={isRefetching}
            context="Couldn't load service detail"
          />
        )}

        {data && (
          <div className="flex flex-col gap-4">
            {/* Overview ------------------------------------------------ */}
            <section className="card p-4">
              <div className="mb-3 flex items-center gap-2">
                <Clock size={14} className="text-ink-300" />
                <h2 className="text-sm font-semibold text-ink-50">Overview</h2>
              </div>
              <dl className="grid grid-cols-1 gap-3 sm:grid-cols-3">
                <div>
                  <dt className="text-2xs uppercase tracking-wide text-ink-400">
                    First seen
                  </dt>
                  <dd className="text-xs text-ink-100" title={fmtAbs(data.first_seen)}>
                    {fmtAbs(data.first_seen)}{" "}
                    <span className="text-ink-400">
                      ({fmtRel(data.first_seen)})
                    </span>
                  </dd>
                </div>
                <div>
                  <dt className="text-2xs uppercase tracking-wide text-ink-400">
                    Status
                  </dt>
                  <dd className="text-xs">
                    {data.in_grace ? (
                      <Pill tone="warn">in grace</Pill>
                    ) : (
                      <Pill tone="good">tracked</Pill>
                    )}
                  </dd>
                </div>
                <div>
                  <dt className="text-2xs uppercase tracking-wide text-ink-400">
                    Grace remaining
                  </dt>
                  <dd className="text-xs text-ink-100">
                    {data.in_grace
                      ? formatDuration(data.grace_seconds_remaining * 1000)
                      : "—"}
                  </dd>
                </div>
              </dl>
              {/* Disable-Learn toggle (Enterprise) — absent on an
                  unlicensed/OSS binary, read-only without runtime:manage. */}
              {name && <ServiceLearnToggle name={name} />}
            </section>

            {/* Logs & patterns ----------------------------------------- */}
            <section className="card overflow-hidden">
              <div className="flex items-center gap-2 p-4 pb-3">
                <ScrollText size={14} className="text-ink-300" />
                <h2 className="text-sm font-semibold text-ink-50">
                  Logs & patterns
                </h2>
                <Pill className="ml-1">{data.counts.patterns}</Pill>
              </div>
              <table className="ddt">
                <thead>
                  <tr>
                    <th>Pattern</th>
                    <th className="w-20">Count</th>
                    <th className="w-28">Verdict</th>
                    <th className="w-40">Source</th>
                    <th className="w-44">Last seen</th>
                    <th className="w-12 text-right">
                      <span className="sr-only">Action</span>
                    </th>
                  </tr>
                </thead>
                <tbody>
                  {data.patterns.length === 0 && (
                    <tr>
                      <td colSpan={6}>
                        <EmptyState
                          title="No log patterns for this service yet."
                          hint="Patterns appear once the agent clusters this service's logs."
                        />
                      </td>
                    </tr>
                  )}
                  {data.patterns.map((p) => (
                    <tr key={p.id}>
                      <td>
                        <Link
                          className="link font-mono text-2xs"
                          to={`/agent/logs/${p.id}`}
                          title={p.template}
                        >
                          {p.template || p.id}
                        </Link>
                      </td>
                      <td className="tabular-nums">{p.count}</td>
                      <td>
                        <VerdictPill verdict={p.verdict} />
                      </td>
                      <td className="font-mono text-2xs text-ink-300">
                        {p.source}
                      </td>
                      <td
                        className="text-2xs text-ink-300"
                        title={fmtAbs(p.last_seen)}
                      >
                        {fmtRel(p.last_seen)}
                      </td>
                      <td>
                        <div className="flex items-center justify-end gap-1">
                          <button
                            type="button"
                            className="btn p-1"
                            aria-label={`View pattern ${p.id}`}
                            title="View details"
                            onClick={() => setPeekPattern(p)}
                          >
                            <Eye size={14} aria-hidden />
                          </button>
                        </div>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </section>

            {/* Incidents ----------------------------------------------- */}
            <section className="card overflow-hidden">
              <div className="flex flex-wrap items-center gap-2 p-4 pb-3">
                <ShieldAlert size={14} className="text-ink-300" />
                <h2 className="text-sm font-semibold text-ink-50">Incidents</h2>
                <Pill className="ml-1">{data.incidents.count}</Pill>
                <span className="text-2xs text-ink-400">
                  last {data.incidents.window_days} days
                </span>
                <div className="ml-auto flex flex-wrap gap-1">
                  {Object.entries(data.incidents.severities).map(
                    ([sev, n]) => (
                      <Pill key={sev} tone={sevTone(sev)}>
                        {sev} {n}
                      </Pill>
                    ),
                  )}
                </div>
              </div>
              <table className="ddt">
                <thead>
                  <tr>
                    <th>Incident</th>
                    <th className="w-28">Severity</th>
                    <th className="w-48">Created</th>
                  </tr>
                </thead>
                <tbody>
                  {data.incidents.recent.length === 0 && (
                    <tr>
                      <td colSpan={3}>
                        <EmptyState
                          title="No recent incidents for this service."
                          hint={`Window: last ${data.incidents.window_days} days.`}
                        />
                      </td>
                    </tr>
                  )}
                  {data.incidents.recent.map((inc) => (
                    <RecentIncidentRow key={inc.id} inc={inc} />
                  ))}
                </tbody>
              </table>
            </section>

            {/* Metrics & Traces (Enterprise) --------------------------- */}
            {name && <MetricsTracesSection name={name} />}
          </div>
        )}

        {isLoading && (
          <div className="mt-4 flex items-center gap-2 text-2xs text-ink-400">
            <Spinner /> Loading service detail…
          </div>
        )}
      </main>

      <PeekPanel
        open={!!peekPattern}
        onClose={() => setPeekPattern(null)}
        title={
          peekPattern ? (
            <span className="font-mono">{peekPattern.id}</span>
          ) : (
            ""
          )
        }
        footer={
          peekPattern ? (
            <Link
              to={`/agent/logs/${peekPattern.id}`}
              className="btn"
              onClick={() => setPeekPattern(null)}
            >
              Open full page ↗
            </Link>
          ) : undefined
        }
      >
        {peekPattern && (
          <div className="space-y-4">
            <div className="flex items-center gap-2">
              <VerdictPill verdict={peekPattern.verdict} />
              <span className="text-2xs text-ink-400">
                {peekPattern.source || "no source"}
              </span>
            </div>

            <pre className="overflow-auto rounded-md border border-ink-600 bg-surface-sunken p-3 font-mono text-2xs leading-relaxed text-ink-100">
              {peekPattern.template || peekPattern.id}
            </pre>

            <dl className="grid grid-cols-2 gap-x-4 gap-y-2 text-xs">
              <PeekField label="Count">
                <span className="tabular-nums">{peekPattern.count}</span>
              </PeekField>
              <PeekField label="Last seen">
                <span title={fmtAbs(peekPattern.last_seen)}>
                  {fmtRel(peekPattern.last_seen)}
                </span>
              </PeekField>
            </dl>

            <div className="border-t border-ink-600 pt-3">
              <div className="mb-1 text-2xs uppercase tracking-wider text-ink-400">
                Sample log lines
              </div>
              {peekPattern.samples && peekPattern.samples.length > 0 ? (
                <div className="space-y-1.5">
                  {[...peekPattern.samples].reverse().map((s, i) => (
                    <pre
                      key={i}
                      className="overflow-auto whitespace-pre-wrap break-words rounded-md border border-ink-600 bg-surface-sunken p-2 font-mono text-2xs leading-relaxed text-ink-100"
                    >
                      {s}
                    </pre>
                  ))}
                </div>
              ) : (
                <p className="text-xs text-ink-400">No samples captured yet</p>
              )}
            </div>

            <div className="border-t border-ink-600 pt-3">
              <div className="mb-2 text-2xs uppercase tracking-wider text-ink-400">
                What's normal
              </div>
              <PatternBaselines
                frequency={peekPattern.baseline_frequency}
                variance={peekPattern.baseline_variance}
                avg={peekPattern.baseline_avg}
                seasonal={peekPattern.seasonal}
              />
            </div>
          </div>
        )}
      </PeekPanel>
    </>
  );
}
