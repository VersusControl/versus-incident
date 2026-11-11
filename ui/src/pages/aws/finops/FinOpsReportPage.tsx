import { useEffect, useRef } from "react";
import { Link, useSearchParams } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import clsx from "clsx";
import { Printer } from "lucide-react";
import { RetryableError } from "@/components/RetryableError";
import { SkCard } from "@/components/Skeleton";
import { useDeploymentOrg } from "@/lib/useDeploymentOrg";
import { api, type FinOpsBreakdownRow, type FinOpsReportView } from "@/lib/api";
import {
  EFFORT_WORDS,
  RISK_WORDS,
  bridgeGeometry,
  bridgeX,
  commitmentOnlyLine,
  compareRowName,
  periodYear,
  fmtDay,
  fmtMoney,
  fmtShare,
  alternativeLines,
  fmtSaving,
  fmtStamp,
} from "@/lib/finops";
import { FinOpsTiles } from "./FinOpsTiles";
import { ResourceFootnote, ResourcesNotNamed } from "./parts";
import "./finops-print.css";

// The monthly report as a print layout (/aws/finops/report?period=YYYY-MM):
// two pages, light only. Page 1: the three cards and what moved the cost,
// with one plain sentence under the top three drivers. Page 2: spend by
// service and by account (by team when tags split the cost), savings,
// expected increases and "About this report". With ?print=1 the browser's
// print dialog opens once the model has rendered. All data comes from one
// GET; the page makes no request after it loads.

function ReportHeader({ report, deployment, compact }: { report: FinOpsReportView; deployment: string; compact?: boolean }) {
  const notFinal = report.not_final;
  const title = `AWS cost report: ${report.period?.label ?? ""}`;
  const sub = `Deployment: ${deployment}, ${report.notes.coverage.accounts}`;
  if (compact) {
    return (
      <div className="flex items-baseline justify-between border-b border-ink-300 pb-2">
        <span className="text-sm font-semibold text-ink-50">{title}</span>
        <span className="text-2xs text-ink-300">{sub}</span>
      </div>
    );
  }
  return (
    <header className="flex items-start justify-between gap-4 border-b border-ink-300 pb-3">
      <div>
        <h1 className="text-xl font-semibold text-ink-50">{title}</h1>
        <p className="mt-0.5 text-xs text-ink-300">{sub}</p>
      </div>
      {report.frozen ? (
        <span className="rounded-full border border-ink-100 px-2.5 py-0.5 text-2xs font-medium text-ink-100">
          Frozen {fmtDay(report.frozen.at.slice(0, 10))}
        </span>
      ) : notFinal ? (
        <span className="rounded-full border border-sev-warn px-2.5 py-0.5 text-2xs font-medium text-sev-warn">Not final</span>
      ) : null}
    </header>
  );
}

