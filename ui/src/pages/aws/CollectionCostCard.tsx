import { useQuery } from "@tanstack/react-query";
import clsx from "clsx";
import { AlertTriangle, Info, Shield } from "lucide-react";
import { SkCard } from "@/components/Skeleton";
import { RetryableError } from "@/components/RetryableError";
import { isForbidden, useRetryUnlessForbidden } from "@/lib/access";
import { api, ApiError, type AwsAccount } from "@/lib/api";
import {
  cappedLine,
  coverageTitle,
  COST_SOURCE_HINT,
  COST_SOURCE_LABEL,
  COST_SOURCES,
  fmtNumber,
  fmtUsd,
  monthEstimate,
  monthEstimateIsLowerBound,
  type CollectionCostView,
  type CoverageRow,
} from "@/lib/awsEstate";

// Settings / Collection cost: the calls Versus made in the last 24 hours,
// what they may cost a month, the calls by account and source, each
// budget per cycle, and the billed features that are off. Cost carries no
// colour; only a budget that was reached is marked.

function Num({ label, value, prefix }: { label: string; value: string; prefix?: string }) {
  return (
    <div className="min-w-[6rem]" data-number={label}>
      <dt className="text-xs text-ink-300">{label}</dt>
      <dd className="mt-0.5 font-mono text-2xl font-semibold tabular-nums text-ink-50">
        {prefix && <span className="mr-1 font-sans text-xs font-normal text-ink-300">{prefix}</span>}
        {value}
      </dd>
    </div>
  );
}

function SectionTitle({ children }: { children: React.ReactNode }) {
  return <h3 className="text-2xs font-medium uppercase tracking-widest text-ink-300">{children}</h3>;
}

function EstimateLines({ view }: { view: CollectionCostView }) {
  const e = view.estimate_month;
  const lines: Array<{ k: string; label: string; value: string; muted?: boolean }> = [
    { k: "direct", label: "Requests AWS bills directly (S3, KMS, SQS)", value: fmtUsd(e.direct_usd) },
    {
      k: "guardduty",
      label: e.guardduty_known ? "GuardDuty reading these calls as events" : "GuardDuty reading these calls as events, if on",
      value: e.guardduty_usd === null ? "not known" : fmtUsd(e.guardduty_usd),
      muted: e.guardduty_usd === null,
    },
    {
      k: "trail",
      label:
        e.extra_trail_copies === null
          ? "A second trail of yours also records our calls"
          : `A second trail of yours also records our calls (${fmtNumber(e.extra_trail_copies)} found)`,
      value: e.extra_trail_copies_usd === null ? "not read" : `${e.unknown_accounts.length > 0 ? "more than " : ""}${fmtUsd(e.extra_trail_copies_usd)}`,
      muted: e.extra_trail_copies_usd === null,
    },
    {
      k: "siem",
      label: "SIEM or log pipeline",
      value: `about ${e.siem_gb < 10 ? e.siem_gb.toFixed(1) : fmtNumber(Math.round(e.siem_gb))} GB a month, priced by your vendor`,
      muted: true,
    },
  ];
  return (
    <div className="border-t border-ink-500/40 px-4 py-4">
      <SectionTitle>Estimate a month</SectionTitle>
      <dl className="mt-3 max-w-xl space-y-2 text-sm">
        {/* No second trail found: nothing to pay, so no line. Unread accounts already make the headline a lower bound. */}
        {lines.filter((l) => !(l.k === "trail" && (e.extra_trail_copies === 0 || e.extra_trail_copies_usd === 0))).map((l) => (
          <div key={l.k} className="grid grid-cols-[minmax(0,1fr)_auto] gap-x-6" data-estimate={l.k}>
            <dt className="text-ink-100">{l.label}</dt>
            <dd className={clsx("text-right", l.muted ? "text-xs text-ink-300" : "font-mono tabular-nums text-ink-100")}>{l.value}</dd>
          </div>
        ))}
      </dl>
    </div>
  );
}

