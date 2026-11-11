import { useState } from "react";
import { Link, useParams, useSearchParams } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import clsx from "clsx";
import { Copy, Link2, Sparkles } from "lucide-react";
import { TopBar } from "@/components/TopBar";
import { Breadcrumb } from "@/components/Breadcrumb";
import { Modal } from "@/components/Modal";
import { RetryableError } from "@/components/RetryableError";
import { SkCard } from "@/components/Skeleton";
import { useToast } from "@/components/toastContext";
import { copyText } from "@/lib/clipboard";
import { useEffectiveRole } from "@/lib/useEffectiveRole";
import { adminGateState } from "@/lib/role";
import {
  api,
  finopsErrorFrom,
  type FinOpsCauseNode,
  type FinOpsDayAmount,
  type FinOpsIncreaseDetailView,
} from "@/lib/api";
import {
  CAUSE_WORDS,
  CONFIDENCE_WORDS,
  EFFORT_WORDS,
  RISK_WORDS,
  fmtDay,
  fmtMoney,
  fmtSignal,
  fmtStamp,
  isZeroAmount,
  niceTicks,
  signalRatio,
  STATUS_WORDS,
  statedAmountSpan,
  usageSignalLabel,
} from "@/lib/finops";
import { Card, ClippedText, InfoTip, ResourcesNotNamed, StatusWord } from "./parts";
import { useNarrow } from "./useNarrow";

// Increase detail (/aws/finops/increases/{id}): the title sentence, the
// daily cost before and after, where the increase came from, why (the
// suspect change), who owns it and how to fix it. Every action changes
// Versus state only; nothing here touches AWS.

function BeforeAfterChart({
  series,
  startedOn,
  dailyBefore,
  currency,
  narrow,
}: {
  series: FinOpsDayAmount[];
  startedOn: string;
  dailyBefore: number;
  currency?: string;
  narrow?: boolean;
}) {
  if (series.length === 0) return null;
  // The drawing scales to the card's width; on a phone it is drawn close to
  // the screen's own size so the axis text stays readable.
  const w = narrow ? 360 : 560;
  const h = narrow ? 200 : 260;
  const left = narrow ? 44 : 40;
  const bottom = 20;
  const ticks = niceTicks(Math.max(...series.map((d) => d.amount_micros), 1));
  const max = ticks[ticks.length - 1];
  const slot = (w - left) / series.length;
  const barW = Math.max(slot * 0.72, 1.5);
  const plotH = h - bottom - 8;
  const y = (v: number) => 8 + plotH - (Math.max(v, 0) / max) * plotH;
  const startIdx = series.findIndex((d) => d.day >= startedOn);
  const labelEvery = Math.max(1, Math.ceil(series.length / (narrow ? 4 : 7)));
  const textSize = narrow ? "text-[11px]" : "text-[9px]";
  // Rough width of one label character at that size, in drawing units.
  const charWidth = narrow ? 6.5 : 5.5;
  return (
    <svg role="img" aria-label={`Daily cost, ${fmtDay(series[0].day)} to ${fmtDay(series[series.length - 1].day)}, started ${fmtDay(startedOn)}`} viewBox={`0 0 ${w} ${h}`} className="block h-auto w-full">
      {ticks.map((t) => (
        <g key={t}>
          <line x1={left} x2={w} y1={y(t)} y2={y(t)} className="stroke-ink-500/40" strokeWidth={0.5} />
          <text x={left - 6} y={y(t) + 3} textAnchor="end" className={clsx("fill-ink-400 font-mono", textSize)}>
            {fmtMoney(t, { currency })}
          </text>
        </g>
      ))}
      {series.map((d, i) => {
        const x = left + i * slot + (slot - barW) / 2;
        const after = startIdx >= 0 && i >= startIdx;
        const base = after ? Math.min(dailyBefore, d.amount_micros) : d.amount_micros;
        const label = `${fmtDay(d.day)}: ${fmtMoney(d.amount_micros, { currency })}${d.partial ? ", partial" : ""}`;
        if (d.partial) {
          return (
            <rect key={d.day} x={x} y={y(d.amount_micros)} width={barW} height={y(0) - y(d.amount_micros)} className="fill-transparent stroke-ink-300" strokeDasharray="3 2">
              <title>{label}</title>
            </rect>
          );
        }
        return (
          <g key={d.day}>
            <title>{label}</title>
            <rect x={x} y={y(base)} width={barW} height={y(0) - y(base)} className="fill-ink-400" />
            {after && d.amount_micros > base && (
              <rect x={x} y={y(d.amount_micros)} width={barW} height={y(base) - y(d.amount_micros)} className="fill-sev-high-solid" />
            )}
          </g>
        );
      })}
      {startIdx >= 0 && (
        <line
          x1={left + startIdx * slot}
          x2={left + startIdx * slot}
          y1={8}
          y2={y(0)}
          className="stroke-sev-high"
          strokeDasharray="2 3"
        />
      )}
      {series.map((d, i) => {
        if (!(i === startIdx || (i % labelEvery === 0 && Math.abs(i - startIdx) > labelEvery / 2))) return null;
        const x = left + i * slot + slot / 2;
        // A label that would run past either edge is anchored inside it.
        const half = (fmtDay(d.day).length * charWidth) / 2;
        const anchor = x + half > w ? "end" : x - half < 0 ? "start" : "middle";
        return (
          <text
            key={`l-${d.day}`}
            x={anchor === "end" ? Math.min(x + slot / 2, w) : anchor === "start" ? Math.max(x - slot / 2, 0) : x}
            y={h - 5}
            textAnchor={anchor}
            className={clsx(textSize, i === startIdx ? "fill-sev-high" : "fill-ink-400")}
          >
            {fmtDay(d.day)}
          </text>
        );
      })}
    </svg>
  );
}

