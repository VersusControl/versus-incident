import { Link } from "react-router-dom";
import clsx from "clsx";
import { AlertTriangle, Copy, Lock } from "lucide-react";
import { useToast } from "@/components/toastContext";
import { copyText } from "@/lib/clipboard";
import {
  accountName,
  ACTOR_LABEL,
  accountsFraction,
  clockSkewText,
  droppedText,
  fmtClockOrDay,
  fmtCount,
  gapReasonText,
  lastChangeBeforeText,
  lateArrivalsText,
  likelyCause,
  missingStretches,
  noChangesText,
  nothingRead,
  notEnabledRegions,
  permissionFix,
  searchedText,
  serviceFilterLabel,
  stretchSpan,
  topActorLabels,
  warningLine,
  type ChangeItem,
  type ChangesView,
  type ChangesWindow,
  type ChangesWindowOption,
} from "@/lib/awsChanges";
import { Popover } from "./Popover";
import { SummaryCard, SummaryNumber, WarningLine } from "../shared/SummaryCard";

// The Changes summary card: the shared card's row of plain numbers, the
// likely cause, the top actors and, only when data in the window is
// missing, one warning line whose popover lists every gap.

/**
 * 1h, 24h and 7d. 7d opens when the edition's `limits` serve it; otherwise
 * it stays listed, locked, with the Enterprise title.
 */
export function WindowSwitch({
  value,
  locked,
  custom,
  allow7d = false,
  onChange,
}: {
  value: ChangesWindow;
  locked: boolean;
  custom: boolean;
  /** The edition serves the 7 day window. */
  allow7d?: boolean;
  onChange: (w: ChangesWindowOption) => void;
}) {
  const open: ChangesWindow[] = allow7d ? ["1h", "24h", "7d"] : ["1h", "24h"];
  return (
    <div role="group" aria-label="Window" className="inline-flex shrink-0 rounded-control border border-ink-500 bg-surface-raised p-0.5">
      {open.map((w) => (
        <button
          key={w}
          type="button"
          aria-pressed={!custom && value === w}
          onClick={() => onChange(w)}
          className={clsx(
            "min-h-7 rounded px-3 text-xs font-medium focus:outline-none focus-visible:ring-2 focus-visible:ring-accent",
            !custom && value === w ? "bg-accent-subtle text-ink-50" : "text-ink-300 hover:text-ink-100",
          )}
        >
          {w}
        </button>
      ))}
      {!allow7d && <button
        type="button"
        aria-disabled="true"
        aria-pressed={false}
        aria-label="7d, Enterprise"
        title="The 7 day window is part of Enterprise"
        onClick={() => onChange("7d")}
        data-locked={locked || undefined}
        className="inline-flex min-h-7 cursor-not-allowed items-center gap-1 rounded px-3 text-xs font-medium text-ink-400 focus:outline-none focus-visible:ring-2 focus-visible:ring-accent"
      >
        7d
        <Lock size={10} aria-hidden="true" />
      </button>}
    </div>
  );
}

function LikelyCauseRow({
  view,
  onShow,
  onShowOutside,
}: {
  view: ChangesView;
  onShow: (id: string) => void;
  onShowOutside: (item: ChangeItem) => void;
}) {
  const cause = likelyCause(view);
  if (!cause) return null;
  const base = "mx-4 mt-3 flex flex-wrap items-center gap-x-3 gap-y-1 rounded bg-ink-600/30 px-3 py-2 text-xs";
  if (cause.kind === "status") {
    return (
      <div className={base} data-likely-cause="status" title={searchedText(view) ?? undefined}>
        <span className="text-2xs text-ink-300">Likely cause</span>
        <AlertTriangle size={13} aria-hidden="true" className="shrink-0 text-ink-300" />
        <span className="text-ink-100" data-likely-cause-text>
          <span className="font-semibold text-ink-50">{cause.alarm}</span>: {cause.text}
        </span>
      </div>
    );
  }
  const it = cause.item;
  // The likely cause is found over every service; one outside the service
  // filter is still named, marked, never hidden.
  const outside = view.suspect.in_filter === false;
  return (
    <div className={clsx(base, "border-l-2 border-sev-warn")} data-likely-cause="found">
      <span className="text-2xs text-ink-300">Likely cause</span>
      {it ? (
        <>
          <span className="font-mono text-2xs tabular-nums text-ink-300">{fmtClockOrDay(it.time, view.window.until)}</span>
          <span className="min-w-0 text-ink-50" data-likely-cause-text>
            {it.summary}
          </span>
          <span className="rounded border border-ink-500 px-1.5 text-2xs text-ink-200" data-likely-account>
            {accountName(view, it.account)}
          </span>
          {it.region && (
            <span className="rounded border border-ink-500 px-1.5 text-2xs text-ink-200" data-likely-region>
              {it.region}
            </span>
          )}
          {outside && (
            <span className="rounded border border-sev-warn/60 px-1.5 text-2xs text-sev-warn" data-outside-filter>
              Not in your service filter
            </span>
          )}
          <button
            type="button"
            className="ml-auto text-2xs text-link hover:underline"
            onClick={() => (outside ? onShowOutside(it) : onShow(it.id))}
          >
            Show ›
          </button>
        </>
      ) : (
        <span className="text-ink-200" data-likely-cause-text>
          a change not on the loaded page
        </span>
      )}
    </div>
  );
}

