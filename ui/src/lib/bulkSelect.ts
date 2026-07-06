// bulkSelect.ts — pure logic for the unified select-all + action model shared
// by the three learned-signal admin pages (logs = PatternsPage, metrics +
// traces = LearnedSignalsView). Kept free of React / fetch so the select-all
// tri-state, the action item set per page (and the gate that hides
// Ignore/Resume), and the selection maths are unit-testable in the project's
// lib/*.ts + lib/*.test.ts pattern (the node vitest env).
//
// Design: the CHECKBOX selection is the ONE row-action model — there is no
// separate per-row ⋯ menu. A select-all checkbox lives in the table header and
// a per-row checkbox on every row; the action bar simply APPEARS below the
// toolbar when one or more rows are selected, offering EVERY action applicable
// to the selection (relabel + Ignore/Resume + Assign-to-service on logs,
// Ignore/Resume + Assign-to-service on metrics/traces). Selecting a single row
// and acting on it is just a one-row selection. Selection resets on tab /
// filter / page change (the page keys the hook on the same reset signature
// usePagination uses, plus the page number).

import type { RowActionId } from "@/lib/rowActions";
import type { ExclusionScope } from "@/lib/rowActions";

// ----- selection maths ------------------------------------------------------

// withToggled returns a NEW set with `key` flipped in/out — the per-row
// checkbox handler. The input set is never mutated.
export function withToggled(
  set: ReadonlySet<string>,
  key: string,
): Set<string> {
  const next = new Set(set);
  if (next.has(key)) next.delete(key);
  else next.add(key);
  return next;
}

// withAll returns a NEW set that adds (checked) or removes (unchecked) EVERY
// key in `keys` — the header select-all handler. It only touches the given
// keys, so a selection carried across a boundary (should one ever leak) is
// preserved for rows not on the current page. The input set is never mutated.
export function withAll(
  set: ReadonlySet<string>,
  keys: readonly string[],
  checked: boolean,
): Set<string> {
  const next = new Set(set);
  for (const k of keys) {
    if (checked) next.add(k);
    else next.delete(k);
  }
  return next;
}

// AllState is the tri-state of the header select-all checkbox against the
// currently visible keys: "none" (unchecked), "some" (indeterminate), "all"
// (checked).
export type AllState = "none" | "some" | "all";

// allState reports the header checkbox tri-state. An empty visible list is
// "none" (nothing to select). Otherwise it is "all" only when every visible key
// is selected, "some" when at least one but not all are.
export function allState(
  set: ReadonlySet<string>,
  keys: readonly string[],
): AllState {
  if (keys.length === 0) return "none";
  let n = 0;
  for (const k of keys) if (set.has(k)) n++;
  if (n === 0) return "none";
  return n === keys.length ? "all" : "some";
}

// selectedInView returns the selected keys that are currently visible, in the
// visible order — what a bulk action actually operates on (a bulk action never
// touches a row that isn't on screen).
export function selectedInView(
  set: ReadonlySet<string>,
  keys: readonly string[],
): string[] {
  return keys.filter((k) => set.has(k));
}

// ----- bulk-action item sets ------------------------------------------------

// BulkActionSpec is the presentation-free description of one action in the bar
// — the same shape and ids the pages route their handlers by (single row =
// one-row selection, multi-select = many). Its `id` is a RowActionId so the
// vocabulary stays one coherent set across both pages.
export interface BulkActionSpec {
  id: RowActionId;
  label: string;
  danger?: boolean;
}

// REASSIGN_ACTION is the shared "Assign to service" (attribution correction)
// action. It opens the ReassignModal for the selected rows rather than firing
// immediately, so the page special-cases it in onBulkAction.
const REASSIGN_ACTION: BulkActionSpec = {
  id: "reassign",
  label: "Assign to service",
};

// buildLogsBulkActions returns the actions for a LOGS selection.
// Active scope: relabel (Mark known / Clear verdict, ungated) + Ignore (gated
// on the exclude surface) + Assign-to-service. Ignored scope: Resume (gated) +
// Assign-to-service. Assign-to-service is ALWAYS offered: log attribution
// override is an OSS capability (the server authorizes the write), the same way
// the ungated relabel actions are always present. The Ignore/Resume grain is
// the PATTERN.
export function buildLogsBulkActions(input: {
  scope: ExclusionScope;
  excludeVisible: boolean;
}): BulkActionSpec[] {
  if (input.scope === "ignored") {
    const items: BulkActionSpec[] = [];
    if (input.excludeVisible)
      items.push({ id: "resume", label: "Resume learning" });
    items.push(REASSIGN_ACTION);
    return items;
  }
  const items: BulkActionSpec[] = [
    { id: "mark-known", label: "Mark known" },
    { id: "clear-verdict", label: "Clear verdict" },
  ];
  if (input.excludeVisible)
    items.push({ id: "ignore", label: "Ignore", danger: true });
  items.push(REASSIGN_ACTION);
  return items;
}

// buildSignalBulkActions returns the actions for a METRICS / TRACES selection.
// Baselines are read-only, so the only actions are the gated exclude toggle
// (Ignore in Active, Resume in Ignored) plus Assign-to-service — and ALL of
// them require the licensed runtime:manage surface (metric/trace attribution
// override is enterprise-only, unlike the OSS log override). So the whole set
// vanishes when the exclude surface is not visible (community / viewer),
// leaving an empty set (no checkbox column, no bar).
export function buildSignalBulkActions(input: {
  scope: ExclusionScope;
  excludeVisible: boolean;
}): BulkActionSpec[] {
  if (!input.excludeVisible) return [];
  const items: BulkActionSpec[] = [
    input.scope === "ignored"
      ? { id: "resume", label: "Resume learning" }
      : { id: "ignore", label: "Ignore", danger: true },
  ];
  items.push(REASSIGN_ACTION);
  return items;
}
