import clsx from "clsx";
import { addDays, format, startOfDay } from "date-fns";
import {
  ACTOR_LABEL,
  accountName,
  alarmEntries,
  alarmTitle,
  bucketIndexFor,
  cellStep,
  fmtClock,
  fmtClockOrDay,
  fmtCellCount,
  fmtCount,
  fmtSpanOrDay,
  gapDetail,
  gapReasonText,
  gridBuckets,
  gridLayout,
  hasArrivingTail,
  isBandGap,
  missingStretches,
  nothingRead,
  suspectItem as findSuspect,
  type ActorClass,
  type ChangesBucket,
  type ChangesGap,
  type ChangesView,
  type MissingStretch,
} from "@/lib/awsChanges";
import { columnPct, columnStyle } from "./gridColumns";
import { Popover } from "./Popover";

// The activity grid: hours by actor class, plus Errors. Row labels are a
// name and one number. Missing stretches are hatched with their reason on
// hover; the unsettled tail after `settled_until` keeps a dashed outline and
// no label. The alarm lines and the likely-cause cell are the only colour
// besides real errors; the alarms themselves are listed behind one chip.

/** Stripes only: the hatch never sets the text colour, so text on it keeps its own. */
export const HATCH = "bg-[repeating-linear-gradient(135deg,rgb(var(--ink-400)/0.45)_0_1px,transparent_1px_6px)]";

/** A lighter hatch for cells that carry counts while part of the scope is missing. */
export const PARTIAL_HATCH = "bg-[repeating-linear-gradient(135deg,rgb(var(--ink-400)/0.3)_0_1px,transparent_1px_8px)]";

type RowKey = ActorClass | "errors";

const STEP_CLASS = ["", "bg-accent/25", "bg-accent/50", "bg-accent/80"] as const;
const ERROR_STEP_CLASS = ["", "bg-sev-critical/25", "bg-sev-critical/50", "bg-sev-critical/80"] as const;

function rowValue(b: ChangesBucket, key: RowKey): number {
  return key === "errors" ? b.errors : b[key];
}

/** "Prod eu-west-1: CloudTrail throttled the collector 02:10–03:40 (…)": a band's hover text. */
function bandTitle(view: ChangesView, g: ChangesGap): string {
  const detail = gapDetail(g);
  return `${accountName(view, g.account)}${g.region ? ` ${g.region}` : ""}: ${gapReasonText(g.reason)} ${fmtSpanOrDay(g.from, g.to, view.window.until)}${detail ? ` (${detail})` : ""}`;
}

/**
 * "Prod eu-west-1: not checked yet": the hover of a hatched hour no band
 * covers, such as an account the first read has not reached. The span is left
 * out when the stretch covers the whole window.
 */
function stretchTitle(view: ChangesView, g: MissingStretch): string {
  const whole = Date.parse(g.from) <= Date.parse(view.window.since) && Date.parse(g.to) >= Date.parse(view.window.until);
  const span = whole ? "" : ` ${fmtSpanOrDay(g.from, g.to, view.window.until)}`;
  return `${accountName(view, g.account)}${g.region ? ` ${g.region}` : ""}: ${gapReasonText(g.reason)}${span}${g.detail ? ` (${g.detail})` : ""}`;
}

/** The narrowest a hatch band is drawn, in percent of the grid, so a short gap stays visible. */
const MIN_BAND_PCT = 0.5;

/** A band's drawn extent on the grid, in percent of its width. */
type BandSpan = { left: number; right: number };

