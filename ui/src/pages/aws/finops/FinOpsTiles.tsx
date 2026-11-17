import clsx from "clsx";
import type { FinOpsBridgeRow, FinOpsDayAmount, FinOpsSummaryView } from "@/lib/api";
import {
  accountsNote,
  fmtChangePct,
  fmtDay,
  fmtMoney,
  forecastDisplay,
  isNotFinal,
  monthName,
  oneOffTotal,
  partialDaysInPeriod,
  partialMonthLabel,
  periodYear,
} from "@/lib/finops";
import { ChangeBadge, Estimated, InfoTip, Money } from "./parts";

// The three summary cards: period cost, change, forecast. They are the only
// summary on the page (one number, one place); measure, lag and sources sit
// behind one ⓘ per card. At most one secondary line, only when it changes a
// decision.

function Tile({
  label,
  info,
  secondary,
  children,
  dashed,
  quiet,
}: {
  quiet?: boolean;
  label: string;
  info?: React.ReactNode;
  secondary?: React.ReactNode;
  children: React.ReactNode;
  dashed?: boolean;
}) {
  return (
    <section
      aria-label={label}
      className={clsx("card min-w-0 px-4 py-3", dashed && "border-dashed")}
    >
      <div className="flex items-center gap-1 text-xs text-ink-300">
        <h2 className="font-normal">{label}</h2>
        {info && !quiet && <InfoTip label={`About ${label}`}>{info}</InfoTip>}
        {secondary && <span className="ml-auto text-2xs">{secondary}</span>}
      </div>
      <div className="mt-1.5 flex min-w-0 flex-wrap items-baseline gap-x-2 gap-y-1">{children}</div>
    </section>
  );
}

/** Step sparkline of daily cost; partial days are dashed and kept apart. */
export function CostSpark({ days, label }: { days: FinOpsDayAmount[]; label: string }) {
  const complete = days.filter((d) => !d.partial);
  if (complete.length < 2) return null;
  const w = 180;
  const h = 34;
  const values = days.map((d) => d.amount_micros);
  const max = Math.max(...values, 1);
  const min = Math.min(0, ...values);
  const step = w / days.length;
  const y = (v: number) => h - 2 - ((v - min) / (max - min || 1)) * (h - 6);
  let path = "";
  days.forEach((d, i) => {
    if (d.partial) return;
    const x0 = i * step;
    const yy = y(d.amount_micros).toFixed(1);
    path += `${i === 0 ? "M" : "L"}${x0.toFixed(1)},${yy}L${(x0 + step).toFixed(1)},${yy}`;
  });
  let lastComplete = -1;
  days.forEach((d, i) => {
    if (!d.partial) lastComplete = i;
  });
  const area = `${path}L${((lastComplete + 1) * step).toFixed(1)},${h}L0,${h}Z`;
  return (
    <svg
      role="img"
      aria-label={label}
      viewBox={`0 0 ${w} ${h}`}
      className="ml-auto h-8 w-36 min-w-0 text-ink-300"
      preserveAspectRatio="none"
    >
      <path d={area} className="fill-ink-500/40" />
      <path d={path} fill="none" stroke="currentColor" strokeWidth="1.2" />
      {days.map((d, i) =>
        d.partial ? (
          <rect
            key={d.day}
            x={i * step + 0.5}
            y={y(d.amount_micros)}
            width={Math.max(step - 1, 1)}
            height={h - y(d.amount_micros)}
            fill="none"
            stroke="currentColor"
            strokeDasharray="2 2"
          />
        ) : null,
      )}
    </svg>
  );
}

