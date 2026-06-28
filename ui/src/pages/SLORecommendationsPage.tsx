import { useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Gauge, Info, Lock, Sparkles } from "lucide-react";

import {
  api,
  ApiError,
  type SLOAutodefineConfig,
  type SLORecommendation,
} from "@/lib/api";
import { displayService, fmtAbs, fmtRel } from "@/lib/format";
import {
  cadenceDirty,
  enableToggleState,
  formatConfidence,
  formatObjective,
  isLockedStatus,
} from "@/lib/sloAdvisor";
import { TopBar } from "@/components/TopBar";
import { Pill } from "@/components/Pill";
import { EmptyState } from "@/components/feedback";
import { SkRows } from "@/components/Skeleton";
import { RetryableError } from "@/components/RetryableError";

// SLORecommendationsPage — the read-only "SLI/SLO auto-define" view (epic X29).
// Per service it shows the SLIs/SLOs the SLO Advisor recommends (indicator,
// target, window, rationale, confidence) plus when they were generated, and an
// admin-only cadence control.
//
// Enterprise-gated: the endpoint returns 403 without an `intelligence` license
// and is absent (404) on an OSS binary — either way the page renders the locked
// upsell state, never real data. When AI is disabled the page shows a clear
// "OFF" banner with the server-supplied reason. Advisory only: adopting an
// objective is a human action; the page mutates nothing but the cadence.

const PAGE_TITLE = "SLI/SLO auto-define";
const SUBTITLE =
  "On a schedule the agent reviews each service's signals and proposes the SLIs and SLOs a team should adopt.";

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
  const list = useMemo(
    () => recs.data?.recommendations ?? [],
    [recs.data],
  );

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
        <p className="mb-3 max-w-3xl text-xs text-ink-300">{SUBTITLE}</p>

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
            {list.map((r) => (
              <ServiceCard key={r.service} rec={r} />
            ))}
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
      setMsg({ ok: true, text: `Cadence set to ${data.cadence}` });
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

  const current = cfg.data.cadence;
  const value = draft || current;
  const tgl = enableToggleState(
    cfg.data.status.enabled,
    cfg.data.enabled,
    cfg.data.status.off_reason,
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
              "Turn the agent's scheduled SLI/SLO review on or off for this org."
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
              How often the advisor re-reviews each service. Minimum{" "}
              {cfg.data.min_cadence}.
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
            disabled={save.isPending || !cadenceDirty(value, current)}
            onClick={() => save.mutate(value.trim())}
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

function ServiceCard({ rec }: { rec: SLORecommendation }) {
  return (
    <div className="card p-4">
      <div className="mb-2 flex flex-wrap items-center justify-between gap-2">
        <div className="flex items-center gap-2">
          <Sparkles size={14} className="text-link" />
          <h3 className="text-sm font-semibold text-ink-50">
            {displayService(rec.service)}
          </h3>
          <Pill>v{rec.version}</Pill>
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
      <div className="overflow-hidden rounded-md border border-ink-600">
        <table className="ddt">
          <thead>
            <tr>
              <th className="w-44">Indicator</th>
              <th className="w-24">Type</th>
              <th className="w-28">Target</th>
              <th className="w-20">Window</th>
              <th>Rationale</th>
              <th className="w-24 text-right">Confidence</th>
            </tr>
          </thead>
          <tbody>
            {rec.slis.map((s, i) => (
              <tr key={`${s.name}-${i}`}>
                <td className="font-medium text-ink-100">{s.name}</td>
                <td>
                  <Pill>{s.type}</Pill>
                </td>
                <td className="tabular-nums text-ink-100">
                  {formatObjective(s)}
                </td>
                <td className="tabular-nums text-ink-200">
                  {s.window_days}d
                </td>
                <td className="text-2xs text-ink-300">
                  {s.rationale}
                  <span className="ml-1 font-mono text-ink-400">
                    ({s.signal})
                  </span>
                </td>
                <td className="text-right tabular-nums text-ink-200">
                  {formatConfidence(s.confidence)}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
}
