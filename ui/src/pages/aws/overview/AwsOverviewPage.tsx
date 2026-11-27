import { Link, useSearchParams } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import clsx from "clsx";
import { format } from "date-fns";
import { ChevronRight, User } from "lucide-react";
import { SkCard } from "@/components/Skeleton";
import { useRetryUnlessForbidden } from "@/lib/access";
import { api } from "@/lib/api";
import {
  countWords,
  fmtNumber,
  readState,
  notReadBadge,
  ROLE_LABEL,
  SEVERITY_LABEL,
  sharePercent,
  type IssueSeverity,
  type OverviewAccount,
  type OverviewIssue,
  type OverviewPeopleChange,
  type OverviewView,
} from "@/lib/awsEstate";
import { AwsOff, AwsPage, NoAccounts, PageError, ScopeBar } from "../shared/AwsPage";
import { ResourceSearch } from "../shared/ResourceSearch";
import { useAwsAccounts } from "../shared/useAwsAccounts";
import { CoverageWarningLine, SummaryCard, SummaryNumber } from "../shared/SummaryCard";

// AWS / Overview: summary data only. One summary card holds the numbers,
// each linking to the page that owns it; then the issues that need
// attention, the accounts, and the changes by people that touched exposure
// or identity. A count the server did not read is a dash, never a zero.

const SEVERITY_PILL: Record<IssueSeverity, string> = {
  critical: "border-sev-critical-solid bg-sev-critical-solid text-white",
  high: "border-sev-high/60 bg-sev-high/10 text-sev-high",
  medium: "border-ink-500 bg-ink-600/60 text-ink-200",
  low: "border-ink-500 bg-transparent text-ink-300",
};

function SeverityPill({ severity }: { severity: IssueSeverity }) {
  return (
    <span
      className={clsx("inline-flex w-16 shrink-0 justify-center rounded-full border px-2 py-0.5 text-2xs font-medium", SEVERITY_PILL[severity])}
      data-severity={severity}
    >
      {SEVERITY_LABEL[severity]}
    </span>
  );
}

function clock(t: string | undefined): string | null {
  if (!t) return null;
  const d = new Date(t);
  return Number.isNaN(d.getTime()) ? null : format(d, "HH:mm");
}

/** Not read: grey, dashed, with the reason. Never a zero, never green. */
function NotReadNote({ text }: { text: string }) {
  return (
    <p className="m-4 rounded border border-dashed border-ink-500 px-3 py-2 text-xs text-ink-300" data-not-read>
      {text}
    </p>
  );
}

function Summary({ view }: { view: OverviewView }) {
  const s = view.summary;
  return (
    <SummaryCard>
      <div className="flex flex-wrap items-start justify-between gap-3 px-4 pt-3">
        <dl className="flex flex-wrap items-end gap-x-10 gap-y-3" data-numbers aria-live="polite">
          <SummaryNumber k="accounts" label="Accounts" value={`${s.accounts_read}/${s.accounts_total}`} to="/aws/coverage" />
          <SummaryNumber k="act_now" label="Act now" words={countWords(s.act_now, readState(s.act_now, s.act_now_capped))} to="/aws/issues?tier=act_now" />
          <SummaryNumber
            k="worth_a_look"
            label="Worth a look"
            words={countWords(s.worth_a_look, readState(s.worth_a_look, s.worth_a_look_capped))}
            to="/aws/issues?tier=worth_a_look"
          />
          <SummaryNumber k="resources" label="Resources" words={countWords(s.resources, readState(s.resources, s.resources_capped))} to="/aws/topology" />
          {/* No share of nothing: with 0 resources read the share is a plain dash, not "not read". */}
          <SummaryNumber
            k="grouped"
            label="Grouped"
            words={
              s.grouped_share != null
                ? s.grouped_share_capped
                  ? { value: sharePercent(s.grouped_share), suffix: "not all read", state: "capped" }
                  : { value: sharePercent(s.grouped_share), state: "read" }
                : s.resources === 0
                  ? { value: "—", state: "read" }
                  : { value: "—", state: "not_read" }
            }
            to={s.grouped_share != null ? "/aws/topology?level=groups" : undefined}
          />
          <SummaryNumber
            k="people_changes"
            label="Changes by people, 24 h"
            words={countWords(s.people_changes_24h, readState(s.people_changes_24h, s.people_changes_capped))}
            to="/aws/changes"
          />
        </dl>
      </div>
      <CoverageWarningLine warning={view.warning} />
    </SummaryCard>
  );
}