function BySource({ view, name }: { view: CollectionCostView; name: (id: string) => string }) {
  const accounts = [...new Set(view.by_account_source.map((r) => r.account))];
  if (accounts.length === 0) return null;
  // A source no account called stays off the table.
  const sources = COST_SOURCES.filter((s) => view.by_account_source.some((r) => r.source === s && r.calls > 0));
  const calls = (a: string, s: string) => view.by_account_source.filter((r) => r.account === a && r.source === s).reduce((n, r) => n + r.calls, 0);
  return (
    <div className="border-t border-ink-500/40 px-4 py-4">
      <SectionTitle>Calls by source</SectionTitle>
      <div className="mt-2 overflow-x-auto">
        <table className="w-full min-w-[36rem] text-sm" data-by-source>
          <thead>
            <tr className="text-2xs uppercase tracking-wider text-ink-300">
              <th className="py-2 pr-4 text-left font-medium">Account</th>
              {sources.map((s) => (
                <th key={s} className="px-2 py-2 text-right font-medium">
                  <span className="inline-flex items-center gap-1" title={COST_SOURCE_HINT[s]}>
                    {COST_SOURCE_LABEL[s] ?? s}
                    <Info size={11} aria-hidden="true" />
                  </span>
                </th>
              ))}
              <th className="py-2 pl-2 text-right font-medium">Total</th>
            </tr>
          </thead>
          <tbody>
            {accounts.map((a) => {
              const total = sources.reduce((n, s) => n + calls(a, s), 0);
              return (
                <tr key={a} className="border-t border-ink-500/40" data-account={a}>
                  <td className="max-w-[10rem] truncate py-2 pr-4 text-ink-50" title={a}>
                    {name(a)}
                  </td>
                  {sources.map((s) => (
                    <td key={s} className="px-2 py-2 text-right font-mono text-xs tabular-nums text-ink-100">
                      {fmtNumber(calls(a, s))}
                    </td>
                  ))}
                  <td className="py-2 pl-2 text-right font-mono text-xs font-semibold tabular-nums text-ink-50">{fmtNumber(total)}</td>
                </tr>
              );
            })}
          </tbody>
        </table>
      </div>
    </div>
  );
}

/** "Inspector in platform: more than 3,000, not all read", from the Capped coverage row a reached budget covers. */
function reachedLine(sources: string[], capped: CoverageRow[], name: (id: string) => string): string | null {
  const row = capped.find((r) => sources.includes(r.source));
  if (!row) return null;
  const where = row.accounts.length === 1 ? ` in ${name(row.accounts[0])}` : "";
  return `${coverageTitle(row, name)}${where}: ${cappedLine(row).replace(/^M/, "m")}`;
}

function Budgets({ view, capped, name }: { view: CollectionCostView; capped: CoverageRow[]; name: (id: string) => string }) {
  return (
    <div className="min-w-0">
      <SectionTitle>Budget per cycle</SectionTitle>
      <ul className="mt-3 space-y-1">
        {view.budgets.map((b) => {
          const reached = b.reached;
          const line = reached ? reachedLine(b.coverage_sources ?? [], capped, name) : null;
          const pct = b.limit > 0 ? Math.min(100, Math.round((b.used / b.limit) * 100)) : 0;
          const unit = b.unit === "pages" ? " pages" : "";
          return (
            <li
              key={b.source}
              className={clsx("grid grid-cols-[minmax(0,8rem)_minmax(3rem,1fr)_auto] items-center gap-x-3 rounded px-2 py-1 text-sm", reached && "bg-ink-600/40")}
              data-budget={b.source}
              data-reached={reached || undefined}
            >
              <span className="flex min-w-0 items-center gap-1.5 text-ink-100">
                {reached && <AlertTriangle size={13} aria-label="Budget reached" className="shrink-0 text-sev-warn" />}
                <span className="truncate">{COST_SOURCE_LABEL[b.source] ?? b.source}</span>
              </span>
              <span className="h-1.5 rounded-full bg-ink-600" aria-hidden="true">
                <span className="block h-1.5 rounded-full bg-ink-300" style={{ width: `${pct}%` }} />
              </span>
              <span className="font-mono text-xs tabular-nums text-ink-100">
                {fmtNumber(b.used)} of {fmtNumber(b.limit)}
                {unit}
              </span>
              {line && (
                <span className="col-span-3 pl-5 text-xs text-ink-300" data-reached-line>
                  {line}
                </span>
              )}
            </li>
          );
        })}
      </ul>
    </div>
  );
}

