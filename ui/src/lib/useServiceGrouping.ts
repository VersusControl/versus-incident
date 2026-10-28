import { useCallback, useMemo, useState } from "react";
import { useSearchParams } from "react-router-dom";
import { usePagination } from "@/lib/pagination";
import {
  GROUP_PARAM,
  GROUP_PREVIEW_ROWS,
  GROUPS_PER_PAGE,
  groupByService,
  groupLimit,
  groupViews,
  nextGroupLimit,
  type ServiceGroupView,
} from "@/lib/serviceGroups";

// useServiceGrouping drives the "By service" view: it reads ?group= from the
// URL, paginates by service, and tracks how many rows each group shows.
// `serviceOf` must be stable.
export function useServiceGrouping<T>(
  rows: T[],
  serviceOf: (row: T) => string,
  resetKey: string,
) {
  const [params] = useSearchParams();
  const group = params.get(GROUP_PARAM);
  const grouped = group !== "flat";

  const groups = useMemo(
    () => (grouped ? groupByService(rows, serviceOf) : []),
    [grouped, rows, serviceOf],
  );
  const pagination = usePagination(groups, {
    pageSize: GROUPS_PER_PAGE,
    resetKey,
  });

  const [limits, setLimits] = useState<Record<string, number>>({});
  const views = useMemo(
    () => groupViews(pagination.pageItems, limits),
    [pagination.pageItems, limits],
  );
  const visibleRows = useMemo(() => views.flatMap((v) => v.rows), [views]);

  const toggle = useCallback((g: ServiceGroupView<T>) => {
    setLimits((l) => ({
      ...l,
      [g.service]: g.expanded
        ? GROUP_PREVIEW_ROWS
        : nextGroupLimit(groupLimit(l, g.service), g.total),
    }));
  }, []);

  const showMore = useCallback((g: ServiceGroupView<T>) => {
    setLimits((l) => ({
      ...l,
      [g.service]: nextGroupLimit(groupLimit(l, g.service), g.total),
    }));
  }, []);

  return { grouped, pagination, views, visibleRows, toggle, showMore };
}