function TopActors({ view }: { view: ChangesView }) {
  if ((view.summary.top_actors?.length ?? 0) === 0) return null;
  return (
    <p className="mx-4 mt-3 flex flex-wrap items-center gap-x-5 gap-y-1 text-2xs text-ink-300" data-top-actors>
      <span>Top actors</span>
      {topActorLabels(view).map((a) => (
        <span key={a.key} className="text-ink-200" data-top-actor>
          {a.label} <span className="font-mono tabular-nums text-ink-100">{fmtCount(a.count)}</span>
        </span>
      ))}
    </p>
  );
}

/** Late arrivals and clock skew: collector health, shown only when there is something to say. */
function SyncNotes({ view }: { view: ChangesView }) {
  const notes = [
    ["late", lateArrivalsText(view.sync.late_arrivals)],
    ["skew", clockSkewText(view.sync.clock_skew_seconds)],
  ].filter((n): n is [string, string] => Boolean(n[1]));
  if (notes.length === 0) return null;
  return (
    <ul className="mx-4 mt-2 space-y-0.5 text-2xs text-ink-300" data-sync-notes>
      {notes.map(([k, text]) => (
        <li key={k} data-sync-note={k}>
          {text}
        </li>
      ))}
    </ul>
  );
}

/** Everything behind the warning line: each gap, the Regions not enabled, skipped rows. */
function WarningDetails({ view }: { view: ChangesView }) {
  const toast = useToast();
  const stretches = missingStretches(view);
  const notEnabled = notEnabledRegions(view);
  const dropped = droppedText(view.sync.dropped);
  return (
    <div className="space-y-2">
      {stretches.length > 0 && (
        <ul className="space-y-1.5">
          {stretches.map((g) => (
            <li key={g.key} className="grid grid-cols-[minmax(0,1fr)_auto] gap-x-3 gap-y-0.5" data-stretch={g.reason}>
              <span>
                <span className="font-medium text-ink-50">{accountName(view, g.account)}</span>
                {g.region ? ` ${g.region}` : ""}
              </span>
              <span className="font-mono text-2xs tabular-nums text-ink-300">{stretchSpan(view, g.from, g.to)}</span>
              <span className="col-span-2 text-2xs text-ink-200" data-stretch-reason>
                {gapReasonText(g.reason)}
                {g.missing_permission ? `, missing ${g.missing_permission}` : ""}
                {g.detail ? ` (${g.detail})` : ""}
              </span>
              {(g.reason === "cloudtrail_denied" && g.missing_permission) || g.reason === "account_disconnected" ? (
                <span className="col-span-2 flex gap-3 text-2xs">
                  {g.reason === "cloudtrail_denied" && g.missing_permission && (
                    <button
                      type="button"
                      className="inline-flex items-center gap-1 text-link hover:underline"
                      onClick={async () => {
                        const ok = await copyText(permissionFix(g.missing_permission as string));
                        toast.push({ tone: ok ? "ok" : "error", title: ok ? "Fix copied" : "Copy failed" });
                      }}
                    >
                      <Copy size={11} aria-hidden="true" />
                      Copy fix
                    </button>
                  )}
                  {g.reason === "account_disconnected" && (
                    <Link to="/aws/settings" className="text-link hover:underline">
                      Reconnect ›
                    </Link>
                  )}
                </span>
              ) : null}
            </li>
          ))}
        </ul>
      )}
      {notEnabled.length > 0 && (
        <ul className={clsx("space-y-1", stretches.length > 0 && "border-t border-ink-500/40 pt-2")}>
          {notEnabled.map((r) => (
            <li key={r} className="flex justify-between gap-3" data-not-enabled-region={r}>
              <span>{r}</span>
              <span className="text-2xs text-ink-300">not enabled</span>
            </li>
          ))}
        </ul>
      )}
      {(dropped || view.summary.truncated) && (
        <ul className="space-y-1 border-t border-ink-500/40 pt-2 text-2xs text-ink-200">
          {dropped && <li data-dropped>{dropped}</li>}
          {view.summary.truncated && <li data-truncated>The collector hit its cap, so more changes exist</li>}
        </ul>
      )}
    </div>
  );
}