function CauseTree({ view, currency }: { view: FinOpsIncreaseDetailView; currency?: string }) {
  // A usage type that moved by less than a cent carries nothing to read.
  const nodes = (view.cause_tree ?? []).filter((n) => !isZeroAmount(n.amount_micros));
  const max = Math.max(1, ...nodes.map((n) => Math.abs(n.amount_micros)));
  const off = view.resource_data === "off";
  // With resource data off the tree stops at usage types: one line says so
  // where it stops, not one per row.
  const offLine = off && nodes.length > 0 && (
    <div className="mt-3 flex items-center gap-2 rounded border border-dashed border-ink-500 px-2 py-1 text-2xs text-ink-300">
      Resource data off
      {view.resource_note && <InfoTip label="About resource data">{view.resource_note}</InfoTip>}
      <a
        href="https://console.aws.amazon.com/cost-management/home#/settings"
        target="_blank"
        rel="noreferrer"
        className="ml-auto text-link hover:underline"
      >
        Turn on ›
      </a>
    </div>
  );
  const resources = (n: FinOpsCauseNode) => {
    if (off) return null;
    const list = n.resources ?? [];
    if (list.length === 0 && view.resource_data === "too_large") {
      return (
        <div className="ml-3 mt-1 flex items-center rounded border border-dashed border-ink-500 px-2 py-1">
          <ResourcesNotNamed note={view.resource_note} />
        </div>
      );
    }
    if (list.length === 0) return null;
    const only = list.length === 1 && list[0].amount_micros === n.amount_micros;
    return (
      <ul className="ml-3 mt-1 border-l border-ink-500 pl-3">
        {list.map((r) => (
          <li key={r.id} className="flex items-center justify-between py-1 text-xs">
            <span className="font-mono text-ink-100">{r.id}</span>
            <span className="font-mono text-2xs tabular-nums text-ink-300">
              {only ? "All of it" : fmtMoney(r.amount_micros, { signed: true, currency })}
            </span>
          </li>
        ))}
      </ul>
    );
  };
  return (
    <>
      <ul className="space-y-4">
        {nodes.map((n) => (
          <li key={n.usage_type}>
            <div className="grid grid-cols-[minmax(0,1fr)_8rem_auto] items-center gap-3">
              <div className="min-w-0">
                <div className="text-xs text-ink-100">{n.label}</div>
                <ClippedText text={n.usage_type} className="font-mono text-2xs text-ink-400" />
              </div>
              <svg aria-hidden="true" viewBox="0 0 100 4" preserveAspectRatio="none" className="h-1 w-full">
                <rect width={Math.max((Math.abs(n.amount_micros) / max) * 100, 1)} height={4} rx={1} className={n.amount_micros > 0 ? "fill-sev-high-solid" : "fill-sev-ok-solid"} />
              </svg>
              <span className={clsx("font-mono text-xs tabular-nums", n.amount_micros > 0 ? "text-sev-high" : "text-sev-ok")}>
                {fmtMoney(n.amount_micros, { signed: true, currency })}
              </span>
            </div>
            {resources(n)}
          </li>
        ))}
      </ul>
      {offLine}
    </>
  );
}