function IssueRow({ issue, accountName }: { issue: OverviewIssue; accountName: (id: string) => string }) {
  const since = issue.since_known ? clock(issue.since) : null;
  const where = [issue.account_name || accountName(issue.account), issue.region, issue.where, since && `since ${since}`].filter(Boolean).join(" · ");
  return (
    <li className="border-b border-ink-500/40 last:border-b-0" data-issue={issue.id}>
      <Link to={`/aws/issues?id=${encodeURIComponent(issue.id)}`} className="flex min-w-0 items-start gap-3 px-4 py-2.5 hover:bg-ink-600/30">
        <SeverityPill severity={issue.severity} />
        <span className="min-w-0">
          <span className="block truncate text-sm text-ink-50" title={issue.title}>
            {issue.title}
          </span>
          <span className="block truncate text-xs text-ink-300">{where}</span>
        </span>
      </Link>
    </li>
  );
}

function CardHead({ id, title, link }: { id: string; title: string; link: { to: string; label: string } }) {
  return (
    <div className="card-header">
      <h2 id={id} className="card-title">
        {title}
      </h2>
      <Link to={link.to} className="text-xs text-link hover:underline">
        {link.label} ›
      </Link>
    </div>
  );
}

function NeedsAttention({ view, accountName }: { view: OverviewView; accountName: (id: string) => string }) {
  const list = view.needs_attention.slice(0, 5);
  // "Nothing needs attention" needs both tiers read as zero; a tier not read is not assessed.
  const notAssessed = view.summary.act_now == null || view.summary.worth_a_look == null;
  return (
    <section className="card min-w-0" aria-labelledby="needs-attention" data-section="needs-attention">
      <CardHead id="needs-attention" title="Needs attention" link={{ to: "/aws/issues", label: "All issues" }} />
      {list.length > 0 ? (
        <ul>
          {list.map((i) => (
            <IssueRow key={i.id} issue={i} accountName={accountName} />
          ))}
        </ul>
      ) : notAssessed ? (
        <NotReadNote text="Not assessed: no account was read" />
      ) : (
        <p className="px-4 py-4 text-sm text-ink-200" data-clear>
          Nothing needs attention
        </p>
      )}
    </section>
  );
}

/** A count that may be a lower bound: "more than 75" over a grey "not all read", on two lines so it never spills. */
function LowerBound({ n, capped }: { n: number | null; capped: boolean }) {
  const w = countWords(n, readState(n, capped));
  if (w.state !== "capped" || !w.prefix) return <>{w.value}</>;
  return (
    <>
      <span className="block whitespace-nowrap">
        {w.prefix} {w.value}
      </span>
      <span className="block whitespace-nowrap font-sans text-2xs text-ink-300">{w.suffix}</span>
    </>
  );
}

/** "Hub, transit gateway"; the detail alone when the role is not known. */
function roleText(a: OverviewAccount): string {
  return [ROLE_LABEL[a.role], a.role_detail].filter(Boolean).join(", ");
}

