import { ChevronLeft, ChevronRight } from "lucide-react";
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

export function CursorPagination({ state, next, className }: { state: CursorPaginationState; next?: string; className?: string }) {
  return (
    <nav aria-label="Page navigation" className={clsx("flex items-center justify-between gap-2 border-t border-ink-500/40 px-3 py-2 text-2xs text-ink-300", className)}>
      <span className="tabular-nums">Page {state.page}</span>
      <div className="flex items-center gap-1">
        <button type="button" className="btn px-2 py-1" disabled={!state.canPrevious} onClick={state.previousPage} aria-label="Previous page">
          <ChevronLeft size={12} aria-hidden /> Prev
        </button>
        <button type="button" className="btn px-2 py-1" disabled={!next} onClick={() => state.nextPage(next)} aria-label="Next page">
          Next <ChevronRight size={12} aria-hidden />
        </button>
      </div>
    </nav>
  );
}
