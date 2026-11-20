import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { Link } from "react-router-dom";
import { CheckCircle2, Info } from "lucide-react";
import { Dropdown } from "@/components/Dropdown";
import { RetryableError } from "@/components/RetryableError";
import { api } from "@/lib/api";
import {
  accountName,
  changesSpanSearch,
  fmtClockOrDay,
  fmtCount,
  isWindowLocked,
  maskActor,
  compareToOptions,
  resolveCompareFrom,
  resolveCompareTo,
  comparePresets,
  maxCompareAgeMs,
  presetAgo,
  type ChangesLimits,
  type CompareChanged,
  type CompareResource,
  type CompareView,
  type ChangesView,
} from "@/lib/awsChanges";
import { ValueHidden } from "./ChangeList";
import { idHintText, partialLines } from "./compareWords";
import { Popover } from "./Popover";

// Compare: how one picked time differs from another (now by default), from
// two saved snapshots. "Nothing differs" only when the server says the diff
// is ok, complete and empty; resource groups left out are counted on one
// line whose popover names each, never silently dropped.

const VISIBLE_CHANGED = 4;

/**
 * What tells look-alike resources apart: what the server's `id_hint` adds,
 * the Region, and the account when several are in scope or the line has
 * look-alikes, so an added and a removed copy both name their account.
 */
function Where({ r, view }: { r: CompareResource; view: ChangesView }) {
  const hint = idHintText(r);
  const parts = [r.region, view.accounts.length > 1 || r.id_hint ? accountName(view, r.account) : null].filter(Boolean);
  return (
    <>
      {hint && (
        <span className="font-mono text-ink-200" data-id-hint>
          {hint}
        </span>
      )}
      {parts.length > 0 && <span className="text-ink-300">{parts.join(" · ")}</span>}
    </>
  );
}

function ChangedResource({ r, view }: { r: CompareChanged; view: ChangesView }) {
  return (
    <li className="py-2">
      <p className="flex flex-wrap items-center gap-2 text-xs text-ink-50">
        <span className="text-ink-300">{r.type}</span>
        <span className="font-medium">{r.name}</span>
        <Where r={r} view={view} />
      </p>
      <ul className="mt-1 space-y-0.5">
        {r.fields.map((f) => (
          <li key={f.field} className="flex flex-wrap items-center gap-1.5 text-2xs text-ink-200">
            <span className="font-mono text-ink-300">{f.field}</span>
            {f.hidden ? (
              <>
                <span>changed</span>
                <ValueHidden />
              </>
            ) : (
              <span className="font-mono">
                {f.before ?? "(none)"} → {f.after ?? "(none)"}
              </span>
            )}
          </li>
        ))}
      </ul>
    </li>
  );
}

type Scope = { account?: string; region?: string; service?: string };

/** The change-list rows inside the compared span, with a link to the rest. */
function ChangesBetween({ c, view, scope, toLabel }: { c: CompareView; view: ChangesView; scope: Scope; toLabel: string }) {
  const items = c.changes_between ?? [];
  const total = Math.max(c.changes_between_total ?? 0, items.length);
  if (total === 0) return null;
  const more = total - items.length;
  return (
    <div className="mt-3" data-changes-between>
      <p className="text-2xs text-ink-200">
        Changes between {fmtClockOrDay(c.from, view.window.until)} and {toLabel} ({fmtCount(total)})
      </p>
      <ul className="mt-1 space-y-0.5">
        {items.map((i) => (
          <li key={i.id} className="flex gap-2 text-2xs text-ink-200">
            {/* Fixed width that fits "MMM d HH:mm", so summaries line up whether or not a row carries a date. */}
            <span className="w-[5.25rem] shrink-0 whitespace-nowrap tabular-nums text-ink-300" data-compare-time>{fmtClockOrDay(i.time, view.window.until)}</span>
            <span className="min-w-0 flex-1">
              {i.summary}
              {i.actor ? <span className="text-ink-300"> · {maskActor(i.actor)}</span> : null}
              {view.accounts.length > 1 && <span className="text-ink-300"> · {accountName(view, i.account)}</span>}
            </span>
          </li>
        ))}
      </ul>
      {more > 0 && (
        <Link to={`/aws/changes${changesSpanSearch(c.from, c.to, scope)}`} className="mt-1 inline-block text-2xs text-link hover:underline" data-changes-between-more>
          and {fmtCount(more)} more ›
        </Link>
      )}
    </div>
  );
}

