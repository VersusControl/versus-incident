import { useState } from "react";
import { Link, useSearchParams } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import clsx from "clsx";
import { AlertTriangle, ArrowUpToLine, ChevronDown, ChevronRight, Copy } from "lucide-react";
import { SkCard } from "@/components/Skeleton";
import { useToast } from "@/components/toastContext";
import { copyText } from "@/lib/clipboard";
import { useRetryUnlessForbidden } from "@/lib/access";
import { api } from "@/lib/api";
import {
  accountListText,
  ACCOUNT_SOURCE,
  cappedLine,
  coverageTitle,
  fixStatement,
  fmtNumber,
  notGapsText,
  notReadLine,
  type CoverageRow,
  type CoverageView,
} from "@/lib/awsEstate";
import { AwsOff, AwsPage, NoAccounts, PageError, ScopeBar } from "../shared/AwsPage";
import { useAwsAccounts } from "../shared/useAwsAccounts";
import { SummaryCard, SummaryNumber } from "../shared/SummaryCard";

// AWS / Coverage: what the stored reads could not cover and why. Every read
// ends in one of five outcomes; only Not read and Capped are gaps. Off in
// AWS is the account's choice and names the check that can't run. Not set
// and not offered are answers, folded away.

type Names = (id: string) => string;

function rowKey(r: CoverageRow): string {
  return `${r.source}:${r.operation ?? ""}:${r.reason}:${r.accounts.join(",")}`;
}

function NotReadRow({ row, names, total }: { row: CoverageRow; names: Names; total: number }) {
  const toast = useToast();
  const [open, setOpen] = useState(false);
  const fix = row.fix ?? [];
  const canExpand = fix.length > 0;
  const copyFix = async () => {
    const ok = await copyText(fixStatement(fix));
    toast.push({ tone: ok ? "ok" : "error", title: ok ? "Fix copied" : "Copy failed" });
  };
  const title = coverageTitle(row, names);
  const line = notReadLine(row, row.accounts.map(names), total);
  let action: React.ReactNode = null;
  if (fix.length > 0) {
    action = (
      <button type="button" className="text-xs text-link hover:underline" onClick={() => void copyFix()} data-copy-fix>
        Copy fix
      </button>
    );
  } else if (row.source === ACCOUNT_SOURCE) {
    action = (
      <Link to="/aws/settings" className="text-xs text-link hover:underline">
        Settings ›
      </Link>
    );
  } else if (row.source.startsWith("organizations")) {
    action = (
      <Link to="/aws/topology" className="text-xs text-link hover:underline">
        Label accounts ›
      </Link>
    );
  } else if (row.reason === "not_on_plan") {
    action = <span className="text-xs text-ink-300">Plan</span>;
  }
  return (
    <li className="border-t border-ink-500/40" data-row={row.source} data-outcome="not_read">
      <div className="grid min-w-0 grid-cols-[1.25rem_minmax(0,1fr)] items-start gap-x-2 px-4 py-2.5 md:grid-cols-[1.25rem_minmax(0,1fr)_minmax(0,1fr)_auto] md:gap-x-6">
        {canExpand ? (
          <button
            type="button"
            className="mt-0.5 text-ink-300 hover:text-ink-100"
            aria-expanded={open}
            aria-label={`${open ? "Hide" : "Show"} the fix for ${title}`}
            onClick={() => setOpen((o) => !o)}
          >
            {open ? <ChevronDown size={14} aria-hidden="true" /> : <ChevronRight size={14} aria-hidden="true" />}
          </button>
        ) : (
          <span aria-hidden="true" />
        )}
        <div className="min-w-0">
          <p className="truncate text-sm font-medium text-ink-50" title={title}>
            {title}
          </p>
          <p className="text-xs text-ink-300" data-line>
            {line}
          </p>
        </div>
        <p className="col-start-2 text-xs text-ink-300 md:col-start-auto md:self-center" data-hides>
          {row.hides ? `Hides: ${row.hides}` : ""}
        </p>
        <div className="col-start-2 md:col-start-auto md:self-center md:text-right">{action}</div>
      </div>
      {open && canExpand && (
        <div className="px-4 pb-3 pl-11" data-fix>
          <p className="mb-2 text-xs text-ink-300">Add this to the read role in each account, then read again</p>
          <div className="relative max-w-xl rounded border border-ink-500/60 bg-surface-sunken">
            <button type="button" className="btn absolute right-2 top-2 px-2 py-0.5 text-2xs" onClick={() => void copyFix()}>
              <Copy size={11} aria-hidden="true" />
              Copy fix
            </button>
            <pre className="overflow-x-auto p-3 pr-24 font-mono text-xs text-ink-100">{fixStatement(fix)}</pre>
          </div>
        </div>
      )}
    </li>
  );
}

