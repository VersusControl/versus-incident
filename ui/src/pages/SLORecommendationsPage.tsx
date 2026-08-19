import { useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  AlertTriangle,
  Check,
  Copy,
  Gauge,
  Info,
  Lock,
  Sparkles,
} from "lucide-react";

import {
  api,
  ApiError,
  type SLOAdoptedSLO,
  type SLOAdoptResponse,
  type SLOAutodefineConfig,
  type SLORecommendation,
  type SLORecommendationSLI,
} from "@/lib/api";
import { displayService, fmtAbs, fmtRel } from "@/lib/format";
import {
  adoptedDetailFor,
  adoptionTypeOf,
  budgetTone,
  burnAlertFraming,
  cadenceDirty,
  clampPercent,
  enableToggleState,
  formatAdoptAdjustment,
  formatBurnAlert,
  formatBurnAlertDetail,
  formatConfidence,
  formatConsumed,
  formatErrorBudget,
  formatEvidence,
  formatGoDuration,
  formatHeadroom,
  formatNotAdoptable,
  formatObjectiveHuman,
  formatObserved,
  formatObservedP99,
  formatThresholdResync,
  formatWindowDays,
  isLockedStatus,
  normalizeCadence,
  pickConfidence,
  priorityLabel,
  sortByPriority,
} from "@/lib/sloAdvisor";
import { TopBar } from "@/components/TopBar";
import { Pill } from "@/components/Pill";
import { ConfirmDialog } from "@/components/ConfirmDialog";
import { EmptyState } from "@/components/feedback";
import { Pagination } from "@/components/Pagination";
import { SkRows } from "@/components/Skeleton";
import { RetryableError } from "@/components/RetryableError";
import { useToast } from "@/components/toastContext";
import { usePagination } from "@/lib/pagination";

// SLORecommendationsPage — the "SLI/SLO auto-define" view.
// Per service it shows the SLIs/SLOs the SLO Advisor recommends as a block per
// indicator that walks an SRE from "what to measure" through "what will page
// you" to "adopt it", plus an admin-only cadence control.
//
// Enterprise-gated: the endpoint returns 403 without an `intelligence` license
// and is absent (404) on an OSS binary — either way the page renders the locked
// upsell state, never real data. When AI is disabled the page shows a clear
// "OFF" banner with the server-supplied reason.
//
// Every field past {name,type,signal,objective,window_days,rationale,
// confidence} is OPTIONAL platform enrichment: against an older server the
// blocks collapse to the header + rationale rather than rendering empty rows.


const PAGE_TITLE = "SLI/SLO auto-define";

const LOCKED_TITLE = "SLI/SLO auto-define is an Enterprise capability";
const LOCKED_BODY =
  "The SLO Advisor reviews each service's metrics, traces, logs and recent incidents and recommends the SLIs and SLOs to adopt — automatically, on a schedule you control.";