/** Plain explanation for Slack or email: what, why, who and the fix. */
function increaseSummary(v: FinOpsIncreaseDetailView): string {
  const inc = v.increase;
  // The server's sentence may already end in a full stop; never add a second.
  const lines = [/[.!?]$/.test(inc.sentence.trim()) ? inc.sentence.trim() : `${inc.sentence.trim()}.`];
  const s = inc.suspect_change;
  if (s.state === "found" && s.summary) lines.push(`Why: ${s.summary}${s.actor ? ` by ${s.actor}` : ""}${s.at ? ` (${fmtStamp(s.at)} UTC)` : ""}.`);
  lines.push(`Owner: ${inc.owner.name} (${inc.owner.rule_text}).`);
  const fix = v.related_savings?.[0];
  if (fix) lines.push(`Fix: ${fix.line}`);
  for (const other of (inc.suspects ?? []).slice(1)) {
    lines.push(`${other.confidence_word || "Possible"}: ${other.summary ?? ""}${other.actor ? ` by ${other.actor}` : ""}.`);
  }
  return lines.join("\n");
}

/**
 * Reads one account and AWS service by usage type and team tag, so the
 * usage types and owners above become exact. It is a paid read: the cost is
 * shown and confirmed first.
 */
function DrillDown({ id, account, serviceLabel }: { id: string; account: string; serviceLabel: string }) {
  const qc = useQueryClient();
  const toast = useToast();
  const [confirming, setConfirming] = useState(false);
  const drill = useMutation({
    mutationFn: async () => {
      // The route takes the AWS service name, which the Details depth labels with.
      const details = await qc.fetchQuery({
        queryKey: ["finops", "increase", id, "details"],
        queryFn: () => api.finopsIncrease(id, { depth: "details" }),
      });
      return api.finopsDrillDown({ account, service: details.increase.service_label || serviceLabel });
    },
    onSuccess: () => {
      setConfirming(false);
      void qc.invalidateQueries({ queryKey: ["finops"] });
      toast.push({ tone: "ok", title: "Detail loaded" });
    },
    onError: (e) => {
      const info = finopsErrorFrom(e, "aws");
      toast.push({ tone: "error", title: info.title, description: info.detail });
    },
  });
  return (
    <>
      <button type="button" className="mt-3 text-xs text-link hover:underline" onClick={() => setConfirming(true)}>
        Read exact detail from AWS ›
      </button>
      {confirming && (
        <Modal
          title="Read exact detail"
          onClose={() => setConfirming(false)}
          closeDisabled={drill.isPending}
          footer={
            <>
              <button type="button" className="btn" onClick={() => setConfirming(false)} disabled={drill.isPending}>
                Cancel
              </button>
              <button type="button" className="btn btn-primary-strong" onClick={() => drill.mutate()} disabled={drill.isPending}>
                {drill.isPending ? "Reading…" : "Read for about $0.01"}
              </button>
            </>
          }
        >
          <dl className="grid grid-cols-[7rem_minmax(0,1fr)] gap-y-1 text-xs">
            <dt className="text-ink-300">Reads</dt>
            <dd className="text-ink-100">
              {serviceLabel} in {account}, by usage type and team
            </dd>
            <dt className="text-ink-300">Cost</dt>
            <dd className="text-ink-100">1 paid Cost Explorer request ($0.01), more for long results</dd>
            <dt className="text-ink-300">Kept</dt>
            <dd className="text-ink-100">For today</dd>
          </dl>
        </Modal>
      )}
    </>
  );
}