function CappedRow({ row, names }: { row: CoverageRow; names: Names }) {
  const where = row.accounts.map(names);
  const title = `${coverageTitle(row, names)}${where.length > 0 && where.length <= 2 ? ` in ${accountListText(where)}` : ""}`;
  return (
    <li className="flex min-w-0 flex-wrap items-center justify-between gap-x-6 gap-y-1 border-t border-ink-500/40 px-4 py-2.5 pl-11" data-row={row.source} data-outcome="capped">
      <div className="min-w-0">
        <p className="truncate text-sm font-medium text-ink-50" title={title}>
          {title}
        </p>
        <p className="text-xs text-ink-300" data-line>
          {cappedLine(row)}
        </p>
      </div>
      <Link to="/aws/settings#collection-cost" className="text-xs text-link hover:underline">
        Budget in Settings ›
      </Link>
    </li>
  );
}

function OffRow({ row, names, total }: { row: CoverageRow; names: Names; total: number }) {
  const title = coverageTitle(row, names);
  const counted = row.total && row.total > 0 && row.seen !== undefined ? `${fmtNumber(row.seen)} of ${fmtNumber(row.total)}` : "";
  const where = accountListText(row.accounts.map(names), total);
  // Account names keep their own case; only "all 4 accounts" opens a line with a capital.
  const line = counted ? `${counted}, ${where}` : where.startsWith("all ") ? `A${where.slice(1)}` : where;
  return (
    <li className="grid min-w-0 gap-x-6 gap-y-1 border-t border-ink-500/40 px-4 py-2.5 pl-11 md:grid-cols-2" data-row={row.source} data-outcome="off_in_aws">
      <div className="min-w-0">
        <p className="truncate text-sm font-medium text-ink-50" title={title}>
          {title}
        </p>
        <p className="text-xs text-ink-300" data-line>
          {line}
        </p>
      </div>
      <p className="self-center text-xs text-ink-300">{row.cant_run ? `Can't run: ${row.cant_run}` : ""}</p>
    </li>
  );
}

function Section({
  id,
  title,
  icon,
  children,
  footer,
}: {
  id: string;
  title: string;
  icon?: React.ReactNode;
  children: React.ReactNode;
  footer?: React.ReactNode;
}) {
  return (
    <section className="card min-w-0" aria-labelledby={`coverage-${id}`} data-section={id}>
      <h2 id={`coverage-${id}`} className="flex items-center gap-2 px-4 py-3 text-sm font-semibold text-ink-50">
        {icon}
        {title}
      </h2>
      <ul>{children}</ul>
      {footer && <div className="border-t border-ink-500/40 px-4 py-3">{footer}</div>}
    </section>
  );
}

function NotGaps({ view }: { view: CoverageView }) {
  const [open, setOpen] = useState(false);
  const { text, parts } = notGapsText(view.not_gaps);
  if (parts === 0) return null;
  return (
    <section className="card min-w-0" data-section="not-gaps">
      <button
        type="button"
        className="flex w-full min-w-0 items-center gap-3 px-4 py-3 text-left"
        aria-expanded={open}
        onClick={() => setOpen((o) => !o)}
      >
        {open ? <ChevronDown size={14} aria-hidden="true" className="text-ink-300" /> : <ChevronRight size={14} aria-hidden="true" className="text-ink-300" />}
        <span className="shrink-0 text-sm text-ink-100">Not gaps ({parts})</span>
        {!open && <span className="truncate text-xs text-ink-300">{text}</span>}
      </button>
      {open && (
        <ul className="space-y-1 px-4 pb-3 pl-11 text-xs text-ink-200">
          {text.split(", ").map((t) => (
            <li key={t}>{t}</li>
          ))}
        </ul>
      )}
    </section>
  );
}

