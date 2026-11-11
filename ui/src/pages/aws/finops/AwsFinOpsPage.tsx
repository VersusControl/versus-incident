import { useCallback, useEffect, useLayoutEffect, useRef, useState } from "react";
import { Link, useSearchParams } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import clsx from "clsx";
import { AlertTriangle, CircleSlash, Clock, Sparkles } from "lucide-react";
import { TopBar } from "@/components/TopBar";
import { Breadcrumb } from "@/components/Breadcrumb";
import { Dropdown } from "@/components/Dropdown";
import { RetryableError } from "@/components/RetryableError";
import { SkCard } from "@/components/Skeleton";
import { useToast } from "@/components/toastContext";
import { copyText } from "@/lib/clipboard";
import { useEffectiveRole } from "@/lib/useEffectiveRole";
import { adminGateState } from "@/lib/role";
import {
  api,
  type FinOpsBreakdownView,
  type FinOpsDepth,
  type FinOpsGroupBy,
  type FinOpsQuery,
  type FinOpsSummaryView,
} from "@/lib/api";
import {
  COMPARE_OPTIONS,
  compareBaseName,
  compareRowName,
  periodYear,
  DEPTH_LABELS,
  fmtClock,
  fmtDay,
  fmtMoney,
  fmtStamp,
  focusRowId,
  hasNoCostData,
  isNotFinal,
  isOrganizationView,
  missingAccountText,
  ownerColumn,
  periodOptions,
  regionName,
  skippedCallsText,
  spendByLayout,
  staleNeedsAccessFix,
  staleReasonText,
  fmtSince,
} from "@/lib/finops";
import { FinOpsTiles } from "./FinOpsTiles";
import { CostBridgeChart } from "./CostBridgeChart";
import { CostTrendChart } from "./CostTrendChart";
import { SpendByService } from "./SpendByService";
import { SpendByCard } from "./TeamSpend";
import { SavingsList } from "./SavingsList";
import { AccountsTab } from "./AccountsTab";
import { ExportMenu } from "./ExportMenu";
import { CostExplorerUnavailable, FinOpsOff } from "./FinOpsOff";
import { InfoTip, ResourcesNotNamed } from "./parts";
import { NOTE_GUTTER, placeNote } from "./notePlacement";
import { useNarrow } from "./useNarrow";
import type { FinOpsExportHandlers } from "./exportHandlers";
import { saveBlockedReason, useFinOpsExport } from "./useFinOpsExport";

// AWS / FinOps: what the cloud costs, what changed, why, who owns it and
// what to save. Three summary cards, the bridge of drivers, spend by service
// and by account | region | team, and savings. The organization view adds an
// Accounts tab. Read-only towards AWS: no control here runs anything there.

const COST_POLICY_FIX = `{
  "Effect": "Allow",
  "Action": ["ce:GetCostAndUsage", "ce:GetCostForecast"],
  "Resource": "*"
}`;

function listParam(p: URLSearchParams, key: string): string[] {
  return (p.get(key) ?? "").split(",").map((s) => s.trim()).filter(Boolean);
}

function useFinOpsQuery(): { q: FinOpsQuery; params: URLSearchParams; set: (k: string, v: string | null) => void } {
  const [params, setParams] = useSearchParams();
  const depth: FinOpsDepth = params.get("depth") === "details" ? "details" : "summary";
  const q: FinOpsQuery = {
    period: params.get("period") ?? undefined,
    compare: params.get("compare") ?? undefined,
    accounts: listParam(params, "accounts"),
    regions: listParam(params, "regions"),
    teams: listParam(params, "teams"),
    depth,
  };
  const set = (k: string, v: string | null) => {
    const next = new URLSearchParams(params);
    if (v === null || v === "") next.delete(k);
    else next.set(k, v);
    setParams(next, { replace: true });
  };
  return { q, params, set };
}

const FRESHNESS_WIDTH = 288;