function MarkExpectedDialog({ id, onClose }: { id: string; onClose: () => void }) {
  const qc = useQueryClient();
  const toast = useToast();
  const [reason, setReason] = useState("");
  const [until, setUntil] = useState("");
  const today = new Date();
  const maxDate = new Date(today.getTime() + 90 * 86_400_000).toISOString().slice(0, 10);
  const minDate = today.toISOString().slice(0, 10);
  const save = useMutation({
    mutationFn: () => api.finopsSetIncreaseStatus(id, { status: "expected", reason: reason.trim(), until }),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ["finops"] });
      toast.push({ tone: "ok", title: "Marked expected" });
      onClose();
    },
    onError: (e) => {
      const info = finopsErrorFrom(e, "local");
      toast.push({ tone: "error", title: info.title, description: info.detail });
    },
  });
  const valid = reason.trim() !== "" && until !== "" && until <= maxDate && until >= minDate;
  return (
    <Modal
      title="Mark expected"
      onClose={onClose}
      closeDisabled={save.isPending}
      footer={
        <>
          <button type="button" className="btn" onClick={onClose} disabled={save.isPending}>
            Cancel
          </button>
          <button type="button" className="btn btn-primary-strong" disabled={!valid || save.isPending} onClick={() => save.mutate()}>
            Mark expected
          </button>
        </>
      }
    >
      <div className="space-y-3">
        <label className="block">
          <span className="field-label">Reason</span>
          <input className="input w-full" value={reason} onChange={(e) => setReason(e.target.value)} maxLength={200} />
        </label>
        <label className="block">
          <span className="field-label">Until</span>
          <input type="date" className="input w-full" value={until} min={minDate} max={maxDate} onChange={(e) => setUntil(e.target.value)} />
        </label>
      </div>
    </Modal>
  );
}

