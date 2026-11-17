import clsx from "clsx";
import type { FinOpsBreakdownRow } from "@/lib/api";
import { fmtMoney, fmtShare, isZeroAmount } from "@/lib/finops";
import { Card, ClippedText, Estimated } from "./parts";

// "Spend by service": ranked bars with amount and share, top six plus
// Other. When no "Spend by" option splits the cost the card widens and adds
// what is inside each service (the top usage groups).

/**
 * The groups inside a service, without groups that round to nothing. A
 * service whose only group left is "Other" gets no line: it would say
 * nothing the row does not.
 */
function insideLine(row: FinOpsBreakdownRow, currency?: string): string {
  const groups = (row.inside ?? []).filter((i) => !isZeroAmount(i.amount_micros));
  const onlyOther = groups.length === 1 && (groups[0].key === "" || groups[0].key === "other" || groups[0].label === "Other");
  if (groups.length === 0 || onlyOther) return "";
  return groups.map((i) => `${i.label} ${fmtMoney(i.amount_micros, { currency })}`).join(", ");
}

export function SpendByService({
  rows,
  currency,
  focusService,
  showInside,
}: {
  rows: FinOpsBreakdownRow[];
  currency?: string;
  focusService?: string | null;
  showInside: boolean;
}) {
  const max = Math.max(1, ...rows.map((r) => r.amount_micros));
  return (
    <Card title="Spend by service">
      <ul className="space-y-2.5">
        {rows.map((r) => {
          const isOther = r.key === "other";
          const focus = focusService != null && r.key === focusService;
          const width = r.amount_micros > 0 ? Math.max((r.amount_micros / max) * 100, 1) : 0;
          const inside = showInside ? insideLine(r, currency) : "";
          return (
            <li key={r.key} className="grid grid-cols-[minmax(0,9rem)_minmax(0,1fr)_auto_2.75rem] items-center gap-x-3">
              <span className={clsx("truncate text-xs", isOther ? "text-ink-300" : "text-ink-100")}>{r.label}</span>
              <svg
                role="img"
                aria-label={`${r.label}, ${fmtMoney(r.amount_micros, { currency })}, ${fmtShare(r.share_bp)}`}
                viewBox="0 0 100 6"
                preserveAspectRatio="none"
                className="h-1.5 w-full"
              >
                {width > 0 && (
                  <rect width={width} height={6} rx={1} className={focus ? "fill-sev-high-solid" : "fill-ink-400"} />
                )}
              </svg>
              <span
                className={clsx(
                  "inline-flex items-center justify-end font-mono text-xs tabular-nums",
                  r.amount_micros < 0 ? "text-sev-ok" : "text-ink-100",
                )}
              >
                {r.estimated && <Estimated />}
                {fmtMoney(r.amount_micros, { currency })}
              </span>
              <span className="text-right font-mono text-2xs tabular-nums text-ink-300">{fmtShare(r.share_bp)}</span>
              {inside && (
                <span className="col-span-4 -mt-1 min-w-0">
                  <ClippedText text={inside} className="font-mono text-2xs text-ink-400" />
                </span>
              )}
            </li>
          );
        })}
      </ul>
    </Card>
  );
}