function Freshness({ summary }: { summary: FinOpsSummaryView }) {
  const [open, setOpen] = useState(false);
  const [pos, setPos] = useState<{ left: number; top: number; width: number } | null>(null);
  const button = useRef<HTMLButtonElement>(null);
  const note = useRef<HTMLDivElement>(null);
  // The same viewport rule as the ⓘ notes: under the button, moved left or
  // above so a phone never cuts off the cause or the fix.
  const place = useCallback(() => {
    if (!button.current) return;
    const r = button.current.getBoundingClientRect();
    const width = document.documentElement.clientWidth || window.innerWidth;
    const height = document.documentElement.clientHeight || window.innerHeight;
    setPos(placeNote(r, note.current?.getBoundingClientRect().height ?? 0, { width, height }, FRESHNESS_WIDTH));
  }, []);
  useLayoutEffect(() => {
    if (open) place();
    else setPos(null);
  }, [open, place]);
  useEffect(() => {
    if (!open) return;
    window.addEventListener("resize", place);
    window.addEventListener("scroll", place, true);
    return () => {
      window.removeEventListener("resize", place);
      window.removeEventListener("scroll", place, true);
    };
  }, [open, place]);
  const sync = summary.sync;
  if (!sync) return null;
  const stale = sync.stale;
  return (
    <div className="relative">
      <button
        ref={button}
        type="button"
        aria-expanded={open}
        onClick={() => setOpen((v) => !v)}
        className={clsx(
          "inline-flex items-center gap-1.5 rounded-control px-2 py-1 text-xs focus:outline-none focus-visible:ring-2 focus-visible:ring-accent",
          stale ? "text-sev-warn" : "text-ink-300 hover:text-ink-100",
        )}
      >
        {stale ? <AlertTriangle size={13} aria-hidden="true" /> : <Clock size={13} aria-hidden="true" />}
        {stale
          ? `Stale since ${fmtDay(sync.stale_detail?.since?.slice(0, 10) ?? sync.last_complete_day)}`
          : `Updated ${fmtClock(sync.updated_at)}`}
      </button>
      {open && (
        <div
          ref={note}
          role="note"
          style={
            pos
              ? { position: "fixed", left: pos.left, top: pos.top, width: pos.width }
              : { position: "fixed", visibility: "hidden", width: FRESHNESS_WIDTH, maxWidth: `calc(100vw - ${2 * NOTE_GUTTER}px)` }
          }
          className="z-dropdown rounded-control border border-ink-500 bg-surface-raised p-3 text-2xs leading-relaxed text-ink-200 shadow-overlay">
          {stale && (
            <p className="mb-1 text-sev-warn">
              {staleReasonText(sync.stale_reason)};{" "}
              {staleNeedsAccessFix(sync.stale_reason)
                ? "the last good data stays until access is fixed in AWS settings."
                : "the last good data stays and the next scheduled refresh retries."}
            </p>
          )}
          <p>Cost data to {fmtDay(sync.last_complete_day)}; AWS data lags up to 24 h.</p>
          {(sync.stale_detail?.days ?? []).length > 0 && (
            <p className="mt-1">Not loaded: {(sync.stale_detail?.days ?? []).map(fmtDay).join(", ")}</p>
          )}
          {sync.next_refresh_at && <p className="mt-1">Next refresh {fmtStamp(sync.next_refresh_at)} UTC</p>}
          {summary.meter.cap > 0 && (
            <p className="mt-1">
              Paid requests today: {summary.meter.used} of {summary.meter.cap}
            </p>
          )}
        </div>
      )}
    </div>
  );
}

function ScopeLabel({ label, value }: { label: string; value: string }) {
  return (
    <span className="inline-flex items-center gap-1 px-1 text-xs text-ink-300">
      {label}: <span className="font-medium text-ink-100">{value}</span>
    </span>
  );
}

function ScopeDropdown({
  label,
  value,
  options,
  onChange,
}: {
  label: string;
  value: string;
  options: Array<{ value: string; label: string }>;
  onChange: (v: string) => void;
}) {
  return (
    <span className="inline-flex items-center gap-1.5 text-xs text-ink-300">
      <span aria-hidden="true">{label}</span>
      <Dropdown value={value} options={options} onChange={onChange} aria-label={label} className="min-w-[8rem]" />
    </span>
  );
}