function Body({ view, names, onReadAgain, canWrite, reading }: { view: CoverageView; names: Names; onReadAgain: () => void; canWrite: boolean; reading: boolean }) {
  const s = view.summary;
  const total = s.accounts_total;
  return (
    <>
      <SummaryCard>
        <div className="flex flex-wrap items-start justify-between gap-3 px-4 pt-3">
          <dl className="flex flex-wrap items-end gap-x-10 gap-y-3" data-numbers>
            <SummaryNumber k="accounts" label="Accounts" value={`${s.accounts_read}/${s.accounts_total}`} />
            <SummaryNumber k="not_read" label="Not read" value={fmtNumber(s.not_read)} />
            <SummaryNumber k="capped" label="Capped" value={fmtNumber(s.capped)} />
            <SummaryNumber k="off_in_aws" label="Off in AWS" value={fmtNumber(s.off_in_aws)} />
          </dl>
          <button
            type="button"
            className="btn"
            onClick={onReadAgain}
            disabled={!canWrite || reading}
            title={canWrite ? undefined : "Your role can't start a read"}
            data-read-again
          >
            {reading ? "Reading…" : "Read again"}
          </button>
        </div>
      </SummaryCard>
      {view.not_read.length > 0 && (
        <Section id="not-read" title="Not read" icon={<AlertTriangle size={14} aria-hidden="true" className="text-sev-warn" />}>
          {view.not_read.map((r) => (
            <NotReadRow key={rowKey(r)} row={r} names={names} total={total} />
          ))}
        </Section>
      )}
      {view.capped.length > 0 && (
        <Section id="capped" title="Capped" icon={<ArrowUpToLine size={14} aria-hidden="true" className="text-sev-warn" />}>
          {view.capped.map((r) => (
            <CappedRow key={rowKey(r)} row={r} names={names} />
          ))}
        </Section>
      )}
      {view.off_in_aws.length > 0 && (
        <Section
          id="off-in-aws"
          title="Off in AWS"
          footer={
            <Link to="/aws/settings#collection-cost" className="text-xs text-link hover:underline">
              What each costs, in Settings ›
            </Link>
          }
        >
          {view.off_in_aws.map((r) => (
            <OffRow key={rowKey(r)} row={r} names={names} total={total} />
          ))}
        </Section>
      )}
      {view.not_read.length === 0 && view.capped.length === 0 && view.off_in_aws.length === 0 && (
        <p className={clsx("card px-4 py-3 text-sm text-ink-200")} data-complete>
          Every source was read
        </p>
      )}
      <NotGaps view={view} />
    </>
  );
}

export function AwsCoveragePage() {
  const [params, setParams] = useSearchParams();
  const account = params.get("account") ?? "";
  const region = params.get("region") ?? "";
  const known = useAwsAccounts();
  const qc = useQueryClient();
  const toast = useToast();
  const retry = useRetryUnlessForbidden();
  const coverage = useQuery({
    queryKey: ["aws-coverage", account, region],
    queryFn: () => api.awsCoverage({ account: account || undefined, region: region || undefined }),
    retry,
    refetchInterval: 60_000,
    refetchIntervalInBackground: false,
  });
  const readAgain = useMutation({
    mutationFn: async () => {
      const ids = account ? [account] : known.accounts.map((a) => a.account_id);
      await Promise.all(ids.map((id) => api.recheckAwsAccount(id)));
    },
    onSuccess: () => {
      toast.push({ tone: "ok", title: "Reading again" });
      void qc.invalidateQueries({ queryKey: ["aws-coverage"] });
    },
    onError: () => toast.push({ tone: "error", title: "Couldn't start a read" }),
  });
  const set = (k: string, v: string) => {
    const next = new URLSearchParams(params);
    if (v) next.set(k, v);
    else next.delete(k);
    setParams(next, { replace: true });
  };
  const view = coverage.data;
  let body: React.ReactNode;
  if (coverage.isPending) body = <SkCard lines={6} />;
  else if (coverage.isError) body = <PageError error={coverage.error} onRetry={() => void coverage.refetch()} retrying={coverage.isFetching} what="Coverage" />;
  else if (!view?.enabled) body = <AwsOff />;
  else if (view.summary.accounts_total === 0) body = <NoAccounts />;
  else body = <Body view={view} names={known.name} onReadAgain={() => readAgain.mutate()} canWrite={known.canWrite} reading={readAgain.isPending} />;
  return (
    <AwsPage title="Coverage" crumbs={[{ label: "Overview", to: "/aws/overview" }, { label: "Coverage" }]}>
      <ScopeBar
        accounts={known.accounts.map((a) => ({ id: a.account_id, name: a.display_name || a.account_id }))}
        regions={view?.regions ?? known.regions}
        account={account}
        region={region}
        onAccount={(v) => set("account", v)}
        onRegion={(v) => set("region", v)}
      />
      {body}
    </AwsPage>
  );
}