export function SLORecommendationsPage() {
  const recs = useQuery({
    queryKey: ["slo-recommendations"],
    queryFn: () => api.listSLORecommendations(),
    retry: (count, err) => (isLockedStatus(err) ? false : count < 1),
  });

  const locked = recs.isError && isLockedStatus(recs.error);
  const status = recs.data?.status;
  // "Adopt these first" floats to the top when the server ranks services:
  // priority is higher = more urgent, so the list is sorted descending.
  const list = useMemo(
    () => sortByPriority(recs.data?.recommendations ?? []),
    [recs.data],
  );
  // The head of the sorted list is the one service worth calling out by rank.
  const topRanked = useMemo(
    () => list.find((r) => typeof r.priority === "number")?.service,
    [list],
  );

  // Paginate the per-service cards at 100/page. Called before the locked early
  // return so hook order stays stable; renders nothing until >1 page.
  const pg = usePagination(list);

  // ----- locked / upsell state (OSS or unlicensed) ------------------------
  if (locked) {
    return (
      <>
        <TopBar title={PAGE_TITLE} />
        <main className="flex-1 overflow-auto p-4 lg:p-6">
          <div className="card p-8">
            <div className="mx-auto flex max-w-md flex-col items-center gap-3 text-center">
              <div className="rounded-full bg-accent-subtle p-3 text-link">
                <Lock size={20} />
              </div>
              <h2 className="text-sm font-semibold text-ink-50">
                {LOCKED_TITLE}
              </h2>
              <p className="text-xs text-ink-300">{LOCKED_BODY}</p>
              <a
                className="btn btn-primary mt-1"
                href="https://versusincident.com/enterprise"
                target="_blank"
                rel="noreferrer"
              >
                Learn about Enterprise
              </a>
            </div>
          </div>
        </main>
      </>
    );
  }

  return (
    <>
      <TopBar
        title={PAGE_TITLE}
        subtitle={recs.data ? `${recs.data.count} services` : undefined}
      />
      <main className="flex-1 overflow-auto p-4 lg:p-6">
        {status && !status.enabled && (
          <div
            className="mb-4 flex items-start gap-2 rounded-md border border-sev-warn/30 bg-sev-warn/15 p-3 text-xs text-ink-100"
            data-testid="slo-ai-off-banner"
          >
            <Info size={14} className="mt-0.5 shrink-0 text-sev-warn" />
            <span>
              {status.off_reason ||
                "SLI/SLO auto-define is OFF: enable AI and configure an API key to use it."}
            </span>
          </div>
        )}

        <CadenceControl />

        {recs.isError && !locked ? (
          <RetryableError
            error={recs.error}
            onRetry={() => recs.refetch()}
            retrying={recs.isRefetching}
            context="Couldn't load SLI/SLO recommendations"
          />
        ) : recs.isLoading ? (
          <div className="card overflow-hidden">
            <table className="ddt">
              <tbody>
                <SkRows rows={4} cols={1} />
              </tbody>
            </table>
          </div>
        ) : list.length === 0 ? (
          <EmptyState
            title="No recommendations yet"
            hint="The advisor proposes SLIs/SLOs once a service has enough learned signal. Give it a cycle (default every 24h), then refresh."
          />
        ) : (
          <div className="grid gap-3">
            {pg.pageItems.map((r) => (
              <ServiceCard
                key={r.service}
                rec={r}
                topRanked={r.service === topRanked}
              />
            ))}
            <Pagination
              state={pg}
              className="rounded-card border border-ink-600 bg-surface-raised"
            />
          </div>
        )}
      </main>
    </>
  );
}