function AccountRow({ a }: { a: OverviewAccount }) {
  const notRead = a.read === "not_read" || a.resources == null;
  const reason = a.not_read_reason;
  const role = roleText(a);
  return (
    <tr className={clsx("border-b border-ink-500/40 align-top", notRead && "text-ink-300")} data-account={a.id} data-state={notRead ? "not_read" : a.read}>
      <td className="py-2.5 pl-4 pr-2">
        <Link to={`/aws/topology?level=groups&account=${a.id}`} className="block break-words font-medium text-ink-50 hover:underline" title={`${a.name} (${a.id})`}>
          {a.name || a.id}
        </Link>
        {/* On a phone the role sits under the name; wider screens give it a column. */}
        {role && <span className="mt-0.5 block text-xs text-ink-300 sm:hidden">{role}</span>}
        {!notRead && reason && (
          <span className="mt-0.5 block text-xs text-ink-300" data-partial-reason>
            {notReadBadge(reason)}
          </span>
        )}
      </td>
      <td className="hidden px-2 py-2.5 text-xs text-ink-300 sm:table-cell" data-role>
        {role}
      </td>
      {notRead ? (
        <td colSpan={2} className="py-2 pr-4 text-right">
          <span className="inline-block max-w-full rounded border border-dashed border-ink-500 px-2 py-0.5 text-left text-2xs text-ink-300" data-not-read title={reason}>
            {notReadBadge(reason)}
          </span>
        </td>
      ) : (
        <>
          <td className="px-2 py-2.5 text-right font-mono text-xs tabular-nums text-ink-100" data-resources>
            <LowerBound n={a.resources} capped={a.resources_capped || a.read === "partial"} />
          </td>
          <td className="py-2.5 pl-2 pr-4 text-right font-mono text-xs tabular-nums text-ink-100">
            {a.open_issues == null ? (
              <span className="text-ink-300" title="Not read">
                —
              </span>
            ) : (
              <Link to={`/aws/issues?account=${a.id}`} className="hover:underline">
                {fmtNumber(a.open_issues)}
              </Link>
            )}
          </td>
        </>
      )}
    </tr>
  );
}

function Accounts({ view }: { view: OverviewView }) {
  const other = view.other_accounts;
  return (
    <section className="card min-w-0" aria-labelledby="overview-accounts" data-section="accounts">
      <CardHead id="overview-accounts" title="Accounts" link={{ to: "/aws/topology", label: "Topology" }} />
      {/* On a phone the card grows with its rows; wider screens cap it and scroll inside. */}
      <div className="sm:max-h-[26rem] sm:overflow-auto" data-accounts-scroll>
        <table className="w-full table-fixed text-sm">
          <colgroup>
            <col className="w-[30%]" />
            <col className="w-0 sm:w-auto" />
            <col className="w-[6.5rem]" />
            <col className="w-[4rem]" />
          </colgroup>
          <thead className="sticky top-0 bg-surface">
            <tr className="text-2xs uppercase tracking-wider text-ink-300">
              <th className="py-2 pl-4 pr-2 text-left font-medium">Account</th>
              <th className="hidden px-2 py-2 text-left font-medium sm:table-cell">
                <span className="sr-only">Role</span>
              </th>
              <th className="px-2 py-2 text-right font-medium">Resources</th>
              <th className="py-2 pl-2 pr-4 text-right font-medium">Issues</th>
            </tr>
          </thead>
          <tbody>
            {view.accounts.map((a) => (
              <AccountRow key={a.id} a={a} />
            ))}
          </tbody>
        </table>
      </div>
      {other.count > 0 && (
        <Link to="/aws/topology" className="flex items-center justify-between px-4 py-3 text-xs text-ink-300 hover:bg-ink-600/30" data-other-accounts>
          <span>
            Other accounts: {fmtNumber(other.count)}
            {other.unknown > 0 ? ` (${fmtNumber(other.unknown)} unknown)` : ""}
          </span>
          <ChevronRight size={14} aria-hidden="true" />
        </Link>
      )}
    </section>
  );
}

/** The summary with the resources it names in bold. */
function ChangeSummary({ c }: { c: OverviewPeopleChange }) {
  const names = (c.resources ?? []).filter((r) => r && c.summary.includes(r));
  if (names.length === 0) return <>{c.summary}</>;
  const pattern = new RegExp(`(${names.map((n) => n.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")).join("|")})`);
  return (
    <>
      {c.summary.split(pattern).map((part, i) =>
        names.includes(part) ? (
          <strong key={i} className="font-semibold text-ink-50">
            {part}
          </strong>
        ) : (
          part
        ),
      )}
    </>
  );
}