function ReportBridge({ report }: { report: FinOpsReportView }) {
  const b = report.bridge;
  const currency = report.currency;
  if (!b.available) {
    return <p className="text-xs text-ink-300">{b.note || "Needs a full previous month"}</p>;
  }
  if (b.steady || (b.rows ?? []).length === 0) {
    return <p className="text-xs text-ink-200">{b.steady_note || "The cost was steady"}</p>;
  }
  const g = bridgeGeometry(b);
  // A month to date is compared with the same days of the base month: the
  // base row names those days, never the whole month.
  const baseName = compareRowName(report.compare, periodYear(report.period));
  const totalName = report.period?.open ? `${report.period.short} to date` : report.period?.short ?? "";
  const showOwner = (b.rows ?? []).some((r) => r.owner && r.owner.name !== "Unallocated") && b.owner_column.show;
  return (
    <table className="w-full border-separate border-spacing-0 text-2xs">
      <caption className="sr-only">What moved the cost</caption>
      <thead>
        <tr className="text-left text-ink-300">
          <th scope="col" className="w-[30%] pb-1 font-normal">
            <span className="sr-only">Driver</span>
          </th>
          <th scope="col" className="pb-1 font-normal">Service</th>
          {showOwner && <th scope="col" className="pb-1 font-normal">Owner</th>}
          <th scope="col" className="w-[30%] pb-1 font-normal">
            <span className="sr-only">Bar</span>
          </th>
          <th scope="col" className="pb-1 text-right font-normal">
            <span className="sr-only">Amount</span>
          </th>
        </tr>
      </thead>
      {g.bars.map((bar) => {
        const edge = bar.kind === "base" || bar.kind === "total";
        const row = bar.row;
        const name = bar.kind === "base" ? baseName : bar.kind === "total" ? totalName : row?.label ?? "";
        const a = bridgeX(g, Math.min(bar.from, bar.to));
        const z = bridgeX(g, Math.max(bar.from, bar.to));
        const fill = edge ? "fill-ink-300" : bar.amount < 0 ? "fill-sev-ok-solid" : row?.status === "open" ? "fill-sev-high-solid" : "fill-sev-high-solid/40";
        return (
          <tbody key={bar.key} className="finops-block">
            <tr>
              <th scope="row" className={clsx("py-1 text-left", edge ? "font-semibold text-ink-50" : "font-normal text-ink-100")}>
                {name}
                {row?.status === "expected" && (
                  <span className="ml-1.5 rounded border border-ink-300 px-1 text-[9px] text-ink-200">Expected</span>
                )}
                {bar.kind === "driver" && row?.resources === "too_large" && (
                  <span className="ml-1.5">
                    <ResourcesNotNamed note={b.resource_note} print />
                  </span>
                )}
              </th>
              <td className="py-1 text-ink-200">{bar.kind === "driver" ? row?.service_label : bar.kind === "other" ? "several" : ""}</td>
              {showOwner && <td className="py-1 text-ink-200">{bar.kind === "driver" ? row?.owner?.name : ""}</td>}
              <td className="py-1">
                <svg role="img" aria-label={`${name}, ${fmtMoney(bar.amount, { signed: !edge, currency })}`} viewBox="0 0 100 10" preserveAspectRatio="none" className="block h-3 w-full">
                  <rect x={a * 100} y={1} width={Math.max((z - a) * 100, 0.6)} height={8} className={fill} />
                </svg>
              </td>
              <td
                className={clsx(
                  "py-1 pl-2 text-right font-mono tabular-nums",
                  edge ? "font-semibold text-ink-50" : bar.amount < 0 ? "text-sev-ok" : "text-ink-100",
                )}
              >
                {fmtMoney(bar.amount, { signed: !edge, currency })}
              </td>
            </tr>
            {row?.sentence && (
              <tr>
                <td colSpan={showOwner ? 5 : 4} className="pb-1.5 text-[10px] text-ink-300">
                  {row.sentence}
                </td>
              </tr>
            )}
          </tbody>
        );
      })}
    </table>
  );
}