// CadenceControl loads + edits the per-org auto-define config: the feature
// enable toggle and the review cadence. The config endpoint is RBAC
// runtime:manage-gated, so a non-admin session gets 403 — in that case the
// control renders nothing (the recommendations still show). Only a writer sees
// and edits the config. The enable toggle is DISABLED until the AI hard gate is
// open (status.enabled) so the feature can't be turned on before AI + an API
// key are configured; the server re-validates the same rule (422 ai_required).
function CadenceControl() {
  const qc = useQueryClient();
  const cfg = useQuery({
    queryKey: ["slo-autodefine-config"],
    queryFn: () => api.getSLOAutodefineConfig(),
    retry: (count, err) => (isLockedStatus(err) ? false : count < 1),
  });

  const [draft, setDraft] = useState<string>("");
  const [msg, setMsg] = useState<{ ok: boolean; text: string } | null>(null);

  const save = useMutation({
    mutationFn: (cadence: string) => api.setSLOAutodefineConfig(cadence),
    onSuccess: (data: SLOAutodefineConfig) => {
      setMsg({ ok: true, text: `Cadence set to ${formatGoDuration(data.cadence)}` });
      setDraft("");
      qc.setQueryData(["slo-autodefine-config"], data);
    },
    onError: (err: unknown) => {
      const text =
        err instanceof ApiError ? err.message : "Could not update cadence";
      setMsg({ ok: false, text });
    },
  });

  const toggle = useMutation({
    mutationFn: (enabled: boolean) => api.setSLOAutodefineEnabled(enabled),
    onSuccess: (data: SLOAutodefineConfig) => {
      setMsg({
        ok: true,
        text: data.enabled
          ? "SLI/SLO auto-define enabled"
          : "SLI/SLO auto-define disabled",
      });
      qc.setQueryData(["slo-autodefine-config"], data);
    },
    onError: (err: unknown) => {
      // Surface the server's ai_required rejection (it races a just-disabled AI)
      // gracefully instead of a bare HTTP error.
      const text =
        err instanceof ApiError ? err.message : "Could not change setting";
      setMsg({ ok: false, text });
    },
  });

  // Admin-only: hide the control entirely when the config endpoint is gated.
  if (cfg.isError && isLockedStatus(cfg.error)) return null;
  if (cfg.isLoading || !cfg.data) return null;

  // A partial/malformed config must degrade, not blank the console: every
  // optional branch below falls back rather than dereferencing blind.
  const current = typeof cfg.data.cadence === "string" ? cfg.data.cadence : "";
  // Display only: the server's "24h0m0s" reads as "24h". Saving and the dirty
  // check still run on the raw draft/current, so the API contract is untouched.
  const value = draft || formatGoDuration(current);
  const minCadence = formatGoDuration(
    typeof cfg.data.min_cadence === "string" ? cfg.data.min_cadence : "",
  );
  const tgl = enableToggleState(
    Boolean(cfg.data.status?.enabled),
    Boolean(cfg.data.enabled),
    cfg.data.status?.off_reason,
  );

  return (
    <div className="card mb-4 p-4" data-testid="slo-cadence-control">
      <div
        className="mb-3 flex flex-wrap items-start gap-3 border-b border-ink-700 pb-3"
        data-testid="slo-enable-control"
      >
        <button
          type="button"
          role="switch"
          aria-checked={tgl.checked}
          aria-label="Enable SLI/SLO auto-define"
          disabled={tgl.disabled || toggle.isPending}
          data-testid="slo-enable-toggle"
          onClick={() => {
            setMsg(null);
            toggle.mutate(!tgl.checked);
          }}
          className={`relative inline-flex h-5 w-9 shrink-0 items-center rounded-full transition ${
            tgl.checked ? "bg-link" : "bg-ink-600"
          } ${tgl.disabled ? "cursor-not-allowed opacity-50" : ""}`}
        >
          <span
            className={`inline-block h-4 w-4 transform rounded-full bg-white transition ${
              tgl.checked ? "translate-x-4" : "translate-x-0.5"
            }`}
          />
        </button>
        <div className="min-w-0">
          <div className="text-xs font-semibold text-ink-100">
            Enable SLI/SLO auto-define
          </div>
          <div className="text-2xs text-ink-400">
            {tgl.disabled ? (
              <span data-testid="slo-enable-offreason">{tgl.reason}</span>
            ) : (
              "When enabled, the agent reviews each service and recommends the SLIs and SLOs to adopt."
            )}
          </div>
        </div>
      </div>
      <div className="flex flex-wrap items-end gap-3">
        <div className="flex items-center gap-2">
          <Gauge size={14} className="text-link" />
          <div>
            <div className="text-xs font-semibold text-ink-100">
              Review cadence
            </div>
            <div className="text-2xs text-ink-400">
              How often the advisor re-reviews each service.
              {minCadence && ` Minimum ${minCadence}.`}
            </div>
          </div>
        </div>
        <div className="flex items-center gap-2">
          <input
            className="input w-28"
            value={value}
            aria-label="Cadence"
            placeholder="24h"
            onChange={(e) => {
              setDraft(e.target.value);
              setMsg(null);
            }}
          />
          <button
            className="btn btn-primary"
            disabled={save.isPending || !cadenceDirty(draft, current)}
            onClick={() => save.mutate(normalizeCadence(draft))}
          >
            {save.isPending ? "Saving…" : "Save"}
          </button>
        </div>
        {msg && (
          <span
            className={`text-2xs ${msg.ok ? "text-sev-ok" : "text-sev-critical"}`}
            role="status"
          >
            {msg.text}
          </span>
        )}
      </div>
    </div>
  );
}

