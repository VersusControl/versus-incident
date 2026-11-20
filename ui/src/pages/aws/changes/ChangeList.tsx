import { useEffect, useRef, useState } from "react";
import { useInfiniteQuery } from "@tanstack/react-query";
import clsx from "clsx";
import { AlertTriangle, Bot, CircleHelp, Cloud, GitBranch, Layers, Lock, User } from "lucide-react";
import { api } from "@/lib/api";
import {
  ACTOR_LABEL,
  accountName,
  appendUnique,
  causeAlarm,
  equallyLikelyIds,
  fmtClockOrDay,
  fmtSpanOrDay,
  fmtCount,
  changesText,
  isCursorExpired,
  maskActor,
  multiRegion,
  pastCoverage,
  routineText,
  type ActorClass,
  type ChangeField,
  type ChangeItem,
  type ChangesAlarm,
  type ChangesView,
} from "@/lib/awsChanges";

// The change list: one plain sentence per change, newest first. Bursts and
// a pipeline's run on one stack arrive already grouped (the server groups
// before paging), each with Expand. Routine agent updates are counts only and
// hidden unless asked for. "Load more" at the end of the card's own scroll
// walks the server's keyset cursor; there are no page numbers because late
// events insert mid-list. Missing data is not repeated here: the summary
// card's warning line and the grid's hatching carry it.

const ACTOR_ICON: Record<ActorClass, typeof User> = {
  pipeline: GitBranch,
  person: User,
  aws_service: Cloud,
  unknown: CircleHelp,
};

export function ValueHidden() {
  return (
    <span className="pill inline-flex items-center gap-1" data-value-hidden>
      <Lock size={10} aria-hidden="true" />
      value hidden
    </span>
  );
}

function FieldLine({ f }: { f: ChangeField }) {
  return (
    <li className="flex items-baseline gap-1.5 text-2xs text-ink-200" data-field-line>
      <span className="shrink-0 font-mono text-ink-300">{f.field}</span>
      <span className="flex min-w-0 flex-wrap items-baseline gap-x-1.5 break-all">
      {f.before_status === "hidden" ? (
        <>
          <span>changed</span>
          <ValueHidden />
        </>
      ) : f.before_status === "known" ? (
        <span className="font-mono">
          {f.before ?? ""} → {f.after ?? ""}
        </span>
      ) : (
        <>
          <span className="font-mono">→ {f.after ?? ""}</span>
          <span className="text-ink-300">
            {f.before_status === "ambiguous"
              ? "(before unclear: changed during a snapshot)"
              : f.missing_permission
                ? `(new value only; the before value needs ${f.missing_permission})`
                : "(new value only)"}
          </span>
        </>
      )}
      </span>
    </li>
  );
}

function noop() {}

function GroupMembers({ id, until, onCursorExpired }: { id: string; until: string; onCursorExpired: () => void }) {
  const members = useInfiniteQuery({
    queryKey: ["aws-changes", "group", id],
    queryFn: ({ pageParam }) => api.awsChangesGroup(id, pageParam || null),
    initialPageParam: "",
    getNextPageParam: (last) => last.next_cursor || undefined,
  });
  // The group token or its cursor fell out of the 24 h tier: the list has
  // moved on, so the page reloads its first page instead of showing an error.
  const expired = members.isError && isCursorExpired(members.error);
  const reported = useRef(false);
  useEffect(() => {
    if (expired && !reported.current) {
      reported.current = true;
      onCursorExpired();
    }
  }, [expired, onCursorExpired]);
  if (members.isPending) return <p className="mt-2 text-2xs text-ink-300">Loading…</p>;
  if (expired) return null;
  if (members.isError) return <p className="mt-2 text-2xs text-sev-critical">Couldn't load this group.</p>;
  const items = appendUnique(members.data.pages.map((p) => p.items));
  return (
    <div className="mt-2 border-l border-ink-500/50 pl-3">
      <ul className="space-y-1" aria-label="Group members">
        {items.map((m) => (
          <li key={m.id} className="text-2xs text-ink-200">
            <span className="tabular-nums text-ink-300">{fmtClockOrDay(m.time, until)}</span> {m.summary}
            {m.error_code && <span className="ml-1 text-sev-critical">{m.error_code}</span>}
          </li>
        ))}
      </ul>
      {members.hasNextPage && (
        <button
          type="button"
          className="mt-1 text-2xs text-link hover:underline"
          disabled={members.isFetchingNextPage}
          onClick={() => void members.fetchNextPage()}
        >
          Show more in this group
        </button>
      )}
    </div>
  );
}

