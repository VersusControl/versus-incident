// rowActions.ts — the shared vocabulary + Active|Ignored partition logic for
// the unified checkbox action model on the three learned-signal admin pages
// (logs = PatternsPage, metrics + traces = LearnedSignalsView). Kept free of
// React / fetch so the action id set, the scope parsing, and the
// Active|Ignored partition are all unit-testable in the project's lib/*.ts +
// lib/*.test.ts pattern (the console has no DOM harness).
//
// There is ONE row-action model: the checkbox selection. The per-page action
// SETS live in bulkSelect.ts (buildLogsBulkActions / buildSignalBulkActions);
// this file owns the id enum they share and the scope helpers the pages use to
// split rows into the Active and Ignored tabs.
//
// Opening a row's detail is NOT an action: every row carries an explicit
// view/open icon (the single click affordance), and the whole-row click no
// longer opens anything.

// RowActionId enumerates every action a learned-signal row can offer. `ignore`
// and `resume` are the learn-exclusion toggle (gated on the exclude surface);
// the rest are ungated. The BulkActionSpec in bulkSelect.ts keys on this enum.
export type RowActionId =
  | "reassign"
  | "mark-known"
  | "clear-verdict"
  | "ignore"
  | "resume";

// ExclusionScope is the Active|Ignored tab selection — orthogonal to the
// verdict/status filter. "active" is the default working list (excluded rows
// removed); "ignored" is only the excluded rows, whose sole action is Resume.
export type ExclusionScope = "active" | "ignored";

export const SCOPE_PARAM = "scope";

// isExclusionScope narrows a raw URL param to a valid scope, defaulting unknown
// values to "active" so a hand-edited link never strands the operator.
export function isExclusionScope(v: string | null): ExclusionScope {
  return v === "ignored" ? "ignored" : "active";
}

// filterByScope partitions a list by the Active|Ignored tab. "ignored" keeps
// only excluded rows; "active" keeps only the rest — so an ignored row LEAVES
// the working list and finds a home under Ignored. The server stays the sole
// authority: `isExcluded` reads the same loaded policy the row cells use, this
// only re-partitions what the policy reports.
export function filterByScope<T>(
  rows: T[],
  scope: ExclusionScope,
  isExcluded: (row: T) => boolean,
): T[] {
  return rows.filter((r) => isExcluded(r) === (scope === "ignored"));
}

// countExcluded is the Ignored-tab badge source — how many of `rows` the policy
// currently holds out of learning.
export function countExcluded<T>(
  rows: T[],
  isExcluded: (row: T) => boolean,
): number {
  return rows.reduce((n, r) => (isExcluded(r) ? n + 1 : n), 0);
}