function Cell({
  bucket,
  index,
  end,
  row,
  suspect,
  shown,
  view,
  bands,
  drawn,
  long,
  columns,
  minWidth,
}: {
  bucket: ChangesBucket;
  /** Where the bucket ends: the next bucket's start, or the window end. */
  end: string;
  row: RowKey;
  suspect: boolean;
  shown: boolean;
  view: ChangesView;
  bands: ChangesGap[];
  /** Where each band is drawn, in percent of the grid, minimum width included. */
  drawn: BandSpan[];
  /** The column's position on the grid. */
  index: number;
  /** The column spans several hours: the label and hover name the whole span. */
  long: boolean;
  /** How many columns the grid has: each cell takes an equal share. */
  columns: number;
  /** The column's minimum width in px, from `gridLayout`. */
  minWidth: number;
}) {
  const span = long ? fmtSpanOrDay(bucket.start, end, view.window.until) : "";
  const label = `${row === "errors" ? "Errors" : ACTOR_LABEL[row]} ${long ? span : fmtClockOrDay(bucket.start, view.window.until)}`;
  const style = columnStyle(columns, minWidth);
  if (bucket.state === "gap") {
    const over = bands.filter((g) => Date.parse(g.from) < Date.parse(end) && Date.parse(g.to) > Date.parse(bucket.start));
    const overlaps = (from: string, to: string) => Date.parse(from) < Date.parse(end) && Date.parse(to) > Date.parse(bucket.start);
    const stretches = over.length > 0 ? [] : missingStretches(view).filter((g) => overlaps(g.from, g.to));
    const why =
      over.length > 0
        ? over.map((g) => bandTitle(view, g)).join("; ")
        : stretches.length > 0
          ? stretches.map((g) => stretchTitle(view, g)).join("; ")
          : long
            ? "Not known for this span"
            : "Not known for this hour";
    return (
      <div
        role="cell"
        data-state="gap"
        aria-label={`${label}: unknown`}
        title={long ? `${span}: ${why}` : why}
        style={style}
        className={clsx("box-border h-7 border-r border-ink-500/20", HATCH)}
      />
    );
  }
  const n = rowValue(bucket, row);
  const step = cellStep(n);
  const partial = bucket.state === "partial";
  const arriving = bucket.state === "arriving";
  const unknownIn = (bucket.gap_accounts ?? []).map((a) => accountName(view, a));
  const missing = `unknown in ${unknownIn.length > 0 ? unknownIn.join(", ") : "part of the scope"}`;
  const note = partial ? `${fmtCount(n)}, ${missing}` : arriving ? `${fmtCount(n)}, not settled yet` : fmtCount(n);
  // A count of 1,000 or more shows short in the cell, so the hover carries the exact one.
  const short = n >= 1000;
  // A hatch band drawn over any part of this column sits beneath the count;
  // the count sits on a chip of the card colour so it reads over it. This goes
  // by the band as drawn, so a short gap widened to the minimum band width
  // still lifts the count in the column the widened band runs into.
  const colStart = (index / columns) * 100;
  const colEnd = ((index + 1) / columns) * 100;
  const underBand = drawn.some((d) => d.left < colEnd && d.right > colStart);
  return (
    <div
      role="cell"
      data-state={bucket.state}
      data-suspect={suspect || undefined}
      data-shown={(suspect && shown) || undefined}
      aria-label={`${label}: ${note}${suspect ? ", likely cause" : ""}`}
      title={
        long
          ? `${span}: ${note}`
          : short
            ? note
            : partial
              ? `Unknown in ${unknownIn.length > 0 ? unknownIn.join(", ") : "part of the scope"}`
              : undefined
      }
      style={style}
      className={clsx(
        "relative box-border flex h-7 items-center justify-center overflow-hidden whitespace-nowrap px-0.5 text-2xs tabular-nums",
        arriving ? "border border-dashed border-ink-400/60" : "border-r border-ink-500/20",
        row === "errors" ? ERROR_STEP_CLASS[step] : STEP_CLASS[step],
        step >= 2 ? "text-ink-50" : "text-ink-200",
        suspect && "z-[1] outline outline-2 -outline-offset-2 outline-sev-warn",
        suspect && shown && "ring-2 ring-sev-warn ring-offset-1 ring-offset-surface",
      )}
    >
      {partial && <span aria-hidden="true" className={clsx("absolute inset-0", PARTIAL_HATCH)} />}
      {n > 0 && (
        <span data-count className={clsx("relative", underBand && "z-[1] rounded-sm bg-surface px-px")}>
          {fmtCellCount(n)}
        </span>
      )}
    </div>
  );
}

function AlarmsChip({ view }: { view: ChangesView }) {
  const entries = alarmEntries(view);
  if (entries.length === 0) return null;
  const history = view.alarm_history_starts && Date.parse(view.alarm_history_starts) > Date.parse(view.window.since) ? view.alarm_history_starts : null;
  return (
    <Popover
      label="Alarms"
      width={340}
      triggerClassName="rounded border border-sev-critical/40 px-2 py-0.5 text-2xs text-sev-critical hover:bg-sev-critical/10"
      triggerAttrs={{ "data-alarms-chip": "" }}
      panelAttrs={{ "data-alarms-popover": "" }}
      trigger={`${fmtCount(entries.length)} ${entries.length === 1 ? "alarm" : "alarms"} ▾`}
    >
      <ul className="space-y-1.5">
        {entries.map((e) => (
          <li key={e.alarm.id} className="grid grid-cols-[minmax(0,1fr)_auto] gap-x-3" data-alarm-entry={e.alarm.id}>
            <span className="font-medium text-ink-50">{e.name}</span>
            <span className="font-mono text-2xs tabular-nums text-ink-300">{e.when}</span>
            <span className="col-span-2 text-2xs text-ink-300">{e.where}</span>
          </li>
        ))}
      </ul>
      {history && (
        <p className="mt-2 border-t border-ink-500/40 pt-2 text-2xs text-ink-300" data-alarm-history>
          Alarm history starts {fmtClockOrDay(history, view.window.until)}
        </p>
      )}
    </Popover>
  );
}