function Row({
  item,
  view,
  multiAccount,
  showRegion,
  alarm,
  tag,
  shown,
  onCursorExpired,
}: {
  item: ChangeItem;
  view: ChangesView;
  multiAccount: boolean;
  /** The scope reads more than one Region: the row names its own. */
  showRegion: boolean;
  alarm: ChangesAlarm | undefined;
  /** The row's likely-cause tag, if any. */
  tag: "likely" | "equal" | null;
  /** The summary card's Show › pointed here. */
  shown: boolean;
  onCursorExpired: () => void;
}) {
  const [open, setOpen] = useState(false);
  const Icon = item.kind === "burst" ? AlertTriangle : item.kind === "group" ? Layers : item.kind === "routine" ? Bot : ACTOR_ICON[item.actor_class];
  const suspect = tag === "likely";
  const account = view.accounts.find((a) => a.account === item.account);
  const afterAlarm =
    alarm && alarm.start_known && alarm.start && !suspect && Date.parse(item.time) > Date.parse(alarm.start);
  const hidden = item.fields.some((f) => f.before_status === "hidden");
  const time =
    fmtSpanOrDay(item.time, item.kind !== "change" ? item.end_time : null, view.window.until);
  // The principal name whenever the server sends one, whatever its class.
  const actor = item.actor ? maskActor(item.actor) : "Unknown actor";
  const grouped = item.kind === "burst" || item.kind === "group";
  const expandable = item.kind !== "routine";

  return (
    <li
      data-item={item.id}
      data-kind={item.kind}
      data-shown={shown || undefined}
      className={clsx(
        "scroll-my-2 px-4 py-2.5",
        item.kind === "burst" && "bg-sev-critical/5",
        (item.actor_class === "unknown" || hidden) && "bg-ink-600/20",
        suspect && "border-l-2 border-sev-warn bg-sev-warn/5",
        shown && "outline outline-2 -outline-offset-2 outline-sev-warn",
      )}
    >
      <div className="flex gap-3">
        <span className="w-20 shrink-0 pt-0.5 text-2xs tabular-nums text-ink-300">{time}</span>
        <span className="mt-0.5 shrink-0" title={item.kind === "change" ? ACTOR_LABEL[item.actor_class] : undefined} data-actor-icon={item.actor_class}>
          <Icon size={14} aria-hidden="true" className={item.kind === "burst" ? "text-sev-critical" : "text-ink-300"} />
        </span>
        <div className="min-w-0 flex-1">
          <p className="text-xs text-ink-50">
            {item.kind === "burst" ? (
              <>
                Error burst from <span className="font-medium">{maskActor(item.actor)}</span>
              </>
            ) : item.kind === "routine" ? (
              <>
                {routineText(item.count ?? 0)}: {item.summary}
              </>
            ) : (
              item.summary
            )}
            {tag && (
              <span className="pill pill-warn ml-2" data-likely-tag={tag}>
                {tag === "likely" ? "Likely cause" : "Equally likely"}
              </span>
            )}
            {afterAlarm && <span className="ml-2 text-2xs text-ink-300">after the alarm</span>}
            {hidden && <span className="ml-2"><ValueHidden /></span>}
          </p>
          <p className="mt-0.5 flex flex-wrap items-center gap-x-2 gap-y-1 text-2xs text-ink-300">
            {item.kind === "burst" && (
              <span className="pill pill-bad">
                {item.error_code ?? "Error"} ×{fmtCount(item.count ?? 0)}
              </span>
            )}
            {item.kind === "group" && item.count !== null && <span>{changesText(item.count)}</span>}
            {multiAccount && (
              <span className="rounded border border-ink-500 px-1" data-account-chip>
                {accountName(view, item.account)}
              </span>
            )}
            {showRegion && item.region && (
              <span className="rounded border border-ink-500 px-1" data-region-chip>
                {item.region}
              </span>
            )}
            {pastCoverage(item, account) && account?.as_of && (
              <span className="rounded border border-dashed border-ink-400 px-1" data-as-of-chip>
                {accountName(view, item.account)}, as of {fmtClockOrDay(account.as_of, view.window.until)}
              </span>
            )}
            {item.kind !== "burst" && <span>{actor}</span>}
            {item.kind === "burst" && !multiAccount && <span>{accountName(view, item.account)}</span>}
            {item.actor_class === "unknown" && item.service && <span className="font-mono">{item.service}</span>}
            {expandable && (
              <button
                type="button"
                aria-expanded={open}
                className="text-link hover:underline"
                onClick={() => setOpen((v) => !v)}
              >
                {open ? "Collapse" : grouped ? "Expand ›" : "Details ›"}
              </button>
            )}
          </p>
          {item.kind === "burst" && (
            <p className="mt-0.5 text-2xs text-ink-300">Rule: same principal, code and service, close together in time</p>
          )}
          {item.fields.length > 0 && <ul className="mt-1 space-y-0.5">{item.fields.map((f) => <FieldLine key={f.field} f={f} />)}</ul>}
          {open && expandable && (
            <dl className="mt-1 grid grid-cols-[auto_1fr] gap-x-2 gap-y-0.5 text-2xs text-ink-200" data-item-detail>
              {item.event_name && (
                <>
                  <dt className="text-ink-300">API call</dt>
                  <dd className="font-mono" data-event-name>
                    {item.event_name}
                  </dd>
                </>
              )}
              <dt className="text-ink-300">Where</dt>
              <dd>
                {accountName(view, item.account)} {item.region}
              </dd>
              {item.resources.length > 0 && (
                <>
                  <dt className="text-ink-300">Resources</dt>
                  <dd className="min-w-0 break-all font-mono">{item.resources.join(", ")}</dd>
                </>
              )}
            </dl>
          )}
          {open && grouped && <GroupMembers id={item.id} until={view.window.until} onCursorExpired={onCursorExpired} />}
        </div>
      </div>
    </li>
  );
}