function ServiceCard({
  rec,
  topRanked,
}: {
  rec: SLORecommendation;
  topRanked?: boolean;
}) {
  const priority = priorityLabel(rec.priority, Boolean(topRanked));
  return (
    <div className="card p-4" data-testid="slo-service-card">
      <div className="mb-2 flex flex-wrap items-center justify-between gap-2">
        <div className="flex items-center gap-2">
          <Sparkles size={14} className="text-link" />
          <h3 className="text-sm font-semibold text-ink-50">
            {displayService(rec.service)}
          </h3>
          <Pill>v{rec.version}</Pill>
          {priority && (
            <Pill
              tone={priority.tone}
              title={priority.title}
              className="whitespace-nowrap"
            >
              <span data-testid="slo-priority">{priority.text}</span>
            </Pill>
          )}
        </div>
        <span
          className="text-2xs text-ink-400"
          title={fmtAbs(rec.generated_at)}
        >
          Generated {fmtRel(rec.generated_at)}
        </span>
      </div>
      {rec.summary && (
        <p className="mb-3 text-xs text-ink-300">{rec.summary}</p>
      )}
      <div className="grid gap-2">
        {rec.slis.map((s, i) => (
          <SLIBlock
            key={`${s.name}-${i}`}
            service={rec.service}
            sli={s}
            adopted={adoptedDetailFor(s, rec)}
          />
        ))}
      </div>
    </div>
  );
}

// Row is one labelled line inside an SLI block. Callers only render a Row when
// they actually have the value, so an older server produces a shorter block
// rather than a column of em dashes.
function Row({
  label,
  testid,
  children,
}: {
  label: string;
  testid?: string;
  children: React.ReactNode;
}) {
  return (
    <div
      className="flex flex-wrap items-baseline gap-x-2 gap-y-0.5"
      data-testid={testid}
    >
      <span className="w-32 shrink-0 text-2xs uppercase tracking-wider text-ink-400">
        {label}
      </span>
      <div className="min-w-0 flex-1 text-xs text-ink-200">{children}</div>
    </div>
  );
}

