import { useCallback, useEffect, useRef, useState } from "react";
import { useSearchParams, Link } from "react-router-dom";
import { keepPreviousData, useInfiniteQuery, useQuery, useQueryClient, type InfiniteData } from "@tanstack/react-query";
import { Info, Search, Sparkles, X } from "lucide-react";
import { TopBar } from "@/components/TopBar";
import { Breadcrumb } from "@/components/Breadcrumb";
import { Dropdown } from "@/components/Dropdown";
import { RetryableError } from "@/components/RetryableError";
import { SkCard } from "@/components/Skeleton";
import { api } from "@/lib/api";
import {
  appendUnique,
  DEFAULT_COMPARE,
  fmtSpanOrDay,
  pollInterval,
  fmtClockOrDay,
  gapReasonText,
  isCursorExpired,
  isWindowLocked,
  notEnabledRegions,
  WINDOW_LABEL,
  type ChangesQuery,
  type ChangesView,
  type ChangesWindow,
} from "@/lib/awsChanges";
import { ActivityGrid } from "./ActivityGrid";
import { ChangeList } from "./ChangeList";
import { ComparePanel } from "./ComparePanel";
import { SummaryCard } from "./SummaryCard";

// AWS / Changes: who changed what in the window, which change is the likely
// cause of an alarm, and how one time differs from another. Read-only: no
// control here acts on AWS. The page never claims "no changes" while data in
// the window is missing; the summary card's warning line says what is.

/**
 * The Changes and Compare cards share one height: the viewport below the
 * Activity card, held between 480 and 720 px. Each scrolls inside. Stacked
 * on narrow screens, each keeps the same cap.
 */
export const PANEL_HEIGHT = "h-[clamp(480px,calc(100dvh_-_6rem),720px)]";

const PAGE_SIZE = 50;
const MOVED_ON_NOTE_MS = 60_000;

/**
 * The span chip's words, from the URL's `since`/`until` so the chip names
 * the asked span from the first frame. An open end runs to now.
 */
function spanText(since: string, until: string | null, sep: string): string {
  const end = until ?? new Date().toISOString();
  return fmtSpanOrDay(since, end, end, sep);
}

function useChangesParams() {
  const [params, setParams] = useSearchParams();
  const asked = params.get("window");
  const locked = asked !== null && asked !== "1h" && asked !== "24h";
  const window: ChangesWindow = asked === "1h" ? "1h" : "24h";
  const since = params.get("since") ?? undefined;
  const q: ChangesQuery = {
    window,
    since,
    until: params.get("until") ?? undefined,
    q: params.get("q") ?? undefined,
    account: params.get("account") ?? undefined,
    region: params.get("region") ?? undefined,
    show_routine: params.get("routine") === "1",
    limit: PAGE_SIZE,
  };
  const set = (k: string, v: string | null, clear: string[] = []) => {
    const next = new URLSearchParams(params);
    for (const c of clear) next.delete(c);
    if (v === null || v === "") next.delete(k);
    else next.set(k, v);
    setParams(next, { replace: true });
  };
  return { q, window, custom: Boolean(since), locked, params, set };
}

/** Free-text search sent as `q`; matches summaries and raw API names such as `s3:PutBucketPolicy`. */
function SearchBox({ value, onSearch }: { value: string; onSearch: (q: string) => void }) {
  const [text, setText] = useState(value);
  return (
    <form
      role="search"
      className="inline-flex items-center gap-1.5"
      onSubmit={(e) => {
        e.preventDefault();
        onSearch(text.trim());
      }}
    >
      <Search size={12} aria-hidden="true" className="text-ink-300" />
      <input
        type="search"
        className="input h-7 w-56 text-xs"
        aria-label="Search changes"
        placeholder="Search, e.g. s3:PutBucketPolicy"
        value={text}
        onChange={(e) => setText(e.target.value)}
      />
    </form>
  );
}

/** "complete", "partly unknown", "not enabled": a Region's state, as its dropdown option names it. */
function regionState(r: ChangesView["regions"][number]): string {
  if (r.state === "not_enabled") return "not enabled";
  return r.complete ? "complete" : "partly unknown";
}

/**
 * The Accounts and Regions pickers. Their options come from `scope`, the
 * answer without the account and Region filters, so every configured account
 * and Region stays one choice away whatever is chosen; "All" clears a filter.
 */