export function ChangeList({
  view,
  items,
  hasMore,
  loadingMore,
  onMore,
  showRoutine,
  onToggleRoutine,
  shownId = null,
  onCursorExpired = noop,
  stale = false,
}: {
  view: ChangesView;
  items: ChangeItem[];
  hasMore: boolean;
  loadingMore: boolean;
  onMore: () => void;
  showRoutine: boolean;
  onToggleRoutine: () => void;
  /** The row the summary card's Show › points at. */
  shownId?: string | null;
  /** A list or group cursor expired; the page reloads its first page. */
  onCursorExpired?: () => void;
  /** `items` answer another filter or span and stand in while this one loads. */
  stale?: boolean;
}) {
  const multiAccount = view.accounts.length > 1;
  const showRegion = multiRegion(view);
  const alarm = causeAlarm(view);
  const listed = items.filter((i) => showRoutine || i.kind !== "routine");
  const s = view.summary;
  const named = view.suspect.status === "found" ? (view.suspect.change_id ?? view.suspect.change?.id ?? null) : null;
  const equal = equallyLikelyIds(view);

  return (
    <section className="card flex h-full min-w-0 flex-col" aria-labelledby="changes-list-title">
      <div className="flex shrink-0 flex-wrap items-center justify-between gap-2 px-4 pb-2 pt-3">
        <h2 id="changes-list-title" className="card-title">
          Changes
        </h2>
        {s.routine > 0 && (
          <button type="button" aria-pressed={showRoutine} className="text-2xs text-link hover:underline" onClick={onToggleRoutine}>
            {showRoutine ? "Hide" : "Show"} {fmtCount(s.routine)} routine agent updates
          </button>
        )}
      </div>
      <div className="min-h-0 flex-1 overflow-y-auto border-t border-ink-500/30" data-list-scroll aria-busy={stale || undefined}>
        <ul className={clsx("divide-y divide-ink-500/30", stale && "opacity-50")} aria-label="Change list">
          {listed.map((item) => (
            <Row
              key={item.id}
              item={item}
              view={view}
              multiAccount={multiAccount}
              showRegion={showRegion}
              alarm={alarm}
              tag={item.id === named ? "likely" : equal.has(item.id) ? "equal" : null}
              shown={item.id === shownId}
              onCursorExpired={onCursorExpired}
            />
          ))}
        </ul>
        {stale && listed.length === 0 && (
          <p className="px-4 py-6 text-center text-xs text-ink-300" data-list-loading>
            Loading…
          </p>
        )}
        {!stale && !view.sync.first_read_pending && listed.length === 0 && (
          <p className="px-4 py-6 text-center text-xs text-ink-300" data-empty={view.complete ? "complete" : "incomplete"}>
            Nothing to list.
          </p>
        )}
        {hasMore && (
          <div className="border-t border-ink-500/30 px-4 py-3 text-center">
            <button type="button" className="btn" disabled={loadingMore} onClick={onMore}>
              {loadingMore ? "Loading…" : "Load more"}
            </button>
          </div>
        )}
      </div>
    </section>
  );
}
