import { useNavigate, Link } from "react-router-dom";
import clsx from "clsx";
import { ChevronRight } from "lucide-react";
import type { FinOpsBridgeView, FinOpsBridgeRow } from "@/lib/api";
import {
  accountColumn,
  bridgeGeometry,
  bridgeX,
  fmtMoney,
  fmtMoneyAxis,
  focusRowId,
  isCostOnly,
  ownerColumn,
  type BridgeBar,
  type BridgeGeometry,
} from "@/lib/finops";
import { Estimated, ResourcesNotNamed, StatusWord, WhyCell } from "./parts";

// "What moved the cost": a horizontal bridge (waterfall), one row per
// driver, from last period's total to this period's. Hand-drawn SVG with
// the theme tokens; the table itself is the accessible alternative. On a
// phone the same rows render as a ranked list with bars.

type BarTone = "total" | "focus" | "expected" | "up" | "down";

function barTone(bar: BridgeBar, focusId: string | null): BarTone {
  if (bar.kind === "base" || bar.kind === "total") return "total";
  if (bar.row && bar.row.id === focusId) return "focus";
  if (bar.amount < 0) return "down";
  if (bar.row?.status === "expected") return "expected";
  return "up";
}

const BAR_FILL: Record<BarTone, string> = {
  total: "fill-ink-500",
  focus: "fill-sev-high-solid",
  expected: "fill-sev-high-solid/40",
  up: "fill-ink-400",
  down: "fill-sev-ok-solid",
};

const AMOUNT_TEXT: Record<BarTone, string> = {
  total: "font-semibold text-ink-50",
  focus: "font-semibold text-sev-high",
  expected: "text-ink-200",
  up: "text-ink-200",
  down: "text-sev-ok",
};

function barLabel(bar: BridgeBar, name: string, currency?: string): string {
  if (bar.kind === "base" || bar.kind === "total") return `${name}, ${fmtMoney(bar.amount, { currency })}`;
  const owner = bar.row?.owner?.name;
  const dir = bar.amount < 0 ? "down" : "up";
  return [name, owner && owner !== "Unallocated" ? owner : null, `${dir} ${fmtMoney(Math.abs(bar.amount), { currency })}`]
    .filter(Boolean)
    .join(", ");
}

function BarSvg({
  g,
  bar,
  tone,
  label,
  focusable,
}: {
  g: BridgeGeometry;
  bar: BridgeBar;
  tone: BarTone;
  label: string;
  focusable?: boolean;
}) {
  const a = bridgeX(g, Math.min(bar.from, bar.to));
  const b = bridgeX(g, Math.max(bar.from, bar.to));
  const width = Math.max((b - a) * 100, 0.6);
  return (
    <svg
      role="img"
      aria-label={label}
      tabIndex={focusable ? 0 : undefined}
      viewBox="0 0 100 10"
      preserveAspectRatio="none"
      className="block h-4 w-full rounded-sm focus:outline-none focus-visible:ring-2 focus-visible:ring-accent"
    >
      <rect x={a * 100} y={1} width={width} height={8} rx={0.6} className={BAR_FILL[tone]} />
    </svg>
  );
}