/** Local midnights inside the window, for the day labels of a multi-day axis. */
function midnights(since: string, until: string): Date[] {
  const a = Date.parse(since);
  const b = Date.parse(until);
  const out: Date[] = [];
  if (!(b > a)) return out;
  let d = startOfDay(new Date(a));
  if (d.getTime() <= a) d = addDays(d, 1);
  while (d.getTime() < b) {
    out.push(d);
    d = addDays(d, 1);
  }
  return out;
}

export function ActivityGrid({ view, shown = false, stale = false }: { view: ChangesView; shown?: boolean; stale?: boolean }) {
  const { buckets, stepSeconds } = gridBuckets(view);
  const long = stepSeconds > 3600;
  const layout = gridLayout(buckets.length);
  const s = view.summary;
  const rows: RowKey[] = ["pipeline", "person", "aws_service"];
  if (s.by_actor_class.unknown > 0) rows.push("unknown");
  rows.push("errors");

  const suspectItem = findSuspect(view);
  const suspectBucket = suspectItem ? bucketIndexFor(buckets, suspectItem.time) : -1;
  const bands = view.gaps.filter(isBandGap);
  const arriving = hasArrivingTail(view.window);
  const tickEvery = Math.max(1, Math.ceil(buckets.length / 8));
  const ends = buckets.map((_, i) => buckets[i + 1]?.start ?? view.window.until);
  const at = (iso: string) => columnPct(buckets, ends, iso);
  // A band is never drawn narrower than the minimum width, so a short gap's
  // band can run past the gap's end; cells decide their chip from this extent.
  const drawn: BandSpan[] = bands.map((g) => {
    const left = at(g.from);
    return { left, right: Math.max(at(g.to), left + MIN_BAND_PCT) };
  });
  // The unsettled outline starts at the edge of the column that holds
  // `settled_until`, not inside it, so its start border sits in that column's
  // border and padding and never over the column's count.
  const arrivingFrom = arriving ? (Math.max(0, bucketIndexFor(buckets, view.window.settled_until)) / Math.max(1, buckets.length)) * 100 : 0;
  // Nothing read yet, or the answer on screen is for another span: a row
  // total would claim a count, so the totals show a dash, as the card does.
  const blank = nothingRead(view) || stale;

  return (
    <section className="card" aria-labelledby="changes-grid-title">
      <div className="flex flex-wrap items-center justify-between gap-2 px-4 pt-3">
        <h2 id="changes-grid-title" className="card-title">
          Activity
        </h2>
        <AlarmsChip view={view} />
      </div>
      <div className="overflow-x-auto px-4 pt-3" data-grid-scroll>
        <div
          role="table"
          aria-label={long ? `Changes per ${stepSeconds / 3600} hours by actor` : "Changes per hour by actor"}
          data-columns={buckets.length}
          data-step-seconds={stepSeconds}
          style={{ minWidth: `${layout.tableMinPx}px` }}
        >
          {/* Layers: bands sit under the counts, which are lifted only where a
              band reaches their column; the arriving outline and alarm lines
              stay above both. */}
          <div role="rowgroup" className="relative">
            {rows.map((row) => {
              const total = row === "errors" ? s.errors : s.by_actor_class[row];
              return (
                <div role="row" key={row} data-row={row} className="flex items-stretch">
                  <div role="rowheader" className="flex w-28 shrink-0 items-center justify-between gap-2 px-2 text-xs sm:w-36">
                    <span className={clsx(row === "errors" ? "text-sev-critical" : "text-ink-100")}>
                      {row === "errors" ? "Errors" : ACTOR_LABEL[row]}
                    </span>
                    <span className="font-mono text-2xs tabular-nums text-ink-300" data-row-total>
                      {blank ? "—" : fmtCount(total)}
                    </span>
                  </div>
                  <div className="flex flex-1">
                    {buckets.map((b, i) => (
                      <Cell
                        key={b.start}
                        bucket={b}
                        end={ends[i]}
                        row={row}
                        view={view}
                        bands={bands}
                        drawn={drawn}
                        index={i}
                        long={long}
                        columns={buckets.length}
                        minWidth={layout.cellMinPx}
                        shown={shown}
                        suspect={i === suspectBucket && suspectItem?.actor_class === row}
                      />
                    ))}
                  </div>
                </div>
              );
            })}
            <div className="pointer-events-none absolute inset-y-0 left-28 right-0 sm:left-36" aria-hidden="true">
              {bands.map((g, k) => (
                <div
                  key={`band-${g.account}-${g.region}-${g.from}`}
                  data-band={g.reason}
                  title={bandTitle(view, g)}
                  className={clsx("pointer-events-auto absolute inset-y-0 border-x border-ink-400/50", HATCH)}
                  style={{ left: `${drawn[k].left}%`, width: `${drawn[k].right - drawn[k].left}%` }}
                />
              ))}
              {arriving && (
                <div
                  data-arriving-outline
                  className="absolute inset-y-0 z-[2] box-border border-2 border-dashed border-ink-300/70"
                  style={{ left: `${arrivingFrom}%`, right: 0 }}
                />
              )}
              {view.alarms
                .filter((a) => a.start_known && a.start)
                .map((a) => (
                  <div
                    key={`alarm-${a.id}`}
                    data-alarm-line={a.id}
                    title={alarmTitle(view, a)}
                    className="pointer-events-auto absolute inset-y-0 z-[2] w-2 -translate-x-1/2 cursor-help"
                    style={{ left: `${at(a.start as string)}%` }}
                  >
                    <div className="mx-auto h-full w-0.5 bg-sev-critical" />
                  </div>
                ))}
            </div>
          </div>
          {long ? (
            <DayAxis view={view} buckets={buckets} ends={ends} cellMinPx={layout.cellMinPx} />
          ) : (
            <div className="flex pt-1 text-2xs text-ink-300" aria-hidden="true">
              <div className="w-28 shrink-0 sm:w-36" />
              <div className="flex flex-1">
                {buckets.map((b, i) => (
                  <div key={b.start} style={columnStyle(buckets.length, layout.cellMinPx)} className="overflow-visible whitespace-nowrap">
                    {i % tickEvery === 0 ? fmtClock(b.start) : ""}
                  </div>
                ))}
              </div>
            </div>
          )}
        </div>
      </div>
      <div className="px-4 pb-4">
        {s.total > 0 && buckets.length > 0 && <BusiestHour buckets={buckets} ends={ends} long={long} until={view.window.until} />}
      </div>
    </section>
  );
}