function CompareBody({
  c,
  view,
  scope,
  toLabel,
  onUse,
}: {
  c: CompareView;
  view: ChangesView;
  scope: Scope;
  toLabel: string;
  onUse: (iso: string) => void;
}) {
  const [all, setAll] = useState(false);
  if (c.status === "unavailable") {
    const oldest = c.snapshots?.oldest ?? null;
    return (
      <div className="rounded bg-ink-600/30 px-3 py-3 text-xs text-ink-100" data-compare="unavailable">
        <p className="font-medium">Compare not available for {fmtClockOrDay(c.from, view.window.until)}</p>
        <p className="mt-1 text-ink-200">
          {c.history_starts
            ? `Earliest full compare: ${fmtClockOrDay(c.history_starts, view.window.until)}; pick that or a later time.`
            : "No snapshot covers that time; pick a later time."}
        </p>
        {c.history_starts && (
          <button type="button" className="btn mt-2" onClick={() => onUse(c.history_starts as string)}>
            Use {fmtClockOrDay(c.history_starts, view.window.until)}
          </button>
        )}
        {/* Only when no full compare is offered: two "earliest" times in one box read as a contradiction. */}
        {oldest && !c.history_starts && (
          <p className="mt-1 text-ink-200" data-oldest-snapshot>
            Oldest snapshot: {fmtClockOrDay(oldest, view.window.until)}
          </p>
        )}
        <ChangesBetween c={c} view={view} scope={scope} toLabel={toLabel} />
      </div>
    );
  }
  const empty = c.added.length === 0 && c.removed.length === 0 && c.changed.length === 0;
  const nothing = c.complete && empty;
  const lines = partialLines(c, view);
  const leftOut = lines.some((l) => l.excluded);
  const shown = all ? c.changed : c.changed.slice(0, VISIBLE_CHANGED);
  return (
    <div data-compare={nothing ? "nothing" : "ok"}>
      {nothing ? (
        <div className="py-4 text-center">
          <CheckCircle2 size={20} aria-hidden="true" className="mx-auto text-sev-ok" />
          <p className="mt-2 text-sm text-ink-50">
            {leftOut ? "Nothing differs in the compared services" : `Nothing differs between ${fmtClockOrDay(c.from, view.window.until)} and ${toLabel}`}
          </p>
          <p className="mt-1 text-2xs text-ink-300">
            {leftOut ? `Compared ${fmtClockOrDay(c.from, view.window.until)} with ${toLabel}.` : "Both snapshots complete."}
          </p>
        </div>
      ) : (
        <>
          <dl className="grid grid-cols-3 gap-2 text-center">
            {(
              [
                ["added", c.added.length],
                ["removed", c.removed.length],
                ["changed", c.changed.length],
              ] as const
            ).map(([k, n]) => (
              <div key={k} className="rounded border border-ink-500/50 px-2 py-1.5">
                <dd className="text-lg tabular-nums text-ink-50">{fmtCount(n)}</dd>
                <dt className="text-2xs text-ink-300">{k}</dt>
              </div>
            ))}
          </dl>
          {empty && !c.complete && (
            <p className="mt-2 text-2xs text-ink-200">No difference found in the compared groups.</p>
          )}
          <ul className="mt-2 divide-y divide-ink-500/30">
            {shown.map((r) => (
              <ChangedResource key={r.resource_arn} r={r} view={view} />
            ))}
          </ul>
          {c.changed.length > VISIBLE_CHANGED && !all && (
            <button type="button" className="text-2xs text-link hover:underline" onClick={() => setAll(true)}>
              Show {fmtCount(c.changed.length - VISIBLE_CHANGED)} more ›
            </button>
          )}
          {(c.added.length > 0 || c.removed.length > 0) && (
            <ul className="mt-2 space-y-0.5 text-2xs text-ink-200">
              {c.added.map((r) => (
                <li key={`a-${r.resource_arn}`} className="flex flex-wrap gap-x-1.5" data-compare-line>
                  <span className="text-sev-ok">Added</span> {r.type} {r.name} <Where r={r} view={view} />
                </li>
              ))}
              {c.removed.map((r) => (
                <li key={`r-${r.resource_arn}`} className="flex flex-wrap gap-x-1.5" data-compare-line>
                  <span className="text-sev-critical">Removed</span> {r.type} {r.name} <Where r={r} view={view} />
                </li>
              ))}
            </ul>
          )}
        </>
      )}
      {lines.length > 0 && (
        <div className="mt-3 text-2xs">
          <Popover
            label="Groups not compared"
            width={380}
            triggerClassName="rounded text-ink-200 hover:underline"
            triggerAttrs={{ "data-not-compared": "" }}
            panelAttrs={{ "data-compare-partial": "" }}
            trigger={`${fmtCount(lines.length)} ${lines.length === 1 ? "group" : "groups"} not compared ▾`}
          >
            <ul className="space-y-1">
              {lines.map((l) => (
                <li key={l.key} className="text-2xs text-ink-100" data-excluded={l.excluded || undefined} data-gap-text>
                  {l.text}
                </li>
              ))}
            </ul>
            {/* `history_starts` is the earliest full compare; offer it only when this one is not full. */}
            {c.history_starts && Date.parse(c.history_starts) > Date.parse(c.from) && (
              <button type="button" className="btn mt-2" onClick={() => onUse(c.history_starts as string)}>
                Use {fmtClockOrDay(c.history_starts, view.window.until)} for a full compare
              </button>
            )}
          </Popover>
        </div>
      )}
      <ChangesBetween c={c} view={view} scope={scope} toLabel={toLabel} />
    </div>
  );
}