function OptionalOff({ view }: { view: CollectionCostView }) {
  return (
    <div className="min-w-0">
      <SectionTitle>Optional sources that are off</SectionTitle>
      <p className="mt-3 flex items-center gap-2 border-b border-ink-500/40 pb-3 text-sm text-ink-50">
        <Shield size={14} aria-hidden="true" className="text-ink-300" />
        Versus never turns on a billed AWS service
      </p>
      <ul>
        {view.optional_off.map((o) => (
          <li key={o.id} className="grid grid-cols-[minmax(0,1fr)_auto] gap-x-4 border-b border-ink-500/40 py-3 last:border-b-0" data-optional={o.id}>
            <span className="min-w-0">
              <span className="block text-sm text-ink-50">{o.name}</span>
              <span className="block text-xs text-ink-300">{o.what}</span>
            </span>
            <span className="max-w-[14rem] text-right text-xs text-ink-200">{o.price}</span>
          </li>
        ))}
      </ul>
    </div>
  );
}

export function CollectionCostCard({ accounts }: { accounts: AwsAccount[] }) {
  const retry = useRetryUnlessForbidden();
  const cost = useQuery({
    queryKey: ["aws-collection-cost"],
    queryFn: () => api.awsCollectionCost(),
    retry,
    refetchInterval: 5 * 60_000,
    refetchIntervalInBackground: false,
  });
  // The Capped rows of coverage name what a reached budget left unread.
  const coverage = useQuery({ queryKey: ["aws-coverage", "", ""], queryFn: () => api.awsCoverage(), retry });
  const byId = new Map(accounts.map((a) => [a.account_id, a.display_name || a.account_id]));
  const name = (id: string) => byId.get(id) ?? id;
  let body: React.ReactNode;
  if (cost.isPending) body = <SkCard lines={4} />;
  else if (cost.isError) {
    if (isForbidden(cost.error) || (cost.error instanceof ApiError && cost.error.status === 404)) return null;
    body = (
      <div className="p-4">
        <RetryableError error={cost.error} onRetry={() => void cost.refetch()} retrying={cost.isFetching} context="Couldn't load the collection cost" />
      </div>
    );
  } else if (!cost.data.enabled) return null;
  else {
    const v = cost.data;
    const stretchedAccounts = v.stretched_accounts;
    body = (
      <>
        <dl className="flex flex-wrap items-end gap-x-12 gap-y-3 px-4 py-4" data-numbers>
          <Num label="Calls 24 h" value={fmtNumber(v.calls_24h)} />
          <Num
            label="Estimate a month"
            // A lower bound reads "at least $N"; a whole estimate "~$N". Never both.
            prefix={monthEstimateIsLowerBound(v.estimate_month) ? "at least" : undefined}
            value={monthEstimateIsLowerBound(v.estimate_month) ? fmtUsd(monthEstimate(v.estimate_month)) : `~${fmtUsd(monthEstimate(v.estimate_month))}`}
          />
          <Num label="Sources capped" value={fmtNumber(v.sources_capped)} />
          <Num label="Stretched" value={fmtNumber(stretchedAccounts)} />
        </dl>
        {v.ledger_incomplete && (
          <p className="px-4 pb-3 text-xs text-ink-300" data-ledger-incomplete>
            Some calls of the last 24 hours are not counted yet
          </p>
        )}
        <EstimateLines view={v} />
        <BySource view={v} name={name} />
        <div className="grid gap-8 border-t border-ink-500/40 px-4 py-4 lg:grid-cols-2">
          <Budgets view={v} capped={coverage.data?.enabled ? (coverage.data.capped ?? []) : []} name={name} />
          {v.optional_off.length > 0 && <OptionalOff view={v} />}
        </div>
      </>
    );
  }
  return (
    <section className="card min-w-0" id="collection-cost" aria-labelledby="collection-cost-title" data-collection-cost>
      <div className="card-header">
        <h2 id="collection-cost-title" className="card-title">
          Collection cost
        </h2>
      </div>
      {body}
    </section>
  );
}