function PeopleChanges({ view, accountName }: { view: OverviewView; accountName: (id: string) => string }) {
  const list = view.people_changes.slice(0, 3);
  const notRead = view.summary.people_changes_24h == null;
  return (
    <section className="card min-w-0" aria-labelledby="people-changes" data-section="people-changes">
      <CardHead id="people-changes" title="Changes by people" link={{ to: "/aws/changes", label: "All changes" }} />
      {list.length > 0 ? (
        <ul>
          {list.map((c) => (
            <li key={c.id} className="flex min-w-0 items-start gap-3 border-b border-ink-500/40 px-4 py-2.5 last:border-b-0" data-change={c.id}>
              <span className="flex shrink-0 items-center gap-1.5 font-mono text-xs tabular-nums text-ink-300">
                {clock(c.time)}
                <User size={12} aria-label="By a person" />
              </span>
              <span className="min-w-0">
                <span className="block break-words text-sm text-ink-100">
                  <ChangeSummary c={c} />
                </span>
                <span className="block truncate text-xs text-ink-300">
                  {[c.account_name || accountName(c.account), c.concern === "exposure" ? "Exposure" : "Identity", c.actor].join(" · ")}
                </span>
              </span>
            </li>
          ))}
        </ul>
      ) : notRead ? (
        <NotReadNote text="Not read: the change history could not be read" />
      ) : (
        <p className="px-4 py-4 text-sm text-ink-200" data-clear>
          No changes by people to exposure or identity
        </p>
      )}
    </section>
  );
}

export function AwsOverviewPage() {
  const [params, setParams] = useSearchParams();
  const account = params.get("account") ?? "";
  const region = params.get("region") ?? "";
  const set = (k: string, v: string) => {
    const next = new URLSearchParams(params);
    if (v) next.set(k, v);
    else next.delete(k);
    setParams(next, { replace: true });
  };
  const known = useAwsAccounts();
  const retry = useRetryUnlessForbidden();
  const overview = useQuery({
    queryKey: ["aws-overview", account, region],
    queryFn: () => api.awsOverview({ account: account || undefined, region: region || undefined }),
    retry,
    refetchInterval: 60_000,
    refetchIntervalInBackground: false,
  });
  const view = overview.data;
  const nameById = new Map((view?.accounts ?? []).map((a) => [a.id, a.name]));
  const accountName = (id: string) => nameById.get(id) || known.name(id);

  let body: React.ReactNode;
  if (overview.isPending) {
    body = (
      <div className="space-y-4" data-loading>
        <SkCard lines={2} />
        <SkCard lines={5} />
      </div>
    );
  } else if (overview.isError) {
    body = <PageError error={overview.error} onRetry={() => void overview.refetch()} retrying={overview.isFetching} what="Overview" />;
  } else if (!view?.enabled) {
    body = <AwsOff />;
  } else if (view.summary.accounts_total === 0) {
    body = <NoAccounts />;
  } else {
    body = (
      <>
        <Summary view={view} />
        <div className="grid min-w-0 gap-4 lg:grid-cols-[minmax(0,1.4fr)_minmax(0,1fr)]">
          <NeedsAttention view={view} accountName={accountName} />
          <Accounts view={view} />
        </div>
        <PeopleChanges view={view} accountName={accountName} />
      </>
    );
  }

  return (
    <AwsPage title="Overview" crumbs={[{ label: "Overview" }]}>
      <ScopeBar
        accounts={known.accounts.map((a) => ({ id: a.account_id, name: a.display_name || a.account_id }))}
        regions={view?.regions ?? known.regions}
        account={account}
        region={region}
        onAccount={(v) => set("account", v)}
        onRegion={(v) => set("region", v)}
      >
        <ResourceSearch account={account} region={region} accountName={accountName} />
      </ScopeBar>
      {body}
    </AwsPage>
  );
}
