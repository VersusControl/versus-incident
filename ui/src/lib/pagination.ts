// Pure pagination math shared by the reusable <Pagination> component and its
// usePagination hook. The pageSlice helper stays React-free so it is
// unit-testable in the node vitest environment (like serviceOverride.ts). The
// founder hit a 2000+ row agent table that froze the UI — every agent admin
// table slices its filtered/sorted rows through usePagination so only one
// PAGE_SIZE window renders.

import { useEffect, useMemo, useState } from "react";

export const PAGE_SIZE = 100;

export interface PageSlice {
  // page is the current 1-based page, always clamped to [1, pageCount].
  page: number;
  pageCount: number;
  pageSize: number;
  total: number;
  // start/end are zero-based slice bounds: items.slice(start, end).
  start: number;
  end: number;
}

export interface PaginationState extends PageSlice {
  setPage: (page: number) => void;
}

// pageSlice resolves the current window for a `total`-length list. It clamps a
// stale/out-of-range page (e.g. the list shrank under the cursor after a
// filter) down to the last real page and never returns page < 1.
export function pageSlice(
  total: number,
  page: number,
  pageSize: number = PAGE_SIZE,
): PageSlice {
  const size = Math.max(1, Math.floor(pageSize));
  const count = Math.max(0, Math.floor(total));
  const pageCount = Math.max(1, Math.ceil(count / size));
  const clamped = Math.min(Math.max(1, Math.floor(page) || 1), pageCount);
  const start = (clamped - 1) * size;
  const end = Math.min(start + size, count);
  return { page: clamped, pageCount, pageSize: size, total: count, start, end };
}

// usePagination slices a filtered/sorted array into fixed-size pages. It owns
// the 1-based page in local state, clamps it when the list shrinks under the
// cursor, and resets to page 1 whenever `resetKey` changes — wire the active
// search/filter/sort signature into resetKey so changing a filter never
// strands the user on a now-empty page. Pagination is applied AFTER
// search/filter/sort: pass the already-filtered/sorted array in.
export function usePagination<T>(
  items: T[],
  opts?: { pageSize?: number; resetKey?: unknown },
): PaginationState & { pageItems: T[] } {
  const pageSize = opts?.pageSize ?? PAGE_SIZE;
  const resetKey = opts?.resetKey;
  const [page, setPage] = useState(1);

  // Reset to page 1 when the filter/search/sort signature changes.
  useEffect(() => {
    setPage(1);
  }, [resetKey]);

  const slice = pageSlice(items.length, page, pageSize);

  // Keep the stored page in sync with the clamped value so prev/next always
  // step from the real current page after the list shrinks.
  useEffect(() => {
    if (page !== slice.page) setPage(slice.page);
  }, [page, slice.page]);

  const pageItems = useMemo(
    () => items.slice(slice.start, slice.end),
    [items, slice.start, slice.end],
  );

  return { ...slice, setPage, pageItems };
}
