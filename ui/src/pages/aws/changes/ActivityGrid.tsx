import clsx from "clsx";
import {
  ACTOR_LABEL,
  accountName,
  alarmEntries,
  alarmTitle,
  bucketIndexFor,
  cellStep,
  fmtClock,
  fmtClockOrDay,
  fmtCount,
  fmtSpanOrDay,
  gapDetail,
  gapReasonText,
  hasArrivingTail,
  isBandGap,
  missingStretches,
  nothingRead,
  pct,
  suspectItem as findSuspect,
  type ActorClass,
  type ChangesBucket,
  type ChangesGap,
  type ChangesView,
  type MissingStretch,
} from "@/lib/awsChanges";
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

function Cell({
  bucket,
  end,
  row,
  suspect,
  shown,
  view,
  bands,
}: {
  bucket: ChangesBucket;
  /** Where the bucket ends: the next bucket's start, or the window end. */
  end: string;
  row: RowKey;
  suspect: boolean;
  shown: boolean;
  view: ChangesView;
  bands: ChangesGap[];
}) {
  const label = `${row === "errors" ? "Errors" : ACTOR_LABEL[row]} ${fmtClockOrDay(bucket.start, view.window.until)}`;
  if (bucket.state === "gap") {
    const over = bands.filter((g) => Date.parse(g.from) < Date.parse(end) && Date.parse(g.to) > Date.parse(bucket.start));
    const overlaps = (from: string, to: string) => Date.parse(from) < Date.parse(end) && Date.parse(to) > Date.parse(bucket.start);
    const stretches = over.length > 0 ? [] : missingStretches(view).filter((g) => overlaps(g.from, g.to));
    const why =
      over.length > 0
        ? over.map((g) => bandTitle(view, g)).join("; ")
        : stretches.length > 0
          ? stretches.map((g) => stretchTitle(view, g)).join("; ")
          : "Not known for this hour";
    return (
      <div
        role="cell"
        data-state="gap"
        aria-label={`${label}: unknown`}
        title={why}
        className={clsx("h-7 min-w-[1.5rem] flex-1 border-r border-ink-500/20", HATCH)}
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
  return (
    <div
      role="cell"
      data-state={bucket.state}
      data-suspect={suspect || undefined}
      data-shown={(suspect && shown) || undefined}
      aria-label={`${label}: ${note}${suspect ? ", likely cause" : ""}`}
      title={partial ? `Unknown in ${unknownIn.length > 0 ? unknownIn.join(", ") : "part of the scope"}` : undefined}
      className={clsx(
        "relative flex h-7 min-w-[1.5rem] flex-1 items-center justify-center text-2xs tabular-nums",
        arriving ? "border border-dashed border-ink-400/60" : "border-r border-ink-500/20",
        row === "errors" ? ERROR_STEP_CLASS[step] : STEP_CLASS[step],
        step >= 2 ? "text-ink-50" : "text-ink-200",
        suspect && "z-[1] outline outline-2 -outline-offset-2 outline-sev-warn",
        suspect && shown && "ring-2 ring-sev-warn ring-offset-1 ring-offset-surface",
      )}
    >
      {partial && <span aria-hidden="true" className={clsx("absolute inset-0", PARTIAL_HATCH)} />}
      {n > 0 && <span className="relative">{fmtCount(n)}</span>}
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

export function ActivityGrid({ view, shown = false, stale = false }: { view: ChangesView; shown?: boolean; stale?: boolean }) {
  const buckets = view.buckets;
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
        <div role="table" aria-label="Changes per hour by actor" className="min-w-[36rem]">
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
                        shown={shown}
                        suspect={i === suspectBucket && suspectItem?.actor_class === row}
                      />
                    ))}
                  </div>
                </div>
              );
            })}
            <div className="pointer-events-none absolute inset-y-0 left-28 right-0 sm:left-36" aria-hidden="true">
              {bands.map((g) => (
                <div
                  key={`band-${g.account}-${g.region}-${g.from}`}
                  data-band={g.reason}
                  title={bandTitle(view, g)}
                  className={clsx("pointer-events-auto absolute inset-y-0 border-x border-ink-400/50", HATCH)}
                  style={{ left: `${pct(view, g.from)}%`, width: `${Math.max(0.5, pct(view, g.to) - pct(view, g.from))}%` }}
                />
              ))}
              {arriving && (
                <div
                  data-arriving-outline
                  className="absolute inset-y-0 border-2 border-dashed border-ink-300/70"
                  style={{ left: `${pct(view, view.window.settled_until)}%`, right: 0 }}
                />
              )}
              {view.alarms
                .filter((a) => a.start_known && a.start)
                .map((a) => (
                  <div
                    key={`alarm-${a.id}`}
                    data-alarm-line={a.id}
                    title={alarmTitle(view, a)}
                    className="pointer-events-auto absolute inset-y-0 w-2 -translate-x-1/2 cursor-help"
                    style={{ left: `${pct(view, a.start as string)}%` }}
                  >
                    <div className="mx-auto h-full w-0.5 bg-sev-critical" />
                  </div>
                ))}
            </div>
          </div>
          <div className="flex pt-1 text-2xs text-ink-300" aria-hidden="true">
            <div className="w-28 shrink-0 sm:w-36" />
            <div className="flex flex-1">
              {buckets.map((b, i) => (
                <div key={b.start} className="min-w-[1.5rem] flex-1 overflow-visible whitespace-nowrap">
                  {i % tickEvery === 0 ? fmtClock(b.start) : ""}
                </div>
              ))}
            </div>
          </div>
        </div>
      </div>
      <div className="px-4 pb-4">{s.total > 0 && buckets.length > 0 && <BusiestHour buckets={buckets} until={view.window.until} />}</div>
    </section>
  );
}

function BusiestHour({ buckets, until }: { buckets: ChangesBucket[]; until: string }) {
  let best: ChangesBucket | null = null;
  let bestN = 0;
  for (const b of buckets) {
    if (b.state !== "complete") continue;
    const n = b.pipeline + b.person + b.aws_service + b.unknown;
    if (n > bestN) {
      best = b;
      bestN = n;
    }
  }
  if (!best || bestN < 100) return null;
  return (
    <p className="mt-2 flex flex-wrap items-center gap-3 text-2xs text-ink-300">
      <span>
        Busiest hour {fmtClockOrDay(best.start, until)}: {fmtCount(bestN)} changes
      </span>
      <span className="inline-flex items-center gap-1">
        <span className="h-2 w-3 bg-accent/25" /> 1–99
        <span className="ml-1 h-2 w-3 bg-accent/50" /> 100–999
        <span className="ml-1 h-2 w-3 bg-accent/80" /> 1,000 or more
      </span>
    </p>
  );
}
