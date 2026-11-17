import { Link } from "react-router-dom";
import { AlertTriangle, Check, CircleSlash, Eye } from "lucide-react";
import type { FinOpsMissingAccount, FinOpsSync } from "@/lib/api";
import { fmtDay, fmtSince, staleReasonText } from "@/lib/finops";
import { InfoTip } from "./parts";
import { copyText } from "@/lib/clipboard";
import { useToast } from "@/components/toastContext";

// The page when there is no cost to show: Cost Explorer reads are off (the
// default), or Cost Explorer is not available for the only account. No
// zeros, no empty charts.

const CALLS: Array<[string, string]> = [
  ["ce:GetCostAndUsage", "by account, service"],
  ["ce:GetCostAndUsage", "by service, usage type"],
  ["ce:GetCostAndUsage", "by team tag, service"],
  ["ce:GetCostAndUsage", "by record type, account"],
  ["ce:GetCostForecast", "forecast to month end"],
  ["ce:GetSavingsPlansCoverage", "discount coverage"],
];

const COST_POLICY = `{
  "Effect": "Allow",
  "Action": [
    "ce:GetCostAndUsage",
    "ce:GetCostAndUsageWithResources",
    "ce:GetCostForecast",
    "ce:GetSavingsPlansCoverage",
    "ce:ListCostAllocationTags",
    "ce:GetRightsizingRecommendation",
    "cost-optimization-hub:ListRecommendations",
    "compute-optimizer:Get*Recommendations",
    "budgets:ViewBudget"
  ],
  "Resource": "*"
}`;

function Preview() {
  return (
    <div aria-hidden="true" className="mt-4 rounded-card border border-dashed border-ink-500 p-3 opacity-60">
      <span className="pill border-dashed">
        <Eye size={11} />
        Preview
      </span>
      <div className="mt-3 grid grid-cols-3 gap-2">
        {["Cost this month", "Change vs last month", "Forecast"].map((t) => (
          <div key={t} className="rounded border border-ink-500/60 p-2">
            <div className="text-2xs text-ink-400">{t}</div>
            <div className="mt-2 h-3 w-2/3 rounded bg-ink-600" />
          </div>
        ))}
      </div>
      <div className="mt-2 rounded border border-ink-500/60 p-2">
        <div className="text-2xs text-ink-400">What moved the cost</div>
        {[70, 52, 40, 30, 22].map((w) => (
          <div key={w} className="mt-2 flex gap-2">
            <div className="h-2 w-16 rounded bg-ink-600" />
            <div className="h-2 rounded bg-ink-600" style={{ width: `${w}%` }} />
          </div>
        ))}
      </div>
    </div>
  );
}

// The cause of each closed reason and, when there is one, the fix on the
// same line.
const OFF_REASONS: Record<string, { text: string; action?: string }> = {
  no_complete_day: { text: "Waiting for the first complete day of cost data" },
  no_connected_accounts: { text: "No account ready to read cost yet", action: "AWS settings ›" },
  cost_source_not_connected: { text: "Cost source account not connected", action: "Connect ›" },
};