export function IncreaseDetailPage() {
  const { id = "" } = useParams();
  const [params] = useSearchParams();
  const depth = params.get("depth") === "details" ? "details" : "summary";
  const toast = useToast();
  const [marking, setMarking] = useState(false);
  const narrow = useNarrow();
  const detail = useQuery({
    queryKey: ["finops", "increase", id, depth],
    queryFn: () => api.finopsIncrease(id, { depth }),
    enabled: id !== "",
  });
  const qc = useQueryClient();
  const access = useEffectiveRole();
  const gate = adminGateState({
    loading: access.loading,
    enterprise: access.enterprise,
    hasSession: access.hasSession,
    isAdmin: access.isAdmin,
  });
  const canAdmin = gate !== "loading" && gate !== "sign-in" && gate !== "read-only";
  const reopen = useMutation({
    mutationFn: () => api.finopsSetIncreaseStatus(id, { status: "open" }),
    onSuccess: () => void qc.invalidateQueries({ queryKey: ["finops"] }),
  });

  const v = detail.data;
  const inc = v?.increase;
  const currency = v?.currency;
  const title = inc?.title ?? "Increase";

  let heading: React.ReactNode = inc?.sentence ?? "";
  // Highlight the amount the sentence states, matched as the sentence
  // writes it ("$69.33", never the rounded "$69"). With none stated (0)
  // nothing is highlighted and no "$0" is looked for.
  const span = inc ? statedAmountSpan(inc.sentence, inc.stated_change_micros, currency) : null;
  if (inc && span) {
    heading = (
      <>
        {inc.sentence.slice(0, span[0])}
        <span className="font-mono text-sev-high">{inc.sentence.slice(span[0], span[1])}</span>
        {inc.sentence.slice(span[1])}
      </>
    );
  }

  const suspect = inc?.suspect_change;
  // Tied candidates are shown alike, none highlighted.
  const tied = (inc?.suspects ?? []).some((x) => x.tied);
  const signal = v?.usage_signal;
  const fix = v?.related_savings?.[0];
  const ratio = signal ? signalRatio(signal.before_micros, signal.after_micros) : "";

  return (
    <main className="flex min-h-0 flex-1 flex-col overflow-auto">
      <TopBar title="FinOps" subtitle="AWS" />
      <div className="mx-auto w-full max-w-7xl p-4 lg:p-6">
        <Breadcrumb items={[{ label: "AWS", to: "/aws" }, { label: "FinOps", to: "/aws/finops" }, { label: title }]} className="mb-3" />
        {detail.isPending ? (
          <SkCard lines={6} />
        ) : detail.isError ? (
          <RetryableError error={detail.error} onRetry={() => void detail.refetch()} retrying={detail.isFetching} context="Couldn't load the increase" />
        ) : !v || !v.enabled || !inc ? (
          <p className="text-sm text-ink-300">Cost data is off</p>
        ) : (
          <>
            <div className="flex flex-wrap items-start justify-between gap-3">
              <div className="min-w-0">
                <h1 className="text-lg font-semibold text-ink-50">{heading}</h1>
                <div className="mt-2 flex flex-wrap gap-2">
                  <span className="pill">Started {fmtDay(inc.started_on)}</span>
                  <span className="pill">Owner: {inc.owner.name}</span>
                  <span className={clsx("pill", inc.status === "open" && "pill-warn")}>
                    <StatusWord status={inc.status} />
                  </span>
                </div>
              </div>
              <div className="flex flex-wrap gap-2">
                {!canAdmin ? null : inc.status === "open" ? (
                  <button type="button" className="btn" onClick={() => setMarking(true)}>
                    Mark expected…
                  </button>
                ) : inc.status === "expected" ? (
                  <button type="button" className="btn" onClick={() => reopen.mutate()} disabled={reopen.isPending}>
                    Reopen
                  </button>
                ) : null}
                <button
                  type="button"
                  className="btn"
                  onClick={async () => {
                    const ok = await copyText(increaseSummary(v));
                    toast.push({ tone: ok ? "ok" : "error", title: ok ? "Summary copied" : "Copy failed" });
                  }}
                >
                  <Copy size={12} aria-hidden="true" />
                  Copy summary
                </button>
                <button
                  type="button"
                  className="btn"
                  onClick={async () => {
                    const ok = await copyText(window.location.href);
                    toast.push({ tone: ok ? "ok" : "error", title: ok ? "Link copied" : "Copy failed" });
                  }}
                >
                  <Link2 size={12} aria-hidden="true" />
                  Copy link
                </button>
                <Link to="/agent/chat" className="btn border-accent text-ink-50">
                  <Sparkles size={12} aria-hidden="true" />
                  Explain with AI
                </Link>
              </div>
            </div>

            <div className="mt-4 grid gap-4 xl:grid-cols-[minmax(0,1.4fr)_minmax(0,1fr)]">
              <div className="min-w-0 space-y-4">
                <Card
                  title="Daily cost before and after"
                  aside={
                    inc.monthly_impact_micros > 0 && inc.status !== "resolved" ? (
                      <span className="font-mono text-xs text-sev-high">
                        {fmtMoney(inc.monthly_impact_micros, { signed: true, currency })}/mo from now
                      </span>
                    ) : undefined
                  }
                >
                  <BeforeAfterChart series={v.series ?? []} startedOn={inc.started_on} dailyBefore={inc.daily_before_micros} currency={currency} narrow={narrow} />
                </Card>
                <Card
                  title="Where the increase came from"
                  aside={
                    <span className="pill pill-warn">
                      {CAUSE_WORDS[inc.cause] ?? inc.cause}
                      {ratio ? `: ${ratio}` : ""}
                    </span>
                  }
                >
                  <CauseTree view={v} currency={currency} />
                  {canAdmin && <DrillDown id={id} account={inc.account} serviceLabel={inc.service_label} />}
                </Card>
              </div>
              <div className="min-w-0 space-y-4">
                <Card title="Why">
                  {tied ? null : suspect?.state === "found" ? (
                    <div className="rounded-card border border-accent/60 p-3">
                      <div className="flex items-center gap-2 text-2xs">
                        {suspect.confidence && (
                          <span className="pill pill-accent">{suspect.confidence_word || CONFIDENCE_WORDS[suspect.confidence] || suspect.confidence}</span>
                        )}
                        <span className="font-mono text-ink-200">{fmtStamp(suspect.at)}</span>
                      </div>
                      <p className="mt-2 text-sm font-medium text-ink-50">{suspect.summary}</p>
                      {suspect.actor && (
                        <p className="mt-1 text-xs text-ink-300">
                          by <span className="font-mono text-ink-200">{suspect.actor}</span>
                        </p>
                      )}
                      {suspect.note && <p className="mt-0.5 text-xs text-ink-300">{suspect.note}</p>}
                      <div className="mt-2 flex gap-4 text-xs">
                        <Link to={`/aws/changes${suspect.ref ? `?ref=${encodeURIComponent(suspect.ref)}` : ""}`} className="text-link hover:underline">
                          Open in Changes ›
                        </Link>
                        <Link to="/aws/map" className="text-link hover:underline">
                          Show on Map ›
                        </Link>
                      </div>
                    </div>
                  ) : (
                    <p className="flex flex-wrap items-center gap-1 text-xs text-ink-300">
                      {suspect?.state === "collectors_pending"
                        ? "Change history arrives with collectors"
                        : suspect?.state === "history_incomplete"
                          ? "Change history incomplete"
                          : suspect?.state === "account_not_connected"
                            ? "Account not connected"
                            : "No change found in the 48 h before"}
                      {suspect?.state === "history_incomplete" && (
                        <InfoTip label="About change history">
                          {suspect.note || "Change history incomplete for that window"}: no change is named rather than a guess.
                        </InfoTip>
                      )}
                      {suspect?.state === "account_not_connected" && (
                        <Link to="/aws/settings" className="ml-2 text-link hover:underline">
                          Connect ›
                        </Link>
                      )}
                    </p>
                  )}
                  {(tied ? inc.suspects ?? [] : (inc.suspects ?? []).slice(1)).map((other) => (
                    <div key={`${other.ref ?? ""}-${other.at ?? ""}`} className="mt-2 rounded-card border border-ink-500 p-3">
                      <div className="flex items-center gap-2 text-2xs">
                        {(tied || other.confidence) && (
                          <span className="pill">
                            {tied ? "Possible" : other.confidence_word || CONFIDENCE_WORDS[other.confidence ?? ""] || other.confidence}
                          </span>
                        )}
                        <span className="font-mono text-ink-200">{fmtStamp(other.at)}</span>
                      </div>
                      <p className="mt-1.5 text-xs text-ink-200">
                        {other.summary}
                        {other.actor && (
                          <>
                            {" "}
                            by <span className="font-mono">{other.actor}</span>
                          </>
                        )}
                      </p>
                    </div>
                  ))}
                  {tied && (
                    <p className="mt-2 text-2xs text-ink-300">
                      Several changes fit;{" "}
                      <a
                        href="https://console.aws.amazon.com/cost-management/home#/settings"
                        target="_blank"
                        rel="noreferrer"
                        className="text-link hover:underline"
                      >
                        turn on resource-level data in Cost Explorer to narrow it ›
                      </a>
                    </p>
                  )}
                  {signal && (
                    <div className="mt-4 border-t border-ink-500/40 pt-3">
                      <h3 className="text-2xs font-semibold uppercase tracking-wider text-ink-300">Usage signal</h3>
                      <div className="mt-2 grid grid-cols-[minmax(0,1fr)_6rem_auto] items-center gap-3 text-xs">
                        <span className="text-ink-100">{usageSignalLabel(signal.label, signal.unit)}</span>
                        <svg aria-hidden="true" viewBox="0 0 100 12" className="h-3 w-full">
                          <rect y={0} height={4} width={Math.max((signal.before_micros / Math.max(signal.after_micros, 1)) * 100, 2)} className="fill-ink-400" />
                          <rect y={7} height={4} width={100} className="fill-sev-high-solid" />
                        </svg>
                        <span className="font-mono tabular-nums text-ink-100">
                          {fmtSignal(signal.before_micros)} → <span className="text-sev-high">{fmtSignal(signal.after_micros)}</span>
                        </span>
                      </div>
                    </div>
                  )}
                </Card>
                <Card title="Who">
                  <dl className="grid grid-cols-[5rem_minmax(0,1fr)] gap-y-2 text-xs">
                    <dt className="text-ink-300">Owner</dt>
                    <dd className="text-ink-100">
                      <span className="font-semibold">{inc.owner.name}</span>
                      <span className="block text-2xs text-ink-300">Rule: {inc.owner.rule_text}</span>
                    </dd>
                    <dt className="text-ink-300">Alert</dt>
                    <dd className="text-ink-100">
                      {inc.alert
                        ? `Sent to ${inc.alert.channel}, ${fmtDay(inc.alert.sent_at.slice(0, 10))}`
                        : inc.alerted_at
                          ? `Sent ${fmtDay(inc.alerted_at.slice(0, 10))}`
                          : "No alert"}
                    </dd>
                  </dl>
                  {(v.history ?? []).length > 0 && (
                    <ul className="mt-3 space-y-1 border-t border-ink-500/40 pt-2 text-2xs text-ink-300">
                      {(v.history ?? []).map((e) => (
                        <li key={`${e.at}-${e.to}`}>
                          {fmtStamp(e.at)} · {e.by}: {STATUS_WORDS[e.to] ?? e.to}
                          {e.reason ? ` (${e.reason}${e.until ? `, until ${fmtDay(e.until)}` : ""})` : ""}
                        </li>
                      ))}
                    </ul>
                  )}
                </Card>
                {fix && (
                  <Card title="Fix">
                    <p className="text-sm font-medium text-ink-50">{fix.title}</p>
                    <dl className="mt-3 grid grid-cols-3 gap-3 text-xs">
                      <div>
                        <dt className="text-2xs text-ink-300">Saves</dt>
                        <dd className="font-mono text-lg font-semibold text-sev-ok">~{fmtMoney(fix.monthly_micros, { currency })}/mo</dd>
                      </div>
                      <div>
                        <dt className="text-2xs text-ink-300">Effort</dt>
                        <dd className="text-ink-100">{EFFORT_WORDS[fix.effort] ?? fix.effort}</dd>
                      </div>
                      <div>
                        <dt className="text-2xs text-ink-300">Risk</dt>
                        <dd className="text-ink-100">{(RISK_WORDS[fix.risk] ?? fix.risk).replace(" risk", "")}</dd>
                      </div>
                    </dl>
                    {fix.guide?.doc_url && (
                      <a href={fix.guide.doc_url} target="_blank" rel="noreferrer" className="mt-3 inline-block text-xs text-link hover:underline">
                        View guide ›
                      </a>
                    )}
                  </Card>
                )}
              </div>
            </div>
          </>
        )}
      </div>
      {marking && <MarkExpectedDialog id={id} onClose={() => setMarking(false)} />}
    </main>
  );
}