// SLIBlock answers, top to bottom: what to measure → what to target → where you
// are now → what it costs you → what will page you → how to implement → adopt.
function SLIBlock({
  service,
  sli,
  adopted,
}: {
  service: string;
  sli: SLORecommendationSLI;
  adopted?: SLOAdoptedSLO;
}) {
  const observed = formatObserved(sli);
  const observedP99 = formatObservedP99(sli);
  const headroom = formatHeadroom(sli.headroom_pp);
  const budget = formatErrorBudget(sli.error_budget, sli.window_days);
  const consumedRatio = sli.error_budget?.consumed_ratio;
  const consumed = formatConsumed(consumedRatio);
  const burns = sli.burn_alerts ?? [];
  const evidence = formatEvidence(sli.evidence);
  const confidence = formatConfidence(pickConfidence(sli));
  const hasMeasure = Boolean(sli.good_events || sli.valid_events || sli.query);
  // Only an objective the platform actually enforces may claim it pages you.
  const burnFraming = burnAlertFraming(
    sli,
    Boolean(sli.adopted) || Boolean(adopted),
  );

  return (
    <div
      className="rounded-md border border-ink-600 bg-surface-raised/40 p-3"
      data-testid="slo-sli-block"
      data-sli={sli.name}
    >
      <div className="mb-2 flex flex-wrap items-center gap-2">
        <span className="text-xs font-semibold text-ink-50">{sli.name}</span>
        <Pill>{sli.type}</Pill>
        <span
          className="text-xs font-semibold tabular-nums text-ink-100"
          data-testid="slo-sli-objective"
        >
          {formatObjectiveHuman(sli)}
        </span>
        {sli.breaching && (
          <Pill tone="bad" title="Current attainment is below the objective.">
            <AlertTriangle size={11} aria-hidden="true" />
            <span data-testid="slo-sli-breaching">Breaching</span>
          </Pill>
        )}
        <span className="ml-auto flex items-center gap-1">
          <span
            className="text-2xs tabular-nums text-ink-300"
            title={evidence ?? undefined}
            data-testid="slo-sli-confidence"
          >
            {confidence} confidence
          </span>
        </span>
      </div>

      <div className="grid gap-1.5">
        {(observed || observedP99) && (
          <Row label="Now vs target" testid="slo-sli-current">
            {observed && (
              <span className="tabular-nums text-ink-100">now {observed}</span>
            )}
            {observed && headroom && (
              <span
                className={`ml-1 tabular-nums ${
                  sli.breaching ? "text-sev-critical" : "text-sev-ok"
                }`}
              >
                · {headroom}
              </span>
            )}
            {observedP99 && (
              <span
                className="ml-1 tabular-nums text-ink-300"
                data-testid="slo-sli-p99"
                title="Supporting evidence — the objective is the compliance ratio above, not the p99."
              >
                {observed ? "· " : ""}
                {observedP99}
              </span>
            )}
          </Row>
        )}

        {budget && (
          <Row label="Error budget" testid="slo-sli-budget">
            <span className="tabular-nums text-ink-100">budget {budget}</span>
            {consumed && (
              <>
                <span className="ml-1 tabular-nums text-ink-300">
                  · {consumed}
                </span>
                <span
                  aria-hidden="true"
                  className="ml-2 inline-block h-1.5 w-24 overflow-hidden rounded-full align-middle bg-ink-600"
                >
                  <span
                    className={`block h-full rounded-full ${
                      budgetTone(consumedRatio) === "bad"
                        ? "bg-sev-critical"
                        : budgetTone(consumedRatio) === "warn"
                          ? "bg-sev-warn"
                          : "bg-sev-ok"
                    }`}
                    style={{ width: `${clampPercent(consumedRatio)}%` }}
                  />
                </span>
              </>
            )}
          </Row>
        )}

        {burns.length > 0 && (
          <Row label={burnFraming.label} testid="slo-sli-burn">
            <span className="flex flex-wrap items-baseline gap-x-2 gap-y-0.5">
              {burns.map((a, i) => (
                <span
                  key={`${a.name}-${i}`}
                  className="tabular-nums text-ink-200"
                  title={formatBurnAlertDetail(a)}
                >
                  {i > 0 && <span className="mr-2 text-ink-500">·</span>}
                  {formatBurnAlert(a)}
                </span>
              ))}
            </span>
            <span
              className="mt-0.5 block text-2xs text-ink-400"
              data-testid="slo-sli-burn-note"
              data-enforcement={burnFraming.mode}
            >
              {burnFraming.note}
            </span>
          </Row>
        )}

        {hasMeasure && (
          <Row label="How to measure" testid="slo-sli-measure">
            {(sli.good_events || sli.valid_events) && (
              <span className="block text-2xs text-ink-300">
                {sli.good_events && <>good: {sli.good_events}</>}
                {sli.good_events && sli.valid_events && " · "}
                {sli.valid_events && <>valid: {sli.valid_events}</>}
              </span>
            )}
            {sli.query && (
              <span className="mt-1 flex items-start gap-1">
                <code
                  className="min-w-0 flex-1 overflow-x-auto whitespace-pre rounded border border-ink-600 bg-surface px-2 py-1 font-mono text-2xs text-ink-100"
                  data-testid="slo-sli-query"
                >
                  {sli.query}
                </code>
                <CopyButton text={sli.query} label={sli.name} />
              </span>
            )}
          </Row>
        )}

        <Row label="Why" testid="slo-sli-rationale">
          <span className="text-2xs text-ink-300">
            {sli.rationale}
            <span className="ml-1 font-mono text-ink-400">({sli.signal})</span>
            {evidence && (
              <span className="ml-1 text-ink-400">· {evidence}</span>
            )}
          </span>
        </Row>
      </div>

      <AdoptControl service={service} sli={sli} adopted={adopted} />
    </div>
  );
}