export function FinOpsOff({ reason, canAdmin, sync }: { reason?: string; canAdmin: boolean; sync?: FinOpsSync }) {
  const toast = useToast();
  // Cost was read before and no account can read it now: never "yet", and
  // the cause sits behind the ⓘ.
  if (reason === "no_connected_accounts" && sync?.last_complete_day) {
    return (
      <section className="card p-5" aria-label="Cost data">
        <p className="flex flex-wrap items-center gap-2 text-sm text-ink-100">
          <AlertTriangle size={16} aria-hidden="true" className="text-sev-warn" />
          No account can read cost
          <InfoTip label="About cost access">
            {staleReasonText(sync.stale_reason)}. Cost is stored to {fmtDay(sync.last_complete_day)}
            {sync.stale_detail?.since ? `; stale since ${fmtSince(sync.stale_detail.since)}` : ""}.
          </InfoTip>
          <Link to="/aws/settings" className="text-link hover:underline">
            AWS settings ›
          </Link>
        </p>
      </section>
    );
  }
  if (reason && reason !== "cost_data_off") {
    const words = OFF_REASONS[reason];
    const action = words ? words.action : "AWS settings ›";
    return (
      <section className="card p-5" aria-label="Cost data">
        <p className="flex flex-wrap items-center gap-2 text-sm text-ink-100">
          <CircleSlash size={16} aria-hidden="true" className="text-ink-300" />
          {words?.text ?? "Cost data unavailable"}
          {action && (
            <Link to="/aws/settings" className="text-link hover:underline">
              {action}
            </Link>
          )}
        </p>
      </section>
    );
  }
  return (
    <div className="grid gap-4 lg:grid-cols-2">
      <section className="card min-w-0 p-4" aria-labelledby="finops-answers">
        <h2 id="finops-answers" className="text-2xs font-semibold uppercase tracking-wider text-ink-300">
          What FinOps answers
        </h2>
        <ul className="mt-2 space-y-1.5 text-xs text-ink-100">
          {["Cost this month vs last month", "What moved the cost", "Where you can save"].map((t) => (
            <li key={t} className="flex items-center gap-2">
              <Check size={13} aria-hidden="true" className="text-ink-300" />
              {t}
            </li>
          ))}
        </ul>
        <Preview />
      </section>
      <section className="card min-w-0 p-4" aria-labelledby="finops-reads">
        <div className="flex items-center justify-between">
          <h2 id="finops-reads" className="text-2xs font-semibold uppercase tracking-wider text-ink-300">
            What it reads
          </h2>
          <span className="text-2xs text-ink-300">{CALLS.length} calls, daily</span>
        </div>
        <table className="mt-2 w-full text-xs">
          <caption className="sr-only">Cost Explorer calls</caption>
          <tbody>
            {CALLS.map(([call, what]) => (
              <tr key={what} className="border-b border-ink-500/40 last:border-0">
                <td className="py-1.5 font-mono text-2xs text-ink-100">{call}</td>
                <td className="py-1.5 text-right text-ink-200">{what}</td>
              </tr>
            ))}
          </tbody>
        </table>
        <div className="mt-3 flex items-center justify-between text-xs">
          <span className="text-ink-300">Paid requests</span>
          <span className="font-mono font-semibold tabular-nums text-ink-50">≤ $0.20 a day</span>
        </div>
        <div className="mt-4 flex items-center justify-between">
          <h3 className="text-2xs font-semibold uppercase tracking-wider text-ink-300">IAM policy</h3>
          <button
            type="button"
            className="btn"
            onClick={async () => {
              const ok = await copyText(COST_POLICY);
              toast.push({ tone: ok ? "ok" : "error", title: ok ? "Policy copied" : "Copy failed" });
            }}
          >
            Copy policy
          </button>
        </div>
        <pre className="mt-2 overflow-x-auto rounded-control border border-ink-500 bg-surface-sunken p-3 font-mono text-2xs text-ink-100">
          {COST_POLICY}
        </pre>
        <div className="mt-4">
          {canAdmin ? (
            <Link to="/aws/settings" className="btn btn-primary-strong">
              Turn on
            </Link>
          ) : (
            <span className="text-xs text-ink-300">Ask an admin to turn on Cost Explorer reads</span>
          )}
        </div>
      </section>
    </div>
  );
}

const ORG_REQUEST =
  "Please allow member accounts to use Cost Explorer (Billing → Cost Explorer preferences → Member account access) so Versus can read this account's cost.";

export function CostExplorerUnavailable({
  account,
  costSource,
}: {
  account?: FinOpsMissingAccount;
  /** The cost source named by a disabled header when no account row exists. */
  costSource?: { account_id: string; account_name?: string };
}) {
  const toast = useToast();
  const name = account
    ? `${account.name || account.id}${account.name ? ` (${account.id})` : ""}`
    : costSource
      ? costSource.account_name
        ? `${costSource.account_name} (${costSource.account_id})`
        : `account ${costSource.account_id}`
      : "this account";
  return (
    <section className="card p-4" aria-labelledby="ce-unavailable">
      <div className="flex flex-wrap items-center gap-3">
        <CircleSlash size={18} aria-hidden="true" className="text-ink-300" />
        <h2 id="ce-unavailable" className="min-w-0 flex-1 text-base font-semibold text-ink-50">
          Cost Explorer is not available for {name}
        </h2>
        <code className="rounded border border-ink-500 px-1.5 py-0.5 font-mono text-2xs text-ink-200">{account?.reason || "ce_unavailable"}</code>
      </div>
      <ol className="mt-4 space-y-2">
        <li className="flex items-center gap-3 rounded-control bg-surface-sunken p-3 text-xs text-ink-100">
          <span className="inline-flex h-6 w-6 items-center justify-center rounded-full border border-ink-500 text-2xs">1</span>
          <span className="flex-1">Cost Explorer never opened</span>
          <a className="btn" href="https://console.aws.amazon.com/cost-management/home#/cost-explorer" target="_blank" rel="noreferrer">
            Open console ›
          </a>
        </li>
        <li className="flex items-center gap-3 rounded-control bg-surface-sunken p-3 text-xs text-ink-100">
          <span className="inline-flex h-6 w-6 items-center justify-center rounded-full border border-ink-500 text-2xs">2</span>
          <span className="flex-1">Turned off by the Organization admin</span>
          <button
            type="button"
            className="btn"
            onClick={async () => {
              const ok = await copyText(ORG_REQUEST);
              toast.push({ tone: ok ? "ok" : "error", title: ok ? "Request copied" : "Copy failed" });
            }}
          >
            Copy request
          </button>
        </li>
        <li className="flex items-center gap-3 rounded-control bg-surface-sunken p-3 text-xs text-ink-100">
          <span className="inline-flex h-6 w-6 items-center justify-center rounded-full border border-ink-500 text-2xs">3</span>
          <span className="flex-1">Use the management account instead</span>
          <Link to="/aws/settings" className="btn">
            Connect account ›
          </Link>
        </li>
      </ol>
    </section>
  );
}