function Axis({ g, currency }: { g: BridgeGeometry; currency?: string }) {
  return (
    <div aria-hidden="true" className="relative h-4 text-2xs text-ink-400">
      {g.broken && <span className="absolute left-0 top-0 -translate-x-1/2 font-mono">//</span>}
      {g.ticks.filter((t) => !(g.broken && t === g.lo)).map((t) => (
        <span
          key={t}
          className="absolute top-0 -translate-x-1/2 font-mono tabular-nums"
          style={{ left: `${bridgeX(g, t) * 100}%` }}
        >
          {fmtMoneyAxis(t, currency)}
        </span>
      ))}
    </div>
  );
}

function rowName(bar: BridgeBar, baseLabel: string, totalLabel: string): string {
  if (bar.kind === "base") return baseLabel;
  if (bar.kind === "total") return totalLabel;
  return bar.row?.label ?? "";
}

function AccountCell({ row }: { row?: FinOpsBridgeRow }) {
  if (!row?.account_name && !row?.account) return null;
  const costOnly = isCostOnly(row);
  return (
    <span className="inline-flex items-center gap-1.5">
      <span className={costOnly ? "text-ink-300" : "text-ink-100"}>{row.account_name || row.account}</span>
      {costOnly && <span className="pill border-dashed">Cost only</span>}
    </span>
  );
}

function OwnerCell({ row }: { row?: FinOpsBridgeRow }) {
  const o = row?.owner;
  if (!o) return null;
  return (
    <span className={clsx("inline-flex items-center", o.name === "Unallocated" ? "text-ink-300" : "text-ink-100")}>
      {o.estimated && <Estimated />}
      {o.name}
    </span>
  );
}

export function CostBridgeChart({
  bridge,
  depth,
  narrow,
  baseLabel,
  totalLabel,
}: {
  bridge: FinOpsBridgeView;
  depth: string;
  narrow: boolean;
  baseLabel: string;
  totalLabel: string;
}) {
  const navigate = useNavigate();
  const currency = bridge.currency;
  const g = bridgeGeometry(bridge);
  const focusId = focusRowId(bridge.rows);
  const owner = ownerColumn(bridge, bridge.sync?.team_tag.active ?? false);
  const showAccount = accountColumn(bridge);
  const details = depth === "details";
  const detailHref = (row?: FinOpsBridgeRow) =>
    row?.increase_id ? `/aws/finops/increases/${encodeURIComponent(row.increase_id)}` : null;

  if (bridge.steady || (bridge.rows ?? []).length === 0) {
    return (
      <p className="text-sm text-ink-200">
        {bridge.steady_note || `No driver above ${fmtMoney(bridge.min_driver_micros, { currency })}; the cost was steady`}
      </p>
    );
  }

  if (narrow) {
    return (
      <ol aria-label="What moved the cost" className="space-y-2">
        {g.bars.map((bar) => {
          const tone = barTone(bar, focusId);
          const name = rowName(bar, baseLabel, totalLabel);
          const href = detailHref(bar.row);
          const head = (
            <>
              <div className="flex items-baseline justify-between gap-2">
                <span className={clsx("min-w-0 truncate text-xs", tone === "total" ? "font-semibold text-ink-50" : "text-ink-100")}>
                  {name}
                </span>
                <span className={clsx("font-mono text-xs tabular-nums", AMOUNT_TEXT[tone])}>
                  {fmtMoney(bar.amount, { signed: bar.kind !== "base" && bar.kind !== "total", currency })}
                </span>
              </div>
              <BarSvg g={g} bar={bar} tone={tone} label={barLabel(bar, name, currency)} />
            </>
          );
          // The meta line holds its own links and ⓘ, so it sits beside the
          // row link, never inside it.
          const meta = bar.row && bar.kind === "driver" && (
            <div className="mt-0.5 flex flex-wrap items-center gap-x-2 px-1 text-2xs text-ink-300">
              {bar.row.service_label && <span>{bar.row.service_label}</span>}
              {owner.show && bar.row.owner && <OwnerCell row={bar.row} />}
              <WhyCell why={bar.row.why} />
              <StatusWord status={bar.row.status} />
              {bar.row.resources === "too_large" && <ResourcesNotNamed compact />}
            </div>
          );
          return (
            <li key={bar.key}>
              {href ? (
                <Link to={href} className="block rounded-control p-1 hover:bg-ink-600/40 focus:outline-none focus-visible:ring-2 focus-visible:ring-accent">
                  {head}
                </Link>
              ) : (
                <div className="p-1">{head}</div>
              )}
              {meta}
            </li>
          );
        })}
      </ol>
    );
  }

  return (
    <div className="-mx-4 overflow-x-auto px-4">
      <table className="w-full min-w-[760px] border-separate border-spacing-0 whitespace-nowrap text-xs">
        <caption className="sr-only">What moved the cost, one row per driver</caption>
        <thead>
          <tr className="text-left text-2xs text-ink-300">
            <th scope="col" className="py-1.5 pr-3 font-normal">Driver</th>
            <th scope="col" className="w-[28%] py-1.5 font-normal">
              <span className="sr-only">Bar</span>
            </th>
            <th scope="col" className="py-1.5 pl-2 pr-3 text-right font-normal">
              <span className="sr-only">Amount</span>
            </th>
            {showAccount && <th scope="col" className="py-1.5 pr-3 font-normal">Account</th>}
            <th scope="col" className="py-1.5 pr-3 font-normal">Service</th>
            {owner.show && <th scope="col" className="py-1.5 pr-3 font-normal">Owner</th>}
            <th scope="col" className="py-1.5 pr-3 font-normal">Why</th>
            <th scope="col" className="py-1.5 pr-3 font-normal">Status</th>
            <th scope="col" className="w-6 py-1.5">
              <span className="sr-only">Details</span>
            </th>
          </tr>
        </thead>
        <tbody>
          {g.bars.map((bar) => {
            const tone = barTone(bar, focusId);
            const name = rowName(bar, baseLabel, totalLabel);
            const href = detailHref(bar.row);
            const isEdge = bar.kind === "base" || bar.kind === "total";
            const focus = tone === "focus";
            return (
              <tr
                key={bar.key}
                data-row-kind={bar.kind}
                onClick={href ? () => navigate(href) : undefined}
                className={clsx(
                  href && "cursor-pointer hover:bg-ink-600/30",
                  focus && "bg-ink-600/30",
                )}
              >
                <td className={clsx("py-1.5 pr-3", focus && "border-l-2 border-l-sev-high-solid pl-2")}>
                  {href ? (
                    <Link
                      to={href}
                      aria-label={barLabel(bar, name, currency)}
                      onClick={(e) => e.stopPropagation()}
                      className={clsx(
                        "rounded focus:outline-none focus-visible:ring-2 focus-visible:ring-accent",
                        focus ? "font-medium text-ink-50" : "text-ink-100",
                      )}
                    >
                      {name}
                    </Link>
                  ) : (
                    <span className={isEdge ? "font-semibold text-ink-50" : "text-ink-200"}>{name}</span>
                  )}
                </td>
                <td className="py-1.5">
                  <BarSvg g={g} bar={bar} tone={tone} label={barLabel(bar, name, currency)} focusable={!href} />
                </td>
                <td className={clsx("py-1.5 pl-2 pr-3 text-right font-mono tabular-nums", AMOUNT_TEXT[tone])}>
                  {fmtMoney(bar.amount, { signed: !isEdge, currency })}
                </td>
                {showAccount && (
                  <td className="py-1.5 pr-3">{bar.kind === "driver" && <AccountCell row={bar.row} />}</td>
                )}
                <td className="py-1.5 pr-3 text-ink-100">
                  {bar.kind === "driver" && bar.row?.service_label}
                  {details && bar.kind === "driver" && bar.row?.usage_type && (
                    <span className="block font-mono text-2xs text-ink-400">{bar.row.usage_type}</span>
                  )}
                  {bar.kind === "driver" && bar.row?.resources === "too_large" && (
                    <span className="block">
                      <ResourcesNotNamed compact />
                    </span>
                  )}
                </td>
                {owner.show && (
                  <td className="py-1.5 pr-3">{bar.kind === "driver" && <OwnerCell row={bar.row} />}</td>
                )}
                <td className="py-1.5 pr-3">{bar.kind === "driver" && <WhyCell why={bar.row?.why} />}</td>
                <td className="py-1.5 pr-3">{bar.kind === "driver" && <StatusWord status={bar.row?.status} />}</td>
                <td className="py-1.5 text-ink-400">
                  {href && <ChevronRight size={14} aria-hidden="true" />}
                </td>
              </tr>
            );
          })}
          <tr aria-hidden="true">
            <td />
            <td className="pt-1">
              <Axis g={g} currency={currency} />
            </td>
            <td colSpan={7} />
          </tr>
        </tbody>
      </table>
    </div>
  );
}
