import { useMemo, useState } from "react";
import { Link } from "react-router-dom";
import clsx from "clsx";
import { Check, ChevronDown, CircleSlash, Download, Plus, Receipt, Search } from "lucide-react";
import type { FinOpsAccountRow, FinOpsAccountsView } from "@/lib/api";
import { fmtDay, fmtMoney } from "@/lib/finops";
import { Sparkline } from "@/components/Sparkline";
import { Estimated } from "./parts";

// The Accounts tab of the organization view: every linked account with its
// status, group, cost, change, top driver, top team and the last 30 days,
// grouped with subtotals. Filters, search and sort by change.

type Filter = "cost_only" | "new" | "closed";
const ROWS_PER_GROUP = 3;

function StatusCell({ a, costSource }: { a: FinOpsAccountRow; costSource: boolean }) {
  if (costSource) {
    return (
      <span className="pill">
        <Receipt size={11} aria-hidden="true" />
        Cost source
      </span>
    );
  }
  const connect = a.kind === "cost_only" && a.status !== "closed" && (
    <Link to="/aws/settings" className="text-xs text-link hover:underline">
      Connect ›
    </Link>
  );
  if (a.status === "closed") {
    return (
      <span className="inline-flex items-center gap-2 text-xs">
        <span className="pill text-ink-300">
          <CircleSlash size={11} aria-hidden="true" />
          Closed
        </span>
        {a.closed_on && <span className="text-2xs text-ink-300">billed until {fmtDay(a.closed_on)}</span>}
      </span>
    );
  }
  if (a.status === "not_readable") {
    return (
      <span className="inline-flex items-center gap-2 text-xs">
        <span className="pill pill-warn">
          <CircleSlash size={11} aria-hidden="true" />
          Cost not readable
        </span>
        {a.error_code && <code className="font-mono text-2xs text-ink-300">{a.error_code}</code>}
      </span>
    );
  }
  if (a.status === "new") {
    return (
      <span className="inline-flex items-center gap-2 text-xs">
        <span className="pill">
          <Plus size={11} aria-hidden="true" />
          New
        </span>
        {connect}
      </span>
    );
  }
  if (a.kind === "connected") {
    return (
      <span className="pill pill-accent">
        <Check size={11} aria-hidden="true" />
        Connected
      </span>
    );
  }
  return (
    <span className="inline-flex items-center gap-2 text-xs">
      <span className="pill border-dashed">Cost only</span>
      {connect}
    </span>
  );
}

function ChangeText({ micros, currency }: { micros: number | null; currency?: string }) {
  if (micros === null) return null;
  return (
    <span className={clsx("font-mono tabular-nums", micros < 0 ? "text-sev-ok" : micros > 0 ? "text-sev-high" : "text-ink-300")}>
      {fmtMoney(micros, { signed: true, currency })}
    </span>
  );
}