function SpendTable({ title, rows, periodShort, currency, max }: { title: string; rows: FinOpsBreakdownRow[]; periodShort: string; currency?: string; max: number }) {
  return (
    <section className="finops-block" aria-label={title}>
      <table className="w-full text-2xs">
        <caption className="pb-1 text-left text-sm font-semibold text-ink-50">{title}</caption>
        <thead>
          <tr className="text-ink-300">
            <th scope="col" className="w-[25%] text-left font-normal">
              <span className="sr-only">Name</span>
            </th>
            <th scope="col" className="font-normal">
              <span className="sr-only">Bar</span>
            </th>
            <th scope="col" className="text-right font-normal">{periodShort}</th>
            <th scope="col" className="w-12 text-right font-normal">Share</th>
          </tr>
        </thead>
        <tbody>
          {rows.map((r) => (
            <tr key={r.key}>
              <th scope="row" className="py-0.5 text-left font-normal text-ink-100">
                {r.estimated ? "≈ " : ""}
                {r.label}
              </th>
              <td className="py-0.5">
                <svg aria-hidden="true" viewBox="0 0 100 6" preserveAspectRatio="none" className="block h-2.5 w-full">
                  {r.amount_micros > 0 && <rect width={Math.max((r.amount_micros / max) * 100, 1)} height={6} className="fill-ink-300" />}
                </svg>
              </td>
              <td className={clsx("py-0.5 pl-3 text-right font-mono tabular-nums", r.amount_micros < 0 ? "text-sev-ok" : "text-ink-100")}>
                {fmtMoney(r.amount_micros, { currency })}
              </td>
              <td className="py-0.5 text-right font-mono tabular-nums text-ink-300">{fmtShare(r.share_bp)}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </section>
  );
}

function PageFooter({ n }: { n: number }) {
  return <footer className="finops-page-footer border-t border-ink-400 pt-2 text-right text-2xs text-ink-300">Page {n} of 2</footer>;
}

export function FinOpsReport({ report, deployment }: { report: FinOpsReportView; deployment: string }) {
  const currency = report.currency;
  const periodShort = report.period?.short ?? "";
  const service = report.spend_by_service ?? [];
  const account = report.spend_by_account ?? [];
  const team = report.spend_by_team ?? [];
  // One scale for every spend table, so bars compare across them.
  const max = Math.max(1, ...[...service, ...account, ...team].map((r) => r.amount_micros));
  const savings = report.savings.top ?? [];
  const expected = report.notes.expected ?? [];
  const notFinal = report.not_final;
  const about: Array<[string, string]> = [
    ["Accounts", report.notes.coverage.accounts],
    ["Measure", report.notes.coverage.measure_label],
    ["Data", `To ${fmtDay(report.notes.coverage.last_complete_day)}, ${notFinal ? "not final" : "complete"}`],
    report.frozen ? ["Frozen", `${fmtStamp(report.frozen.at)} UTC`] : ["Generated", `${fmtStamp(report.generated_at)} UTC`],
  ];
  if ((report.notes.missing ?? []).length) about.push(["Missing", (report.notes.missing ?? []).join("; ")]);
  about.push(["Access", "Read only"]);
  const summaryView = { ...report, ...report.summary };
  const resourceNote = report.bridge.available ? report.bridge.resource_note : undefined;

  return (
    <>
      <section className="finops-page" data-page="1" aria-label="Page 1">
        <ReportHeader report={report} deployment={deployment} />
        <div className="mt-5 finops-block">
          <FinOpsTiles summary={summaryView} bridgeRows={report.bridge.rows} quiet />
        </div>
        <section className="mt-6 finops-block" aria-label="What moved the cost">
          <div className="mb-2 flex items-baseline justify-between gap-2">
            <h2 className="text-sm font-semibold text-ink-50">What moved the cost</h2>
            {resourceNote && <ResourcesNotNamed note={resourceNote} print />}
          </div>
          <ReportBridge report={report} />
          {resourceNote && <ResourceFootnote note={resourceNote} />}
        </section>
        <PageFooter n={1} />
      </section>

      <section className="finops-page" data-page="2" aria-label="Page 2">
        <ReportHeader report={report} deployment={deployment} compact />
        <div className="mt-5 space-y-5">
          {service.length > 0 && <SpendTable title="Spend by service" rows={service} periodShort={periodShort} currency={currency} max={max} />}
          {account.length > 1 && <SpendTable title="Spend by account" rows={account} periodShort={periodShort} currency={currency} max={max} />}
          {team.length > 1 && <SpendTable title="Spend by team" rows={team} periodShort={periodShort} currency={currency} max={max} />}
          <section className="finops-block" aria-label="Savings found">
            <h2 className="mb-1 text-sm font-semibold text-ink-50">
              {report.savings.total.count > 0
                ? `Savings found: ${fmtMoney(report.savings.total.monthly_micros, { currency })} a month`
                : "Savings found"}
            </h2>
            {savings.length === 0 && commitmentOnlyLine(report.savings.total, currency) ? (
              <p className="font-mono text-2xs tabular-nums text-ink-100">{commitmentOnlyLine(report.savings.total, currency)}</p>
            ) : savings.length === 0 ? (
              <p className="text-2xs text-ink-300">None found</p>
            ) : (
              <div className="overflow-x-auto">
                <table className="w-full text-2xs">
                  <thead>
                    <tr className="bg-ink-700 text-left text-ink-300">
                      <th scope="col" className="px-1.5 py-1 font-normal">Idea</th>
                      <th scope="col" className="py-1 pr-3 text-right font-normal">Per month</th>
                      <th scope="col" className="py-1 font-normal">Effort</th>
                      <th scope="col" className="py-1 font-normal">Risk</th>
                      <th scope="col" className="py-1 font-normal">Owner</th>
                    </tr>
                  </thead>
                  <tbody>
                    {savings.map((sv) => (
                      <tr key={sv.id} className="border-b border-ink-500">
                        <th scope="row" className="px-1.5 py-1 text-left font-normal text-ink-100">
                          {sv.title}
                          {alternativeLines(sv, currency).map((line) => (
                            <span key={line} className="block text-[10px] text-ink-300">
                              {line}
                            </span>
                          ))}
                        </th>
                        <td className="py-1 pr-3 text-right font-mono tabular-nums text-ink-100">{fmtSaving(sv, currency)}</td>
                        <td className="py-1 text-ink-200">{EFFORT_WORDS[sv.effort] ?? sv.effort}</td>
                        <td className="py-1 text-ink-200">{(RISK_WORDS[sv.risk] ?? sv.risk).replace(" risk", "").replace("No", "None")}</td>
                        <td className="py-1 text-ink-200">{sv.owner.name}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}
          </section>
          {expected.length > 0 && (
            <section className="finops-block" aria-label="Expected increases">
              <h2 className="mb-1 text-sm font-semibold text-ink-50">Expected increases</h2>
              <ul className="space-y-1.5">
                {expected.map((e) => (
                  <li key={e.id} className="flex items-end justify-between text-2xs">
                    <span>
                      <span className="block font-medium text-ink-100">{e.title}</span>
                      {e.expected && (
                        <span className="text-ink-300">
                          {e.expected.reason} until {fmtDay(e.expected.until)}
                        </span>
                      )}
                    </span>
                    {e.expected && <span className="text-ink-300">Marked {fmtDay(e.expected.at.slice(0, 10))}</span>}
                  </li>
                ))}
              </ul>
            </section>
          )}
          <section className="finops-block" aria-label="About this report">
            <h2 className="mb-1 text-sm font-semibold text-ink-50">About this report</h2>
            <dl className="grid grid-cols-[8rem_minmax(0,1fr)] gap-y-0.5 text-2xs">
              {about.map(([k, v]) => (
                <div key={k} className="contents">
                  <dt className="font-semibold text-ink-100">{k}</dt>
                  <dd className="text-ink-200">{v}</dd>
                </div>
              ))}
            </dl>
          </section>
        </div>
        <PageFooter n={2} />
      </section>
    </>
  );
}

export function FinOpsReportPage() {
  const [params] = useSearchParams();
  const period = params.get("period") ?? undefined;
  const compare = params.get("compare") ?? undefined;
  const wantPrint = params.get("print") === "1";
  const org = useDeploymentOrg();
  const report = useQuery({
    queryKey: ["finops", "report", period, compare],
    queryFn: () => api.finopsReport({ period, compare }),
    staleTime: Infinity,
    refetchOnWindowFocus: false,
    refetchOnReconnect: false,
  });
  const printed = useRef(false);
  const data = report.data;
  const deployment = org.data?.org || "default";

  useEffect(() => {
    if (!wantPrint || !data?.enabled || printed.current) return;
    printed.current = true;
    const id = window.requestAnimationFrame(() => window.print());
    return () => window.cancelAnimationFrame(id);
  }, [wantPrint, data]);

  return (
    <div data-theme="light" className="finops-report text-ink-100">
      <div className="finops-no-print mx-auto mb-4 flex w-[210mm] max-w-full items-center justify-between px-1 text-xs">
        <Link to="/aws/finops" className="text-link hover:underline">
          ‹ FinOps
        </Link>
        <button type="button" className="btn" onClick={() => window.print()} disabled={!data?.enabled}>
          <Printer size={12} aria-hidden="true" />
          Print
        </button>
      </div>
      {report.isPending ? (
        <div className="finops-page">
          <SkCard lines={8} />
        </div>
      ) : report.isError ? (
        <div className="finops-page">
          <RetryableError error={report.error} onRetry={() => void report.refetch()} retrying={report.isFetching} context="Couldn't load the report" />
        </div>
      ) : !data?.enabled ? (
        <div className="finops-page">
          <p className="text-sm text-ink-300">Cost data is off</p>
        </div>
      ) : (
        <FinOpsReport report={data} deployment={deployment} />
      )}
    </div>
  );
}
