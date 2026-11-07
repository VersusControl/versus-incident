import { ChevronLeft, ChevronRight } from "lucide-react";
import { useState } from "react";
import clsx from "clsx";
import type { CursorPaginationState, PaginationState } from "@/lib/pagination";

// Pagination — the single client-side pager wired into every agent admin
// table. Page size is fixed at 100 (PAGE_SIZE): the founder hit a 2000+ row
// table that froze the browser, so only the current page's rows ever render.
// The slicing + page state lives in usePagination (@/lib/pagination); this
// component only renders the "1–100 of 2,314" indicator plus prev/next, and
// renders nothing when everything fits on one page so small tables stay clean.
export function Pagination({
  state,
  noun,
  className,
}: {
  state: PaginationState;
  // noun names what is counted when it isn't rows, e.g. "services".
  noun?: string;
  className?: string;
}) {
  const { page, pageCount, total, start, end, setPage } = state;
  if (pageCount <= 1) return null;

  const fmt = (n: number) => n.toLocaleString();
  const displayStart = total === 0 ? 0 : start + 1;

  return (
    <div
      className={clsx(
        "flex items-center justify-between gap-2 border-t border-ink-500/40 px-3 py-2 text-2xs text-ink-300",
        className,
      )}
    >
      <span className="tabular-nums">
        {fmt(displayStart)}–{fmt(end)} of {fmt(total)}
        {noun ? ` ${noun}` : null}
      </span>
      <div className="flex items-center gap-1">
        <button
          type="button"
          className="btn px-2 py-1"
          disabled={page <= 1}
          onClick={() => setPage(page - 1)}
          aria-label="Previous page"
        >
          <ChevronLeft size={12} aria-hidden /> Prev
        </button>
        <span className="px-1 tabular-nums text-ink-200">
          Page {fmt(page)} / {fmt(pageCount)}
        </span>
        <button
          type="button"
          className="btn px-2 py-1"
          disabled={page >= pageCount}
          onClick={() => setPage(page + 1)}
          aria-label="Next page"
        >
          Next <ChevronRight size={12} aria-hidden />
        </button>
      </div>
    </div>
  );
}

/** A bounded history count reads at most this many items plus one. */
export const COUNT_CAP = 1000;

/**
 * Pager for server-paged lists. With a known total it reads like the client
 * pager ("51–100 of 312 · Page 2 / 7"). A capped count arrives as the number
 * of rows read, at most 1,001, and reads "1,000+" with "Page 2 / 20+". Lists
 * that count only on their first page keep that count while paging. Without a
 * total it shows "Page N". A range runs over the items each page actually
 * showed. A page with no items shows no range, so an offset past a list that
 * shrank never names an item that does not exist.
 */
export function CursorPagination({ state, next, className, busy = false, total, limit, capped = false, count }: {
  state: CursorPaginationState;
  next?: string;
  className?: string;
  busy?: boolean;
  /** Items matching the filters, when the server reports them. */
  total?: number;
  /** Page size; a total is shown only with a positive limit. */
  limit?: number;
  /** The total is a lower bound from a bounded count. */
  capped?: boolean;
  /** Items on the current page. */
  count?: number;
}) {
  const current = limit && limit > 0 && total !== undefined && total > 0 ? { total, capped } : undefined;
  const [firstPage, setFirstPage] = useState(current);
  if (state.page === 1 && (firstPage?.total !== current?.total || firstPage?.capped !== current?.capped)) setFirstPage(current);
  const known = current ?? (state.page > 1 ? firstPage : undefined);
  // Items each page showed, by page. A page can hold fewer than the page size
  // and still have a next page (a response trimmed for size), so later ranges
  // start after the items actually shown, not after full pages. Paging moves
  // one page at a time, so every earlier page has been seen; one that was not
  // is assumed full.
  const [shownPerPage, setShownPerPage] = useState<number[]>([]);
  if (count !== undefined && shownPerPage[state.page - 1] !== count) {
    const updated = state.page === 1 ? [] : [...shownPerPage];
    updated[state.page - 1] = count;
    setShownPerPage(updated);
  }
  // A list with one page has nowhere to go: "Page 1" beside two disabled
  // buttons is a control that cannot act, so the pager only appears once
  // another page exists in either direction.
  if (!next && !state.canPrevious) return null;
  const fmt = (value: number) => value.toLocaleString();
  const size = limit && limit > 0 ? limit : 0;
  const plus = known?.capped ? "+" : "";
  // The server reads one row past the cap to know the count is capped; the
  // reader is told "1,000+", never the extra row.
  const shown = known ? (known.capped ? Math.min(known.total, COUNT_CAP) : known.total) : 0;
  const pageCount = known && size ? Math.max(state.page, Math.ceil(shown / size)) : 0;
  let start = 1;
  for (let page = 1; page < state.page; page++) start += shownPerPage[page - 1] ?? size;
  const items = count ?? (known && !known.capped ? Math.min(size, shown - start + 1) : size);
  const end = start + items - 1;
  // An uncapped total kept from the first page can trail a list that grew;
  // the range then stands alone rather than claiming "41–60 of 50".
  const totalText = known && (known.capped || end <= shown) ? ` of ${fmt(shown)}${plus}` : "";
  return (
    <nav aria-label="Page navigation" className={clsx("flex items-center justify-between gap-2 border-t border-ink-500/40 px-3 py-2 text-2xs text-ink-300", className)}>
      {known && size ? (
        <span className="tabular-nums">
          {items > 0 ? `${fmt(start)}–${fmt(end)}${totalText}` : "No results on this page"}
        </span>
      ) : <span className="tabular-nums">Page {fmt(state.page)}</span>}
      <div className="flex items-center gap-1">
        <button type="button" className="btn px-2 py-1" disabled={busy || !state.canPrevious} onClick={state.previousPage} aria-label="Previous page">
          <ChevronLeft size={12} aria-hidden /> Prev
        </button>
        {known && size ? (
          <span className="px-1 tabular-nums text-ink-200">
            Page {fmt(state.page)} / {fmt(pageCount)}{plus}
          </span>
        ) : null}
        <button type="button" className="btn px-2 py-1" disabled={busy || !next || next === state.cursor} onClick={() => state.nextPage(next)} aria-label="Next page">
          Next <ChevronRight size={12} aria-hidden />
        </button>
      </div>
    </nav>
  );
}
