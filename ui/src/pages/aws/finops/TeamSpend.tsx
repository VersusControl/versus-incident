import { useId } from "react";
import { Link } from "react-router-dom";
import clsx from "clsx";
import { Lock } from "lucide-react";
import type { FinOpsBreakdownRow, FinOpsMissingAccount } from "@/lib/api";
import { GROUP_LABELS, fmtMoney, fmtShare, missingAccountText, type SpendByLayout } from "@/lib/finops";
import { Card, Estimated } from "./parts";

// "Spend by account | region | team": amount and share. The switch appears
// only when two or more options split the cost; with exactly one the card is
// titled with it; with none the page removes the card.

function Switch({
  layout,
  selected,
  onSelect,
}: {
  layout: Extract<SpendByLayout, { kind: "switch" }>;
  selected: string;
  onSelect: (groupBy: string) => void;
}) {
  const hint = useId();
  return (
    <div role="group" aria-label="Spend by" className="inline-flex rounded-control border border-ink-500 bg-surface-raised p-0.5">
      {layout.choices.map((c) => {
        const active = c.groupBy === selected;
        const disabled = Boolean(c.disabledReason);
        return (
          <button
            key={c.groupBy}
            type="button"
            aria-pressed={active}
            aria-disabled={disabled || undefined}
            aria-describedby={disabled ? `${hint}-${c.groupBy}` : undefined}
            title={c.disabledReason}
            onClick={() => {
              if (!disabled) onSelect(c.groupBy);
            }}
            className={clsx(
              "inline-flex min-h-7 items-center gap-1 rounded px-2.5 text-xs font-medium focus:outline-none focus-visible:ring-2 focus-visible:ring-accent",
              active ? "bg-accent-subtle text-ink-50" : "text-ink-300",
              disabled ? "cursor-not-allowed opacity-60" : !active && "hover:text-ink-100",
            )}
          >
            {disabled && <Lock size={11} aria-hidden="true" />}
            {c.label}
          </button>
        );
      })}
      {layout.choices
        .filter((c) => c.disabledReason)
        .map((c) => (
          <span key={c.groupBy} id={`${hint}-${c.groupBy}`} className="sr-only">
            {c.disabledReason}
          </span>
        ))}
    </div>
  );
}

export function SpendByCard({
  layout,
  selected,
  onSelect,
  rows,
  currency,
  loading,
  missingAccounts,
  accountsTab,
}: {
  layout: Exclude<SpendByLayout, { kind: "none" }>;
  selected: string;
  onSelect: (groupBy: string) => void;
  rows: FinOpsBreakdownRow[] | null;
  currency?: string;
  loading?: boolean;
  missingAccounts?: FinOpsMissingAccount[];
  /** Link to the Accounts tab for "N other accounts ›". */
  accountsTab?: string;
}) {
  const title =
    layout.kind === "single" ? `Spend by ${(GROUP_LABELS[layout.groupBy] ?? layout.groupBy).toLowerCase()}` : "Spend by";
  const max = Math.max(1, ...(rows ?? []).map((r) => r.amount_micros));
  return (
    <Card
      title={
        <span className="flex flex-wrap items-center gap-3">
          {title}
          {layout.kind === "switch" && <Switch layout={layout} selected={selected} onSelect={onSelect} />}
        </span>
      }
    >
      {loading && !rows ? (
        <div className="sk h-24 rounded" />
      ) : (
        <ul className="space-y-3">
          {(rows ?? []).map((r) => {
            const other = r.key === "other";
            const unalloc = selected === "team" && r.key === "Unallocated";
            const width = r.amount_micros > 0 ? Math.max((r.amount_micros / max) * 100, 1) : 0;
            return (
              <li key={r.key}>
                <div className="flex items-baseline gap-2 text-xs">
                  {other && selected === "account" && accountsTab ? (
                    <Link to={accountsTab} className="min-w-0 flex-1 truncate text-link hover:underline">
                      {r.count ?? ""} other accounts ›
                    </Link>
                  ) : (
                    <span className={clsx("min-w-0 flex-1 truncate", other || unalloc ? "text-ink-300" : "text-ink-100")}>
                      {r.label}
                      {unalloc && (
                        <Link to="/aws/settings" className="ml-2 text-link hover:underline">
                          Find untagged ›
                        </Link>
                      )}
                    </span>
                  )}
                  <span className="inline-flex items-center font-mono tabular-nums text-ink-100">
                    {r.estimated && <Estimated />}
                    {fmtMoney(r.amount_micros, { currency })}
                  </span>
                  <span className="w-9 text-right font-mono text-2xs tabular-nums text-ink-300">{fmtShare(r.share_bp)}</span>
                </div>
                <svg aria-hidden="true" viewBox="0 0 100 4" preserveAspectRatio="none" className="mt-1 h-1 w-full">
                  <rect width={100} height={4} rx={1} className="fill-ink-600" />
                  {width > 0 && <rect width={width} height={4} rx={1} className="fill-ink-400" />}
                </svg>
              </li>
            );
          })}
          {selected === "account" &&
            (missingAccounts ?? []).map((m) => (
              <li key={m.id} className="flex items-baseline gap-2 rounded border border-dashed border-ink-500 px-2 py-1 text-xs">
                <span className="min-w-0 flex-1 truncate text-ink-200">{m.name || m.id}</span>
                <span className="text-ink-300">{missingAccountText(m).split(": ")[1]}</span>
              </li>
            ))}
        </ul>
      )}
    </Card>
  );
}