export function ComparePanel({
  view,
  value,
  onChange,
  to = "",
  onToChange = () => {},
  scope,
  limits,
}: {
  view: ChangesView;
  /** The edition's limits from the Changes answer; the Compare answer's win once it lands. */
  limits?: ChangesLimits;
  value: string;
  onChange: (v: string) => void;
  /** The compare end: "" for now, or an RFC 3339 time between `from` and now. */
  to?: string;
  onToChange?: (v: string) => void;
  scope: Scope;
}) {
  const from = resolveCompareFrom(value, view.window.until);
  const toTime = resolveCompareTo(to, from, view.window.until);
  const compare = useQuery({
    queryKey: ["aws-changes", "compare", from, toTime, scope.account ?? "", scope.region ?? "", scope.service ?? ""],
    queryFn: () => api.awsChangesCompare({ from, to: toTime || undefined, account: scope.account, region: scope.region, service: scope.service }),
  });
  // The starting points reach back as far as the edition keeps history.
  const presets = comparePresets(maxCompareAgeMs(compare.data?.limits ?? limits ?? view.limits));
  const options: Array<{ value: string; label: string }> = presets.map((p) => ({
    value: p.key,
    label: `${fmtClockOrDay(resolveCompareFrom(p.key, view.window.until), view.window.until)} (${presetAgo(p.hours)})`,
  }));
  if (!presets.some((p) => p.key === value)) options.push({ value, label: fmtClockOrDay(from, view.window.until) });
  const toOptions = compareToOptions(from, toTime, view.window.until);
  const toLabel = toTime ? fmtClockOrDay(toTime, view.window.until) : "now";

  return (
    <section className="card flex h-full min-w-0 flex-col" aria-labelledby="changes-compare-title">
      <div className="flex shrink-0 flex-wrap items-center gap-2 px-4 pb-2 pt-3">
        <h2 id="changes-compare-title" className="card-title">
          Compare
        </h2>
        {/* The list grows to its longest option, "Nov 18 14:23 (2 d ago)", whatever the trigger's width. */}
        <Dropdown
          value={value}
          options={options}
          onChange={onChange}
          aria-label="Compare from"
          className="min-w-[9rem]"
          listClassName="w-max min-w-full max-w-[20rem]"
          wrapOptions
        />
        <span aria-hidden="true" className="text-xs text-ink-300">
          →
        </span>
        <Dropdown value={toTime} options={toOptions} onChange={onToChange} aria-label="Compare to" className="min-w-[7rem]" />
      </div>
      <div className="min-h-0 flex-1 overflow-y-auto border-t border-ink-500/30 px-4 pb-4 pt-3" data-compare-scroll>
        {compare.isPending ? (
          <div className="sk h-24 rounded" />
        ) : compare.isError ? (
          isWindowLocked(compare.error) ? (
            <p className="flex items-start gap-1.5 text-2xs text-ink-300" data-compare="locked">
              <Info size={12} aria-hidden="true" className="mt-0.5 shrink-0" />
              Compare beyond the last 24 h is part of Enterprise; pick a time inside the last 24 h.
            </p>
          ) : (
            <RetryableError error={compare.error} onRetry={() => void compare.refetch()} context="Couldn't load the comparison" />
          )
        ) : (
          <CompareBody c={compare.data} view={view} scope={scope} toLabel={toLabel} onUse={onChange} />
        )}
      </div>
    </section>
  );
}