export function FinOpsTiles({
  summary,
  bridgeRows,
  quiet,
}: {
  summary: FinOpsSummaryView;
  bridgeRows?: FinOpsBridgeRow[] | null;
  /** Print: no ⓘ buttons (the report states coverage in its notes). */
  quiet?: boolean;
}) {
  const currency = summary.currency;
  const period = summary.period;
  const compare = summary.compare;
  const sync = summary.sync;
  const accounts = accountsNote(summary);
  const notFinal = isNotFinal(summary);
  const partial = period?.open ? partialDaysInPeriod(summary) : [];
  const shortDays = period && period.range.start > period.calendar.start;

  // Cost card -------------------------------------------------------------
  const costLabel = period ? (period.open ? `${period.short} to date` : `${period.short} cost`) : "Cost";
  let costSecondary: React.ReactNode = null;
  if (accounts) costSecondary = accounts;
  // In print the report header carries "Not final" once.
  else if (notFinal && !quiet) costSecondary = <span className="font-medium text-sev-warn">Not final</span>;
  else if (sync?.stale) costSecondary = (sync.stale_detail?.days ?? []).length > 0 ? "Not loaded" : `Data to ${fmtDay(sync.last_complete_day)}`;
  else if (shortDays) costSecondary = `${period?.complete_days} days`;
  else if (partial.length) costSecondary = "Partial";
  // The active team or region filter rests on proportional split cells.
  const estimatedSplit = (sync?.estimated ?? []).length > 0;
  const partialAmount = (summary.sparkline ?? []).filter((d) => d.partial).reduce((s, d) => s + d.amount_micros, 0);
  const costInfo = (
    <>
      What you actually pay: net amortized cost, after discounts, credits and commitments.
      {sync?.last_complete_day && <> Complete days to {fmtDay(sync.last_complete_day)}; AWS data lags up to 24 h.</>}
      {partial.length > 0 && <> {fmtDay(partial[0])} is partial ({fmtMoney(partialAmount, { currency })} so far) and not in the total.</>}
      {notFinal && <> Not final until AWS closes the bill{sync?.bill_closes_on ? ` on ${fmtDay(sync.bill_closes_on)}` : ""}.</>}
      {(sync?.stale_detail?.days ?? []).length > 0 && <> Not loaded: {(sync?.stale_detail?.days ?? []).map(fmtDay).join(", ")}.</>}
    </>
  );

  // Change card -----------------------------------------------------------
  const change = summary.change;
  const changeLabel = compare ? `Change vs ${compare.short}` : "Change";
  // A month to date compares with the same days of the base month.
  const baseRange = partialMonthLabel(compare?.range, periodYear(period));
  const oneOff = oneOffTotal(bridgeRows);
  let changeBody: React.ReactNode;
  if (!change) {
    const text = shortDays ? "No previous month" : `No full ${compare?.short ?? "previous month"}`;
    changeBody = <span className="text-sm text-ink-300">{text}</span>;
  } else {
    changeBody = (
      <>
        {estimatedSplit && !quiet && <Estimated />}
        <Money
          micros={change.change_micros}
          signed
          currency={currency}
          className={clsx(
            "text-2xl font-semibold",
            change.direction === "down" ? "text-sev-ok" : "text-ink-50",
          )}
        />
        <span className="font-mono text-xs tabular-nums text-ink-300">{fmtChangePct(change.change_bp)}</span>
        <ChangeBadge direction={change.direction} word={change.word} />
      </>
    );
  }

  // Forecast card ---------------------------------------------------------
  const fc = forecastDisplay(summary.forecast);
  const fcMonth = monthName(summary.forecast.month) || "next month";
  const budget = summary.budget;
  let fcBody: React.ReactNode;
  if (fc.kind === "amount") {
    fcBody = (
      <>
        <Money micros={fc.micros} currency={currency} className="text-2xl font-semibold text-ink-50" />
        {fc.estimate && <span className="pill">Estimate</span>}
        {budget?.estimated && !quiet && <Estimated />}
        {budget && budget.forecast_diff_micros > 0 && (
          <span className="text-xs text-sev-critical">
            Over budget by {fmtMoney(budget.forecast_diff_micros, { currency })}
          </span>
        )}
        {budget && budget.forecast_diff_micros <= 0 && (
          <span className="text-xs text-ink-300">Budget {fmtMoney(budget.monthly_micros, { currency })}</span>
        )}
      </>
    );
  } else if (fc.kind === "skipped") {
    fcBody = <span className="text-sm text-ink-300">Skipped today</span>;
  } else {
    fcBody = <span className="text-sm text-ink-300">Not enough history</span>;
  }
  const fcInfo =
    fc.kind === "amount" ? (
      fc.estimate ? (
        <>Estimate: month to date plus the average of the last 7 complete days for each day left.</>
      ) : (
        <>
          Source: AWS forecast
          {summary.forecast.low_micros && summary.forecast.high_micros
            ? ` (likely ${fmtMoney(summary.forecast.low_micros, { currency })} to ${fmtMoney(summary.forecast.high_micros, { currency })})`
            : ""}
          .
          {budget && <> Budget {budget.name}: {fmtMoney(budget.monthly_micros, { currency })} a month ({budget.status_word}).</>}
        </>
      )
    ) : fc.kind === "skipped" ? (
      <>Skipped today to stay under the daily paid-request cap.</>
    ) : (
      <>Needs at least 7 complete days of history.</>
    );

  return (
    <div className="grid grid-cols-1 gap-3 sm:grid-cols-3">
      <Tile quiet={quiet} label={costLabel} info={costInfo} secondary={costSecondary}>
        {estimatedSplit && !quiet && <Estimated />}
        <Money micros={summary.total_micros} currency={currency} className="text-2xl font-semibold text-ink-50" />
        {summary.sparkline && (
          <CostSpark days={summary.sparkline} label={`Daily cost, ${period?.label ?? "this period"}`} />
        )}
      </Tile>
      <Tile
        quiet={quiet}
        label={changeLabel}
        info={
          change && compare ? (
            <>
              {baseRange ? `${baseRange}:` : `${compare.label} total`} {fmtMoney(change.base_micros, { currency })}. About the same means within ±2%.
            </>
          ) : (
            <>The change needs a full previous period to compare with.</>
          )
        }
        secondary={
          accounts ||
          (oneOff > 0 ? `Includes ${fmtMoney(oneOff, { currency })} one-time` : null)
        }
      >
        {changeBody}
      </Tile>
      <Tile
        quiet={quiet}
        label={`Forecast ${fcMonth}`}
        info={fcInfo}
        secondary={accounts || null}
        dashed={fc.kind !== "amount" || fc.estimate}
      >
        {fcBody}
      </Tile>
    </div>
  );
}