function ScopeBar({
  scope,
  account,
  region,
  set,
  children,
}: {
  scope: ChangesView;
  account: string;
  region: string;
  set: (k: string, v: string | null) => void;
  children?: React.ReactNode;
}) {
  const accounts = scope.accounts;
  // A gap over the whole window is named by its reason, not by its ends.
  const coversWindow = (g: { from: string; to: string }) =>
    Date.parse(g.from) <= Date.parse(scope.window.since) && Date.parse(g.to) >= Date.parse(scope.window.until);
  const accountOptions = [
    { value: "", label: `All ${accounts.length}` },
    ...accounts.map((a) => {
      const gaps = scope.gaps.filter((g) => g.account === a.account && g.reason !== "arriving");
      const state = a.complete
        ? "complete"
        : a.as_of
          ? `as of ${fmtClockOrDay(a.as_of, scope.window.until)}`
          : gaps[0] && !coversWindow(gaps[0])
            ? `gap ${fmtSpanOrDay(gaps[0].from, gaps[0].to, scope.window.until)}`
            : gapReasonText(gaps[0]?.reason ?? a.reason);
      return { value: a.account, label: `${a.name || a.account} (${a.account}) — ${state}` };
    }),
    ...(account && !accounts.some((a) => a.account === account) ? [{ value: account, label: account }] : []),
  ];
  // Every Region in scope comes from the server, not-enabled ones included;
  // each option names its state, as the Accounts options do.
  const known = scope.regions ?? [];
  const notEnabled = notEnabledRegions(scope);
  const all: ChangesView["regions"] = [
    ...known,
    ...notEnabled.filter((r) => !known.some((k) => k.region === r)).map((r) => ({ region: r, state: "not_enabled" as const, complete: false })),
    ...(region && !known.some((k) => k.region === region) && !notEnabled.includes(region) ? [{ region, state: "selected" as const, complete: true }] : []),
  ];
  const order = (r: ChangesView["regions"][number]) => (r.state === "not_enabled" ? 2 : r.state === "global" || r.region === "global" ? 1 : 0);
  const regions = [...all].sort((a, b) => order(a) - order(b) || a.region.localeCompare(b.region));
  const readable = regions.filter((r) => r.state !== "not_enabled");
  return (
    <div className="flex flex-wrap items-center gap-x-4 gap-y-2 border-b border-ink-500/40 pb-3 text-xs text-ink-300">
      {/* Below `sm` the control is held to the screen width and its options
          wrap, so the state after the dash stays readable. */}
      <span className="flex w-full min-w-0 max-w-full items-center gap-1.5 sm:w-auto" data-accounts-filter>
        <span aria-hidden="true">Accounts</span>
        <Dropdown
          value={account}
          options={accountOptions}
          onChange={(v) => set("account", v)}
          aria-label="Accounts"
          wrapOptions
          className="min-w-0 max-w-full flex-1 sm:w-[26rem] sm:flex-none"
        />
      </span>
      <span className="inline-flex min-w-0 max-w-full items-center gap-1.5" data-regions-filter>
        <span aria-hidden="true">Regions</span>
        {/* The open list may grow wider than the button so each Region's
            state reads in full; it wraps rather than leave the screen. */}
        <Dropdown
          value={region}
          options={[
            { value: "", label: `${readable.length} ${readable.length === 1 ? "region" : "regions"}` },
            ...regions.map((r) => ({
              value: r.region,
              label: `${r.region === "global" ? "Global (IAM, …)" : r.region} — ${regionState(r)}`,
            })),
          ]}
          onChange={(v) => set("region", v)}
          aria-label="Regions"
          wrapOptions
          className="min-w-[9rem] max-w-full"
          listClassName="w-max min-w-full max-w-[min(20rem,calc(100vw-2rem))]"
        />
      </span>
      <div className="flex flex-wrap items-center gap-3 sm:ml-auto">{children}</div>
    </div>
  );
}

