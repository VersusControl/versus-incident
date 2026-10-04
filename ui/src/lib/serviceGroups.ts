// Group-by-service view for list pages, modelled on the AWS RDS console:
// each service shows a short preview and expands on demand.

export const GROUP_PARAM = "group";
export const GROUP_BY_SERVICE = "service";
export const GROUP_PREVIEW_ROWS = 2;
// An expanded group renders in steps so a huge service never mounts thousands
// of rows at once.
export const GROUP_EXPAND_STEP = 100;
export const GROUPS_PER_PAGE = 20;

export interface ServiceGroup<T> {
  service: string;
  rows: T[];
}

export interface ServiceGroupView<T> {
  service: string;
  total: number;
  rows: T[];
  hidden: number;
  expanded: boolean;
}

// groupByService buckets rows by service in first-appearance order, so groups
// follow the active sort (the group holding the top-ranked row comes first).
export function groupByService<T>(
  rows: readonly T[],
  serviceOf: (row: T) => string,
): ServiceGroup<T>[] {
  const groups = new Map<string, T[]>();
  for (const row of rows) {
    const key = serviceOf(row);
    const bucket = groups.get(key);
    if (bucket) bucket.push(row);
    else groups.set(key, [row]);
  }
  return Array.from(groups, ([service, groupRows]) => ({
    service,
    rows: groupRows,
  }));
}

export function groupLimit(
  limits: Readonly<Record<string, number>>,
  service: string,
): number {
  return limits[service] ?? GROUP_PREVIEW_ROWS;
}

// nextGroupLimit is the limit after "show more": the first step expands the
// preview to a full step, later steps add another step.
export function nextGroupLimit(current: number, total: number): number {
  const next =
    current <= GROUP_PREVIEW_ROWS ? GROUP_EXPAND_STEP : current + GROUP_EXPAND_STEP;
  return Math.min(total, next);
}

export function groupViews<T>(
  groups: readonly ServiceGroup<T>[],
  limits: Readonly<Record<string, number>>,
): ServiceGroupView<T>[] {
  return groups.map((g) => {
    const limit = groupLimit(limits, g.service);
    const shown = g.rows.slice(0, limit);
    return {
      service: g.service,
      total: g.rows.length,
      rows: shown,
      hidden: g.rows.length - shown.length,
      expanded: limit > GROUP_PREVIEW_ROWS && g.rows.length > GROUP_PREVIEW_ROWS,
    };
  });
}