/**
 * The axis of a multi-day window: a day label at each local midnight, placed
 * on the column grid as the overlays are, with a minor tick per column.
 */
function DayAxis({ view, buckets, ends, cellMinPx }: { view: ChangesView; buckets: ChangesBucket[]; ends: string[]; cellMinPx: number }) {
  const days = midnights(view.window.since, view.window.until);
  return (
    <div className="flex pt-0.5 text-2xs text-ink-300" aria-hidden="true" data-day-axis>
      <div className="w-28 shrink-0 sm:w-36" />
      <div className="relative flex-1">
        <div className="flex h-1.5">
          {buckets.map((b) => (
            <div key={b.start} data-minor-tick style={columnStyle(buckets.length, cellMinPx)} className="box-border border-l border-ink-500/40" />
          ))}
        </div>
        <div className="relative h-4">
          {days.map((d) => {
            const p = columnPct(buckets, ends, d.toISOString());
            return (
              <span
                key={d.getTime()}
                data-day-label
                className={clsx("absolute top-0 whitespace-nowrap border-l border-ink-300/70 pl-1", p > 90 && "-translate-x-full border-l-0 border-r pl-0 pr-1")}
                style={{ left: `${p}%` }}
              >
                {format(d, "MMM d")}
              </span>
            );
          })}
        </div>
      </div>
    </div>
  );
}

function BusiestHour({ buckets, ends, long, until }: { buckets: ChangesBucket[]; ends: string[]; long: boolean; until: string }) {
  let best: ChangesBucket | null = null;
  let bestEnd = until;
  let bestN = 0;
  for (const [i, b] of buckets.entries()) {
    if (b.state !== "complete") continue;
    const n = b.pipeline + b.person + b.aws_service + b.unknown;
    if (n > bestN) {
      best = b;
      bestEnd = ends[i];
      bestN = n;
    }
  }
  if (!best || bestN < 100) return null;
  return (
    <p className="mt-2 flex flex-wrap items-center gap-3 text-2xs text-ink-300">
      <span>
        {long ? `Busiest span ${fmtSpanOrDay(best.start, bestEnd, until)}` : `Busiest hour ${fmtClockOrDay(best.start, until)}`}: {fmtCount(bestN)} changes
      </span>
      <span className="inline-flex items-center gap-1">
        <span className="h-2 w-3 bg-accent/25" /> 1–99
        <span className="ml-1 h-2 w-3 bg-accent/50" /> 100–999
        <span className="ml-1 h-2 w-3 bg-accent/80" /> 1,000 or more
      </span>
    </p>
  );
}