export function AwsChangesPage() {
  const { q, window, custom, locked, params, set } = useChangesParams();
  const list = useInfiniteQuery({
    queryKey: ["aws-changes", q],
    queryFn: ({ pageParam }) => api.awsChanges({ ...q, cursor: pageParam || undefined }),
    initialPageParam: "",
    getNextPageParam: (last) => last.next_cursor || undefined,
    // Follow the collector while the page is open; faster during the first
    // read or an arriving tail. Paused while the tab is hidden. Refetching in
    // place keeps scroll position and expanded rows.
    refetchInterval: (query) => pollInterval(query.state.data?.pages[0]),
    refetchIntervalInBackground: false,
    // A new filter keeps the current page on screen until its answer lands,
    // so the scope bar and the picker the user just used stay mounted.
    placeholderData: keepPreviousData,
  });
  const view = list.data?.pages[0];
  // The pickers list every account and Region, not only the filtered ones,
  // so with a filter set they read their options from the unfiltered answer.
  const filtered = Boolean(q.account || q.region);
  const scopeQuery = useQuery({
    queryKey: ["aws-changes-scope", { window: q.window, since: q.since, until: q.until }],
    queryFn: () => api.awsChanges({ window: q.window, since: q.since, until: q.until, limit: 1 }),
    enabled: filtered,
    placeholderData: keepPreviousData,
    refetchInterval: (query) => pollInterval(query.state.data),
    refetchIntervalInBackground: false,
  });
  // Until that answer lands, the last unfiltered page of the same window
  // stands in, so the options do not shrink right after a choice.
  const scopeKey = JSON.stringify([q.window, q.since, q.until]);
  const lastUnfiltered = useRef<{ key: string; view: ChangesView } | null>(null);
  if (!filtered && view && !list.isPlaceholderData) lastUnfiltered.current = { key: scopeKey, view };
  const remembered = lastUnfiltered.current?.key === scopeKey ? lastUnfiltered.current.view : undefined;
  const scope = filtered ? (scopeQuery.data ?? remembered ?? view) : view;
  // The span the answer on screen was read for. While a new span loads the
  // previous answer stays as a placeholder; its numbers are not this span's.
  const shownSpan = useRef<string | null>(null);
  if (view && !list.isPlaceholderData) shownSpan.current = scopeKey;
  const staleSpan = Boolean(view) && list.isPlaceholderData && shownSpan.current !== scopeKey;
  const items = list.data ? appendUnique(list.data.pages.map((p) => p.items)) : [];
  const lockedByServer = list.isError && isWindowLocked(list.error);
  const qc = useQueryClient();
  // When the list last moved on, and for which filters. The note shows only
  // for those filters, and only until a read that lands about 60 s later.
  const [movedOn, setMovedOn] = useState<{ key: string; at: number } | null>(null);
  const refetchList = list.refetch;
  const keyRef = useRef(q);
  keyRef.current = q;
  // A cursor whose window left the 24 h tier: drop the later pages and
  // quietly re-read the first, with a one-line note instead of an error.
  const reloadFirstPage = useCallback(() => {
    qc.setQueryData<InfiniteData<ChangesView, string>>(["aws-changes", keyRef.current], (d) =>
      d ? { pages: d.pages.slice(0, 1), pageParams: d.pageParams.slice(0, 1) } : d,
    );
    setMovedOn({ key: JSON.stringify(keyRef.current), at: Date.now() });
    void refetchList();
  }, [qc, refetchList]);
  const expired = list.isError && isCursorExpired(list.error);
  // Any path that hits an expired cursor (Show more or a background poll
  // refetching a later page) ends in the same quiet first-page reload.
  const errorAt = list.errorUpdatedAt;
  useEffect(() => {
    if (expired) reloadFirstPage();
  }, [expired, errorAt, reloadFirstPage]);
  const showMovedOn =
    movedOn !== null && movedOn.key === JSON.stringify(q) && list.dataUpdatedAt < movedOn.at + MOVED_ON_NOTE_MS;
  const showLockNote = locked || lockedByServer;
  // The row the summary card's Show › points at: the list scrolls to it and
  // the row and its grid cell are outlined.
  const [shownId, setShownId] = useState<string | null>(null);
  const show = (id: string) => {
    setShownId(id);
    const row = [...document.querySelectorAll("[data-list-scroll] [data-item]")].find((e) => e.getAttribute("data-item") === id);
    if (row) row.scrollIntoView({ block: "nearest", behavior: "smooth" });
    else document.querySelector("[data-suspect]")?.scrollIntoView({ block: "nearest", behavior: "smooth" });
  };

  const headerRight = (
    <div className="flex flex-wrap items-center gap-2">
      <Link to="/agent/chat" className="btn border-accent text-ink-50">
        <Sparkles size={12} aria-hidden="true" />
        Ask AI
      </Link>
    </div>
  );

  let body: React.ReactNode;
  if (list.isPending) {
    body = <SkCard lines={6} />;
  } else if (list.isError && !lockedByServer && !(expired && view)) {
    body = <RetryableError error={list.error} onRetry={() => void list.refetch()} retrying={list.isFetching} context="Couldn't load changes" />;
  } else if (view) {
    body = (
      <div className="space-y-4">
        <ScopeBar scope={scope ?? view} account={q.account ?? ""} region={q.region ?? ""} set={set}>
          <SearchBox key={q.q ?? ""} value={q.q ?? ""} onSearch={(text) => set("q", text)} />
          {custom && (
            <span className="pill inline-flex items-center gap-1" data-span>
              {spanText(q.since ?? view.window.since, q.until ?? (staleSpan ? null : view.window.until), " – ")}
              <button type="button" aria-label="Clear span" className="text-ink-300 hover:text-ink-50" onClick={() => set("since", null, ["until"])}>
                <X size={11} aria-hidden="true" />
              </button>
            </span>
          )}
        </ScopeBar>
        <SummaryCard
          view={view}
          window={window}
          custom={custom}
          locked={showLockNote}
          onWindow={(w) => set("window", w === "24h" ? null : w, ["since", "until"])}
          onShow={show}
          stale={staleSpan}
        />
        {showLockNote && (
          <p className="flex items-center gap-1.5 text-2xs text-ink-300" data-lock-note>
            <Info size={12} aria-hidden="true" />
            The 7 day window and longer change history are part of Enterprise; this page shows the last {WINDOW_LABEL[window]}.
            <a href="https://versusincident.com/enterprise" target="_blank" rel="noreferrer" className="text-link hover:underline">
              About Enterprise
            </a>
          </p>
        )}
        {showMovedOn && (
          <p className="text-2xs text-ink-200" data-moved-on role="status">
            The list moved on; showing the latest changes.
          </p>
        )}
        <ActivityGrid view={view} stale={staleSpan} shown={shownId !== null && shownId === (view.suspect.change_id ?? view.suspect.change?.id)} />
        <div className="grid gap-4 lg:grid-cols-5" data-panels>
          <div className={`min-w-0 lg:col-span-3 ${PANEL_HEIGHT}`} data-panel="changes">
            <ChangeList
              view={view}
              items={items}
              hasMore={Boolean(list.hasNextPage)}
              loadingMore={list.isFetchingNextPage}
              onMore={() => void list.fetchNextPage()}
              onCursorExpired={reloadFirstPage}
              showRoutine={q.show_routine ?? false}
              onToggleRoutine={() => set("routine", q.show_routine ? null : "1")}
              shownId={shownId}
            />
          </div>
          <div className={`min-w-0 lg:col-span-2 ${PANEL_HEIGHT}`} data-panel="compare">
            <ComparePanel
              view={view}
              value={params.get("from") ?? DEFAULT_COMPARE}
              onChange={(v) => set("from", v === DEFAULT_COMPARE ? null : v)}
              to={params.get("to") ?? ""}
              onToChange={(v) => set("to", v)}
              scope={{ account: q.account, region: q.region }}
            />
          </div>
        </div>
      </div>
    );
  } else {
    body = (
      <p className="flex items-center gap-1.5 text-xs text-ink-300" data-lock-note>
        <Info size={12} aria-hidden="true" />
        The 7 day window and longer change history are part of Enterprise.
        <button type="button" className="text-link hover:underline" onClick={() => set("window", null)}>
          Show the last 24 h
        </button>
      </p>
    );
  }

  return (
    <main className="flex min-h-0 flex-1 flex-col overflow-auto">
      <TopBar title="Changes" subtitle="AWS" actions={headerRight} />
      <div className="mx-auto w-full max-w-7xl space-y-3 p-4 lg:p-6">
        <Breadcrumb items={[{ label: "AWS", to: "/aws" }, { label: "Changes" }]} />
        {body}
      </div>
    </main>
  );
}