// copyText writes to the clipboard with no new dependency: the async
// Clipboard API where it exists (HTTPS / localhost), and a hidden-textarea
// execCommand fallback for insecure origins and older browsers.
async function copyText(text: string): Promise<boolean> {
  try {
    if (navigator.clipboard?.writeText) {
      await navigator.clipboard.writeText(text);
      return true;
    }
  } catch {
    // Permission denied or a non-secure context — try the legacy path.
  }
  try {
    const ta = document.createElement("textarea");
    ta.value = text;
    ta.setAttribute("readonly", "");
    ta.style.position = "fixed";
    ta.style.opacity = "0";
    document.body.appendChild(ta);
    ta.select();
    const ok = document.execCommand("copy");
    ta.remove();
    return ok;
  } catch {
    return false;
  }
}

function CopyButton({ text, label }: { text: string; label: string }) {
  const toast = useToast();
  const [copied, setCopied] = useState(false);

  return (
    <button
      type="button"
      className="btn shrink-0"
      aria-label={`Copy ${label} query`}
      title="Copy query"
      data-testid="slo-sli-copy"
      onClick={async () => {
        const ok = await copyText(text);
        setCopied(ok);
        if (ok) window.setTimeout(() => setCopied(false), 2000);
        toast.push({
          tone: ok ? "ok" : "error",
          title: ok ? "Query copied" : "Could not copy the query",
          description: ok ? undefined : "Select the query and copy it manually.",
        });
      }}
    >
      {copied ? (
        <Check size={12} aria-hidden="true" />
      ) : (
        <Copy size={12} aria-hidden="true" />
      )}
      <span>{copied ? "Copied" : "Copy"}</span>
    </button>
  );
}