export function AccountsTab({
  view,
  periodShort,
  onExportCsv,
}: {
  view: FinOpsAccountsView;
  periodShort: string;
  onExportCsv: () => void;
}) {
  const currency = view.currency;
  const [query, setQuery] = useState("");
  const [filters, setFilters] = useState<Set<Filter>>(new Set());
  const [sort, setSort] = useState<"change" | "cost">("change");
  const [expanded, setExpanded] = useState<Set<string>>(new Set());
  const accounts = useMemo(() => view.accounts ?? [], [view.accounts]);

  const counts = {
    cost_only: accounts.filter((a) => a.kind === "cost_only").length,
    new: accounts.filter((a) => a.status === "new").length,
    closed: accounts.filter((a) => a.status === "closed").length,
    connected: accounts.filter((a) => a.kind === "connected").length,
  };
  const groupChange = (view.groups ?? []).every((g) => g.change_micros !== null)
    ? (view.groups ?? []).reduce((s, g) => s + (g.change_micros ?? 0), 0)
    : null;

  const visible = useMemo(() => {
    const q = query.trim().toLowerCase();
    const list = accounts.filter((a) => {
      if (q && !a.name.toLowerCase().includes(q) && !a.id.includes(q)) return false;
      if (filters.has("cost_only") && a.kind !== "cost_only") return false;
      if (filters.has("new") && a.status !== "new") return false;
      if (filters.has("closed") && a.status !== "closed") return false;
      return true;
    });
    const key = (a: FinOpsAccountRow) => (sort === "change" ? (a.change_micros ?? Number.NEGATIVE_INFINITY) : a.cost_micros);
    return [...list].sort((a, b) => key(b) - key(a));
  }, [accounts, query, filters, sort]);

  const groups = (view.groups ?? []).map((g) => ({ ...g, rows: visible.filter((a) => (a.group || "") === g.name) }));
  const ungrouped = visible.filter((a) => !(view.groups ?? []).some((g) => g.name === a.group));
  if (ungrouped.length) groups.push({ name: "Other", cost_micros: 0, change_micros: null, count: ungrouped.length, rows: ungrouped });

  const toggleFilter = (f: Filter) =>
    setFilters((cur) => {
      const next = new Set(cur);
      if (next.has(f)) next.delete(f);
      else next.add(f);
      return next;
    });

  return (
    <div className="space-y-3">
      <div className="flex flex-wrap items-baseline gap-x-4 gap-y-1">
        <span className="font-mono text-lg font-semibold tabular-nums text-ink-50">{fmtMoney(view.total_micros, { currency })}</span>
        {groupChange !== null && (
          <span className="text-lg font-semibold">
            <ChangeText micros={groupChange} currency={currency} />
          </span>
        )}
        <span className="text-xs text-ink-300">
          <span className="font-semibold text-ink-100">{counts.connected}</span> connected
        </span>
        <span className="text-xs text-ink-300">
          <span className="font-semibold text-ink-100">{counts.cost_only}</span> cost only
        </span>
        <Link to="/aws/settings" className="ml-auto text-xs text-link hover:underline">
          Connect accounts ›
        </Link>
      </div>

      <section className="card" aria-label="Accounts">
        <div className="flex flex-wrap items-center gap-2 border-b border-ink-500/40 px-3 py-2">
          <span className="text-2xs text-ink-300">Show</span>
          {(
            [
              ["cost_only", `Cost only ${counts.cost_only}`],
              ["new", `New this month ${counts.new}`],
              ["closed", `Closed ${counts.closed}`],
            ] as Array<[Filter, string]>
          ).map(([f, label]) => (
            <button
              key={f}
              type="button"
              aria-pressed={filters.has(f)}
              onClick={() => toggleFilter(f)}
              className={clsx(
                "rounded-full border px-2.5 py-0.5 text-xs",
                filters.has(f) ? "border-accent bg-accent-subtle text-ink-50" : "border-ink-500 text-ink-200 hover:text-ink-50",
              )}
            >
              {label}
            </button>
          ))}
          <label className="relative ml-auto">
            <span className="sr-only">Search accounts or IDs</span>
            <Search size={12} aria-hidden="true" className="absolute left-2 top-1/2 -translate-y-1/2 text-ink-400" />
            <input
              type="search"
              value={query}
              onChange={(e) => setQuery(e.target.value)}
              placeholder="Search accounts or IDs"
              className="input h-7 w-52 pl-6 text-xs"
            />
          </label>
          <button
            type="button"
            className="btn"
            onClick={() => setSort((s) => (s === "change" ? "cost" : "change"))}
            aria-label={`Sort by ${sort === "change" ? "cost" : "change"}`}
          >
            Sort: {sort === "change" ? "Change" : "Cost"}
            <ChevronDown size={12} aria-hidden="true" />
          </button>
          <button type="button" className="btn" onClick={onExportCsv}>
            <Download size={12} aria-hidden="true" />
            Export CSV
          </button>
        </div>
        <div className="overflow-x-auto">
          <table className="w-full min-w-[980px] text-xs">
            <caption className="sr-only">Linked accounts by group</caption>
            <thead>
              <tr className="text-left text-2xs text-ink-300">
                <th scope="col" className="px-3 py-2 font-normal">Account</th>
                <th scope="col" className="py-2 pr-3 font-normal">Status</th>
                <th scope="col" className="py-2 pr-3 font-normal">Group</th>
                <th scope="col" className="py-2 pr-3 text-right font-normal">{periodShort}</th>
                <th scope="col" className="py-2 pr-3 text-right font-normal" aria-sort={sort === "change" ? "descending" : undefined}>
                  Change
                </th>
                <th scope="col" className="py-2 pr-3 font-normal">Top driver</th>
                <th scope="col" className="py-2 pr-3 font-normal">Top team</th>
                <th scope="col" className="py-2 pr-3 font-normal">Last 30 days</th>
              </tr>
            </thead>
            {groups.map((g) => {
              const open = expanded.has(g.name);
              const rows = open ? g.rows : g.rows.slice(0, ROWS_PER_GROUP);
              const more = g.rows.length - rows.length;
              return (
                <tbody key={g.name} className="border-t border-ink-500/40">
                  <tr className="bg-surface-sunken/40">
                    <th scope="rowgroup" colSpan={3} className="px-3 py-1.5 text-left font-semibold text-ink-50">
                      {g.name} <span className="ml-1 text-2xs font-normal text-ink-400">{g.count} accounts</span>
                    </th>
                    <td className="py-1.5 pr-3 text-right font-mono font-semibold tabular-nums text-ink-50">
                      {g.cost_micros ? fmtMoney(g.cost_micros, { currency }) : ""}
                    </td>
                    <td className="py-1.5 pr-3 text-right">
                      <ChangeText micros={g.change_micros} currency={currency} />
                    </td>
                    <td colSpan={3} />
                  </tr>
                  {rows.map((a) => (
                    <tr key={a.id} className="border-t border-ink-500/20">
                      <th scope="row" className="px-3 py-1.5 text-left font-normal">
                        <span className="text-ink-100">{a.name}</span>
                        <span className="ml-2 font-mono text-2xs text-ink-400">{a.id}</span>
                      </th>
                      <td className="py-1.5 pr-3">
                        <StatusCell a={a} costSource={view.cost_source?.kind === "management" && view.cost_source.account_id === a.id} />
                      </td>
                      <td className="py-1.5 pr-3 text-ink-200">{a.group}</td>
                      <td className="py-1.5 pr-3 text-right font-mono tabular-nums text-ink-100">
                        {a.status === "not_readable" ? <span className="text-ink-300">Not readable</span> : fmtMoney(a.cost_micros, { currency })}
                      </td>
                      <td className="py-1.5 pr-3 text-right">
                        <ChangeText micros={a.change_micros} currency={currency} />
                      </td>
                      <td className="py-1.5 pr-3 text-ink-200">{a.top_driver?.label}</td>
                      <td className={clsx("py-1.5 pr-3", a.top_team ? "text-ink-200" : "text-ink-400")}>
                        {a.top_team ? (
                          <span className="inline-flex items-center">
                            {a.top_team_estimated && <Estimated />}
                            {a.top_team}
                          </span>
                        ) : a.status === "not_readable" ? (
                          ""
                        ) : (
                          "Unallocated"
                        )}
                      </td>
                      <td className="py-1.5 pr-3 text-ink-300">
                        <Sparkline data={a.sparkline_micros ?? []} width={96} height={18} aria-label={`${a.name}, last 30 days`} />
                      </td>
                    </tr>
                  ))}
                  {(more > 0 || open) && g.rows.length > ROWS_PER_GROUP && (
                    <tr>
                      <td colSpan={8} className="px-3 py-1">
                        <button
                          type="button"
                          className="text-xs text-ink-200 hover:text-ink-50"
                          aria-expanded={open}
                          onClick={() =>
                            setExpanded((cur) => {
                              const next = new Set(cur);
                              if (next.has(g.name)) next.delete(g.name);
                              else next.add(g.name);
                              return next;
                            })
                          }
                        >
                          {open ? "Show less" : `${more} more ›`}
                        </button>
                      </td>
                    </tr>
                  )}
                </tbody>
              );
            })}
          </table>
        </div>
      </section>
    </div>
  );
}