export function AwsFinOpsPage({ handlers: handlersOverride }: { handlers?: FinOpsExportHandlers } = {}) {
  const { q, params, set } = useFinOpsQuery();
  const toast = useToast();
  const narrow = useNarrow();
  const access = useEffectiveRole();
  const gate = adminGateState({
    loading: access.loading,
    enterprise: access.enterprise,
    hasSession: access.hasSession,
    isAdmin: access.isAdmin,
  });
  const canAdmin = gate !== "loading" && gate !== "sign-in" && gate !== "read-only";

  const summary = useQuery({ queryKey: ["finops", "summary", q], queryFn: () => api.finopsSummary(q) });
  const s = summary.data;
  const on = Boolean(s?.enabled) && !hasNoCostData(s);
  const bridge = useQuery({ queryKey: ["finops", "bridge", q], queryFn: () => api.finopsBridge(q), enabled: on });
  const savings = useQuery({ queryKey: ["finops", "savings", q], queryFn: () => api.finopsSavings(q), enabled: on });
  const byService = useQuery({
    queryKey: ["finops", "breakdown", "service", q],
    queryFn: () => api.finopsBreakdown({ ...q, group_by: "service" }),
    enabled: on,
  });
  const org = isOrganizationView(s);
  const tab = org && params.get("tab") === "accounts" ? "accounts" : "overview";
  const accounts = useQuery({
    queryKey: ["finops", "accounts", q],
    queryFn: () => api.finopsAccounts(q),
    enabled: on && org,
  });

  const layout = spendByLayout(byService.data?.options, byService.data?.default_group_by);
  const requested = params.get("by");
  const selected =
    layout.kind === "single"
      ? layout.groupBy
      : layout.kind === "switch"
        ? layout.choices.some((c) => c.groupBy === requested && !c.disabledReason)
          ? (requested as string)
          : layout.defaultGroupBy
        : "";
  const byGroup = useQuery({
    queryKey: ["finops", "breakdown", selected, q],
    queryFn: () => api.finopsBreakdown({ ...q, group_by: selected as FinOpsGroupBy }),
    enabled: on && selected !== "",
  });
  const spendView: FinOpsBreakdownView | undefined = byGroup.data;

  const exportHandlers = useFinOpsExport({ q, header: s, csvGroupBy: tab === "accounts" ? "account" : "service" });
  const handlers = handlersOverride ?? exportHandlers;
  const notFinal = isNotFinal(s);
  const period = s?.period;
  const compare = s?.compare;
  const sync = s?.sync;
  const currency = s?.currency;
  const saveDisabledReason = saveBlockedReason(s);

  // Header ------------------------------------------------------------------
  const depth = q.depth ?? "summary";
  const headerRight = (
    <div className="flex flex-wrap items-center gap-2">
      {s?.enabled && !hasNoCostData(s) && <Freshness summary={s} />}
      {s && hasNoCostData(s) && (
        <span className="inline-flex items-center gap-1.5 text-xs text-ink-300">
          <CircleSlash size={13} aria-hidden="true" />
          No cost data
        </span>
      )}
      {s && !s.enabled && s.reason === "cost_data_off" && (
        <span className="inline-flex items-center gap-1.5 text-xs text-ink-300">
          <CircleSlash size={13} aria-hidden="true" />
          Cost Explorer reads: off
        </span>
      )}
      {(s?.enabled || hasNoCostData(s)) && (
        <div role="group" aria-label="Depth" className="inline-flex rounded-control border border-ink-500 bg-surface-raised p-0.5">
          {(["summary", "details"] as const).map((d) => (
            <button
              key={d}
              type="button"
              aria-pressed={depth === d}
              onClick={() => set("depth", d === "summary" ? null : d)}
              className={clsx(
                "min-h-7 rounded px-3 text-xs font-medium focus:outline-none focus-visible:ring-2 focus-visible:ring-accent",
                depth === d ? "bg-accent-subtle text-ink-50" : "text-ink-300 hover:text-ink-100",
              )}
            >
              {DEPTH_LABELS[d]}
            </button>
          ))}
        </div>
      )}
      {(s?.enabled || hasNoCostData(s)) && (
        <ExportMenu handlers={handlers} notFinal={notFinal} saveDisabledReason={saveDisabledReason} canSave={canAdmin} disabled={!on} />
      )}
      <Link to="/agent/chat" className="btn border-accent text-ink-50">
        <Sparkles size={12} aria-hidden="true" />
        Ask AI
      </Link>
    </div>
  );

  // Scope bar ---------------------------------------------------------------
  const options = new Map((byService.data?.options ?? []).map((o) => [o.group_by, o]));
  const scope = s?.scope_options;
  const accountRows = (scope?.accounts ?? []).map((a) => ({ key: a.id, label: a.name || a.id }));
  const regionRows = (scope?.regions ?? []).map((r) => ({ key: r, label: regionName(r) }));
  const teamRows = (scope?.teams ?? []).filter((t) => t !== "Unallocated").map((t) => ({ key: t, label: t }));
  const oneAccountName = accountRows[0]?.label ?? "";
  const regionOpt = options.get("region");
  const teamOpt = options.get("team");
  const periodOpts = periodOptions(sync?.last_complete_day).map((o) =>
    period && o.value === (params.get("period") ?? period.key) ? { ...o, label: period.label } : o,
  );
  const periodValue = params.get("period") ?? period?.key ?? "mtd";
  if (period && !periodOpts.some((o) => o.value === periodValue)) periodOpts.unshift({ value: periodValue, label: period.label });

  const scopeBar = s?.enabled && sync && (
    <div className="flex flex-wrap items-center gap-x-4 gap-y-2 border-b border-ink-500/40 pb-3">
      <ScopeDropdown label="Period" value={periodValue} options={periodOpts} onChange={(v) => set("period", v)} />
      {compare && compare.available ? (
        <ScopeDropdown
          label="Compare"
          value={params.get("compare") ?? compare.key}
          options={COMPARE_OPTIONS.map((o) => (o.value === compare.key ? { ...o, label: `vs ${compare.label}` } : o))}
          onChange={(v) => set("compare", v)}
        />
      ) : (
        <ScopeLabel label="Compare" value="none yet" />
      )}
      {sync.accounts_total <= 1 ? (
        oneAccountName && <ScopeLabel label="Account" value={oneAccountName} />
      ) : (
        <ScopeDropdown
          label="Accounts"
          value={q.accounts?.[0] ?? ""}
          options={[{ value: "", label: `All ${sync.accounts_total}` }, ...accountRows.map((r) => ({ value: r.key, label: r.label }))]}
          onChange={(v) => set("accounts", v)}
        />
      )}
      {regionRows.length === 1 || (regionOpt?.reason === "single_value" && regionRows[0]) ? (
        <ScopeLabel label="Region" value={regionRows[0].label} />
      ) : (
        <ScopeDropdown
          label="Regions"
          value={q.regions?.[0] ?? ""}
          options={[
            { value: "", label: org || regionRows.length === 0 ? "All" : `${regionRows.length} regions` },
            ...regionRows.map((r) => ({ value: r.key, label: r.label })),
          ]}
          onChange={(v) => set("regions", v)}
        />
      )}
      {teamOpt?.reason === "no_team_tag" || teamOpt?.reason === "tag_not_active" ? (
        <ScopeLabel label="Team" value="no tag" />
      ) : teamOpt?.reason === "same_tag" && teamRows.length === 1 ? (
        <ScopeLabel label="Team" value={teamRows[0].label} />
      ) : (
        <ScopeDropdown
          label="Team"
          value={q.teams?.[0] ?? ""}
          options={[{ value: "", label: "All" }, ...teamRows.map((r) => ({ value: r.key, label: r.label }))]}
          onChange={(v) => set("teams", v)}
        />
      )}
      {sync.learning && (
        <span className="ml-auto inline-flex items-center gap-1 text-xs text-ink-300">
          <span className="pill">{sync.learning_until ? `Learning until ${fmtDay(sync.learning_until)}` : "Learning"}</span>
          <InfoTip label="About learning">
            {(sync.notes ?? []).find((n) => n.toLowerCase().startsWith("learning")) ?? "Increases start after 14 days of history."}
          </InfoTip>
        </span>
      )}
    </div>
  );

  // Gap lines: what is missing plus the action, one line each ---------------
  const gaps = s?.enabled && sync && !hasNoCostData(s) && (
    <>
      {sync.stale && staleNeedsAccessFix(sync.stale_reason) && (
        // Stored cost stays on the page; access was lost, so the line names
        // the cause behind its ⓘ and the fix beside it.
        <p className="flex flex-wrap items-center gap-2 text-xs text-ink-100" data-gap="stale-access">
          <AlertTriangle size={13} aria-hidden="true" className="text-sev-warn" />
          <span>Stale since {fmtSince(sync.stale_detail?.since) || fmtDay(sync.last_complete_day)}</span>
          <InfoTip label="About stale cost">
            {staleReasonText(sync.stale_reason)}. The cost stored to {fmtDay(sync.last_complete_day)} stays until a refresh can read again.
          </InfoTip>
          <Link to="/aws/settings" className="text-link hover:underline">
            AWS settings ›
          </Link>
        </p>
      )}
      {(sync.missing_accounts ?? []).map((m) => (
        <p key={m.id} className="flex flex-wrap items-center gap-2 text-xs text-ink-100">
          <AlertTriangle size={13} aria-hidden="true" className="text-sev-warn" />
          {missingAccountText(m)}
          <button
            type="button"
            className="text-link hover:underline"
            onClick={async () => {
              const ok = await copyText(COST_POLICY_FIX);
              toast.push({ tone: ok ? "ok" : "error", title: ok ? "Fix copied" : "Copy failed" });
            }}
          >
            Copy fix
          </button>
        </p>
      ))}
      {(sync.skipped_calls ?? []).length > 0 && (
        <p className="flex flex-wrap items-center gap-2 text-xs text-ink-100">
          <AlertTriangle size={13} aria-hidden="true" className="text-sev-warn" />
          {skippedCallsText(sync.skipped_calls)}
          <Link to="/aws/settings" className="text-link hover:underline">
            Raise cap in Settings ›
          </Link>
        </p>
      )}
      {(sync.other_currencies ?? []).map((c) => (
        <p key={c.currency} className="text-xs text-ink-200">
          Also billed in {c.currency}: {fmtMoney(c.amount_micros, { currency: c.currency })} (not converted)
        </p>
      ))}
    </>
  );

  // Body --------------------------------------------------------------------
  let body: React.ReactNode;
  if (summary.isPending) {
    body = <SkCard lines={6} />;
  } else if (summary.isError) {
    body = (
      <RetryableError error={summary.error} onRetry={() => void summary.refetch()} retrying={summary.isFetching} context="Couldn't load FinOps" />
    );
  } else if (hasNoCostData(s)) {
    body = <CostExplorerUnavailable account={sync?.missing_accounts?.[0]} costSource={s?.cost_source} />;
  } else if (!s?.enabled) {
    body = <FinOpsOff reason={s?.reason} canAdmin={canAdmin} sync={s?.sync} />;
  } else if (tab === "accounts") {
    body = accounts.data ? (
      <AccountsTab view={accounts.data} periodShort={period?.short ?? "Cost"} onExportCsv={() => void handlers.csv()} />
    ) : accounts.isError ? (
      <RetryableError error={accounts.error} onRetry={() => void accounts.refetch()} context="Couldn't load accounts" />
    ) : (
      <SkCard lines={8} />
    );
  } else {
    const b = bridge.data;
    const readable = sync && sync.accounts_readable < sync.accounts_total ? `, ${sync.accounts_readable} accounts` : "";
    const year = periodYear(period);
    const baseLabel = readable ? `${compareRowName(compare, year)}${readable}` : compareBaseName(compare, year);
    const totalLabel = readable
      ? `${period?.short ?? ""}${readable}`
      : period?.open
        ? `${period.short} to date`
        : `${period?.short ?? ""} total`;
    const owner = b ? ownerColumn(b, sync?.team_tag.active ?? false) : null;
    const focusId = focusRowId(b?.rows);
    const focusService = b?.rows?.find((r) => r.id === focusId)?.service ?? null;
    const removed = layout.kind === "none";
    body = (
      <div className="space-y-4">
        <FinOpsTiles summary={s} bridgeRows={b?.rows} />
        <section className="card" aria-labelledby="finops-bridge-title">
          <div className="flex flex-wrap items-center justify-between gap-2 px-4 pt-3">
            <div>
              <h2 id="finops-bridge-title" className="card-title">
                What moved the cost
              </h2>
              {b?.offset_note && <p className="mt-0.5 text-2xs text-ink-300">{b.offset_note}</p>}
            </div>
            {b && !b.available && b.note && <span className="text-xs text-ink-300">{b.note}</span>}
            {b?.available && b.resource_note && <ResourcesNotNamed note={b.resource_note} />}
            {b?.available && owner && !owner.show && (
              <span className="inline-flex items-center gap-2 text-xs text-ink-300">
                {owner.note}
                {owner.setUpTags && (
                  <Link to="/aws/settings" className="text-link hover:underline">
                    Set up tags ›
                  </Link>
                )}
              </span>
            )}
          </div>
          <div className="px-4 pb-4 pt-3">
            {bridge.isPending ? (
              <div className="sk h-48 rounded" />
            ) : bridge.isError ? (
              <RetryableError error={bridge.error} onRetry={() => void bridge.refetch()} context="Couldn't load drivers" />
            ) : b && !b.available ? (
              <CostTrendChart days={s.sparkline ?? []} currency={currency} />
            ) : b ? (
              <CostBridgeChart bridge={b} depth={depth} narrow={narrow} baseLabel={baseLabel} totalLabel={totalLabel} />
            ) : null}
          </div>
        </section>
        <div className="grid gap-4 lg:grid-cols-3">
          {byService.data ? (
            <div className={clsx("min-w-0", removed && "lg:col-span-2")}>
              <SpendByService rows={byService.data.rows ?? []} currency={currency} focusService={focusService} showInside={removed} />
            </div>
          ) : (
            <SkCard lines={6} />
          )}
          {layout.kind !== "none" && (
            <SpendByCard
              layout={layout}
              selected={selected}
              onSelect={(g) => set("by", g)}
              rows={spendView?.rows ?? null}
              currency={currency}
              loading={!spendView}
              missingAccounts={sync?.missing_accounts}
              accountsTab={org ? "/aws/finops?tab=accounts" : undefined}
            />
          )}
          {savings.data ? <SavingsList savings={savings.data} canDismiss={canAdmin} /> : <SkCard lines={5} />}
        </div>
      </div>
    );
  }

  const accountsCount = sync?.accounts_total ?? 0;
  return (
    <main className="flex min-h-0 flex-1 flex-col overflow-auto">
      <TopBar title="FinOps" subtitle="AWS" actions={headerRight} />
      <div className="mx-auto w-full max-w-7xl space-y-3 p-4 lg:p-6">
        <div className="flex flex-wrap items-center justify-between gap-2">
          <Breadcrumb items={[{ label: "AWS", to: "/aws" }, { label: "FinOps" }]} />
          {s?.cost_source?.kind === "management" && (
            <span className="inline-flex items-center gap-1 rounded-full border border-ink-500 px-2 py-0.5 text-2xs text-ink-300">
              Cost source: <span className="text-ink-100">management</span>
              <InfoTip label="About the cost source">
                Account {s.cost_source.account_id} is the management account: one read covers the whole bill of
                every linked account.
              </InfoTip>
            </span>
          )}
        </div>
        {org && on && (
          <div role="tablist" aria-label="FinOps view" className="flex gap-4 border-b border-ink-500/40">
            {(
              [
                ["overview", "Overview"],
                ["accounts", "Accounts"],
              ] as const
            ).map(([key, label]) => (
              <button
                key={key}
                type="button"
                role="tab"
                aria-selected={tab === key}
                onClick={() => set("tab", key === "overview" ? null : key)}
                className={clsx(
                  "-mb-px inline-flex items-center gap-1.5 border-b-2 px-1 pb-2 text-sm focus:outline-none focus-visible:ring-2 focus-visible:ring-accent",
                  tab === key ? "border-accent text-ink-50" : "border-transparent text-ink-300 hover:text-ink-100",
                )}
              >
                {label}
                {key === "accounts" && <span className="rounded-full bg-ink-600 px-1.5 text-2xs tabular-nums text-ink-200">{accountsCount}</span>}
              </button>
            ))}
          </div>
        )}
        {scopeBar}
        {gaps}
        {body}
      </div>
    </main>
  );
}