// AdoptControl turns a recommendation into a tracked objective, and lets an
// operator hand it back to the auto-derived one. Both directions are
// org-visible, so they go through the shared ConfirmDialog and spell out what
// the platform will do. The server's `adopted` is the source of truth — the
// local flag is only an optimistic bridge until the refetch lands, so the state
// survives a reload. Availability and latency are separate objectives: the
// revert names which one to hand back, so adopting both on one service and
// reverting only one works. When the server says the SLI is not adoptable it
// renders the server's reason instead of a dead button; when the server says
// nothing at all (older build) it renders nothing.
function AdoptControl({
  service,
  sli,
  adopted,
}: {
  service: string;
  sli: SLORecommendationSLI;
  adopted?: SLOAdoptedSLO;
}) {
  const qc = useQueryClient();
  const toast = useToast();
  const [confirming, setConfirming] = useState<"adopt" | "revert" | null>(null);
  const [optimistic, setOptimistic] = useState<boolean | null>(null);
  const [seen, setSeen] = useState(sli.adopted);
  // A fresh server answer always wins over the optimistic flag.
  if (seen !== sli.adopted) {
    setSeen(sli.adopted);
    setOptimistic(null);
  }
  const isLatency = adoptionTypeOf(sli) === "latency";

  const adopt = useMutation({
    mutationFn: () => api.adoptSLORecommendation(service, sli.name),
    onSuccess: (res: SLOAdoptResponse) => {
      setConfirming(null);
      setOptimistic(true);
      toast.push({
        tone: "ok",
        title: `Now tracking “${sli.name}”`,
        description: formatAdoptAdjustment(res) ?? undefined,
      });
      qc.invalidateQueries({ queryKey: ["slo-recommendations"] });
    },
  });

  const revert = useMutation({
    mutationFn: () =>
      isLatency
        ? api.unadoptSLORecommendation(service, "latency")
        : api.unadoptSLORecommendation(service),
    onSuccess: () => {
      setConfirming(null);
      setOptimistic(false);
      toast.push({ tone: "ok", title: `Reverted “${sli.name}” to auto-derived` });
      qc.invalidateQueries({ queryKey: ["slo-recommendations"] });
    },
  });

  if (sli.adoptable === undefined) return null;

  const isAdopted = optimistic ?? (sli.adopted || Boolean(adopted));
  const resync = formatThresholdResync(adopted);

  if (isAdopted) {
    return (
      <div className="mt-2 flex flex-wrap items-center gap-2 border-t border-ink-700 pt-2">
        <Pill tone="good">
          <Check size={11} aria-hidden="true" />
          <span data-testid="slo-adopted">Adopted — the platform is tracking this objective</span>
        </Pill>
        {adopted?.adopted_at && (
          <span
            className="text-2xs text-ink-400"
            title={fmtAbs(adopted.adopted_at)}
            data-testid="slo-adopted-detail"
          >
            Adopted {fmtRel(adopted.adopted_at)}
            {adopted.by ? ` by ${adopted.by}` : ""}
          </span>
        )}
        {resync && (
          <span
            className="text-2xs text-ink-400"
            title={fmtAbs(adopted?.threshold_resync?.at)}
            data-testid="slo-threshold-resync"
          >
            {resync}
          </span>
        )}
        <button
          type="button"
          className="btn ml-auto"
          data-testid="slo-unadopt-btn"
          onClick={() => setConfirming("revert")}
        >
          Revert to auto-derived
        </button>
        {confirming === "revert" && (
          <ConfirmDialog
            title={`Revert “${sli.name}” to auto-derived?`}
            confirmLabel="Revert objective"
            busy={revert.isPending}
            error={revert.error}
            onClose={() => !revert.isPending && setConfirming(null)}
            onConfirm={() => revert.mutate()}
            message={
              <div className="space-y-2">
                <p>
                  {displayService(service)} goes back to the objective the
                  platform derives from observed attainment. Burn-rate alerting
                  continues against that derived target.
                </p>
                <p className="text-ink-300">
                  You can adopt this recommendation again at any time.
                </p>
              </div>
            }
          />
        )}
      </div>
    );
  }

  if (!sli.adoptable) {
    return (
      <div
        className="mt-2 border-t border-ink-700 pt-2 text-2xs text-ink-400"
        data-testid="slo-adopt-manual"
      >
        Manual adoption — {formatNotAdoptable(sli)}
      </div>
    );
  }

  return (
    <div className="mt-2 flex items-center gap-2 border-t border-ink-700 pt-2">
      <button
        type="button"
        className="btn btn-primary"
        data-testid="slo-adopt-btn"
        onClick={() => setConfirming("adopt")}
      >
        Adopt
      </button>
      <span className="text-2xs text-ink-400">
        Start tracking this objective and page on burn.
      </span>
      {confirming === "adopt" && (
        <ConfirmDialog
          title={`Adopt “${sli.name}”?`}
          confirmLabel="Adopt objective"
          busy={adopt.isPending}
          error={adopt.error}
          onClose={() => !adopt.isPending && setConfirming(null)}
          onConfirm={() => adopt.mutate()}
          message={
            <div className="space-y-2">
              <p>
                The platform will track{" "}
                <strong className="text-ink-50">{sli.name}</strong> at{" "}
                <strong className="text-ink-50">
                  {formatObjectiveHuman(sli)}
                </strong>{" "}
                for {displayService(service)} and raise a burn-rate alert when
                the error budget is at risk.
              </p>
              <p className="text-ink-300">
                Accounting window: {formatWindowDays(sli.window_days)}. You can
                stop tracking it later.
              </p>
            </div>
          }
        />
      )}
    </div>
  );
}