export function ChangesSummaryCard({
  view,
  window,
  custom,
  locked,
  onWindow,
  onShow,
  onShowOutside = () => {},
  services = [],
  stale = false,
  allow7d = false,
}: {
  view: ChangesView;
  window: ChangesWindow;
  custom: boolean;
  locked: boolean;
  onWindow: (w: ChangesWindowOption) => void;
  onShow: (id: string) => void;
  /** Show › on a likely cause outside the service filter. */
  onShowOutside?: (item: ChangeItem) => void;
  /** The service filter the numbers answer; empty means all services. */
  services?: readonly string[];
  /**
   * `view` answers another span or filter and stands in while this one
   * loads: every number is a dash and the card reads as loading.
   */
  stale?: boolean;
  /** The edition serves the 7 day window. */
  allow7d?: boolean;
}) {
  const s = view.summary;
  const warning = warningLine(view, window, custom);
  // Nothing read yet, or the answer on screen is for another span: a number
  // would claim a count, so every number shows a dash. The warning line says
  // why when nothing was read.
  const blank = nothingRead(view) || stale;
  const n = (v: number) => (blank ? "—" : fmtCount(v));
  const noChanges = !warning && !blank && s.total === 0;
  const before = noChanges ? lastChangeBeforeText(view) : null;
  return (
    <SummaryCard loading={stale}>
      <div className="flex flex-wrap items-start justify-between gap-3 px-4 pt-3">
        <div className="flex flex-wrap items-end gap-x-8 gap-y-3" data-numbers aria-live="polite">
          {noChanges && (
            <div className="self-center pr-6" data-no-changes>
              <p className="text-base text-ink-50">{noChangesText(view, window, custom, services)}</p>
              {before && (
                <p className="mt-0.5 text-2xs text-ink-300" data-last-change-before>
                  {before}
                </p>
              )}
            </div>
          )}
          <dl className="flex flex-wrap items-end gap-x-8 gap-y-3">
            {!noChanges && (
              <>
                {/* Every figure in this row counts changes, as the grid's rows do. */}
                <SummaryNumber k="changes" label="Changes" value={n(s.total)} />
                <SummaryNumber k="people" label="By people" value={n(s.by_actor_class.person)} />
                <SummaryNumber k="pipeline" label={ACTOR_LABEL.pipeline} value={n(s.by_actor_class.pipeline)} />
                <SummaryNumber k="aws_service" label="AWS service" value={n(s.by_actor_class.aws_service)} />
                {s.by_actor_class.unknown > 0 && <SummaryNumber k="unknown" label="Unknown actor" value={n(s.by_actor_class.unknown)} />}
              </>
            )}
            {(!noChanges || s.errors > 0) && <SummaryNumber k="errors" label="Failed calls" value={n(s.errors)} bad={!blank && s.errors > 0} />}
            {view.accounts.length > 0 && <SummaryNumber k="accounts" label="Accounts" value={blank ? "—" : accountsFraction(view)} />}
          </dl>
          {stale && (
            <p className="self-end pb-1 text-xs text-ink-300" data-loading-text>
              Loading…
            </p>
          )}
          {services.length > 0 && !noChanges && (
            <p className="self-end pb-1 text-xs text-ink-200" data-service-filter title="Service filter">
              <span className="sr-only">Service filter: </span>
              {serviceFilterLabel(services, view.services)}
            </p>
          )}
        </div>
        <WindowSwitch value={window} locked={locked} custom={custom} allow7d={allow7d} onChange={onWindow} />
      </div>
      {!stale && <LikelyCauseRow view={view} onShow={onShow} onShowOutside={onShowOutside} />}
      {!blank && <TopActors view={view} />}
      <SyncNotes view={view} />
      {warning && (
        <WarningLine>
          {warning.reading ? (
            <p className="inline-flex items-center gap-2" data-warning-text>
              <AlertTriangle size={14} aria-hidden="true" className="shrink-0 text-sev-warn" />
              {warning.text}
            </p>
          ) : (
            <Popover
              label="Missing data"
              width={420}
              triggerClassName="inline-flex items-start gap-2 rounded text-left hover:underline"
              panelAttrs={{ "data-warning-popover": "" }}
              trigger={
                <>
                  <AlertTriangle size={14} aria-hidden="true" className="mt-0.5 shrink-0 text-sev-warn" />
                  <span data-warning-text>{warning.text}</span>
                </>
              }
            >
              <WarningDetails view={view} />
            </Popover>
          )}
          {warning.reconnect && (
            <Link to="/aws/settings" className="font-semibold text-link hover:underline" data-reconnect>
              Reconnect ›
            </Link>
          )}
          {/* Every warning line opens the coverage view, as on the other AWS pages. */}
          {!warning.reading && (
            <Link to="/aws/coverage" className="ml-4 text-link hover:underline" data-details>
              Details ›
            </Link>
          )}
        </WarningLine>
      )}
    </SummaryCard>
  );
}
