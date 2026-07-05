import { useEffect, useRef } from "react";
import { X } from "lucide-react";
import clsx from "clsx";
import type { AllState } from "@/lib/bulkSelect";

// BulkActionBar + the two checkbox controls — the ONE row-action affordance
// shared by the three learned-signal admin pages (logs, metrics, traces). There
// is no per-row ⋯ menu and no "select mode" toggle: a select-all checkbox lives
// in the table header, a checkbox on every row, and the action bar simply
// APPEARS below the toolbar when one or more rows are selected, offering every
// action applicable to the selection. Acting on a single row is just a one-row
// selection. The actions come from the RowActionId vocabulary so the two pages
// stay coherent.

// SelectAllCheckbox is the header checkbox. It renders the tri-state from
// bulkSelect.allState — indeterminate when only some visible rows are selected
// (set imperatively since React has no `indeterminate` prop).
export function SelectAllCheckbox({
  state,
  onChange,
  label = "Select all rows on this page",
  disabled,
}: {
  state: AllState;
  onChange: (checked: boolean) => void;
  label?: string;
  disabled?: boolean;
}) {
  const ref = useRef<HTMLInputElement>(null);
  useEffect(() => {
    if (ref.current) ref.current.indeterminate = state === "some";
  }, [state]);
  return (
    <input
      ref={ref}
      type="checkbox"
      className="h-4 w-4 accent-link align-middle"
      aria-label={label}
      checked={state === "all"}
      disabled={disabled}
      onChange={(e) => onChange(e.target.checked)}
      // A header checkbox click must never trigger a column sort / the row.
      onClick={(e) => e.stopPropagation()}
    />
  );
}

// RowSelectCheckbox is the per-row checkbox.
export function RowSelectCheckbox({
  checked,
  onChange,
  label,
  disabled,
}: {
  checked: boolean;
  onChange: (checked: boolean) => void;
  label: string;
  disabled?: boolean;
}) {
  return (
    <input
      type="checkbox"
      className="h-4 w-4 accent-link align-middle"
      aria-label={label}
      checked={checked}
      disabled={disabled}
      onChange={(e) => onChange(e.target.checked)}
      // Selecting a row must never open its Peek panel / navigate.
      onClick={(e) => e.stopPropagation()}
    />
  );
}

// ActionBarItem is one button in the bar. `id` is a plain string so the bar
// serves BOTH the learned-signal RowActionId vocabulary (logs / metrics /
// traces) AND the Services page's grace actions ("end" / "restart") from one
// component — the page maps the id back to its handler.
export interface ActionBarItem {
  id: string;
  label: string;
  danger?: boolean;
}

// BulkActionBar renders the selection count, one button per available action,
// and a Clear control. It is rendered by the page only when count > 0, so an
// empty selection shows nothing. When the applicable actions are hidden
// (community / viewer) the page passes an empty `actions` list, so the bar
// collapses to the count + Clear — never an empty action row that implies a
// missing permission.
export function BulkActionBar({
  count,
  actions,
  onAction,
  onClear,
  busy,
}: {
  count: number;
  actions: ActionBarItem[];
  onAction: (spec: ActionBarItem) => void;
  onClear: () => void;
  busy?: boolean;
}) {
  return (
    <div
      role="region"
      aria-label="Bulk actions"
      className="mb-3 flex flex-wrap items-center gap-2 rounded-control border border-ink-500 bg-surface-raised px-3 py-2"
    >
      <span className="text-xs font-medium tabular-nums text-ink-100">
        {count} selected
      </span>
      <div className="flex flex-wrap items-center gap-1.5">
        {actions.map((a) => (
          <button
            key={a.id}
            type="button"
            disabled={busy}
            onClick={() => onAction(a)}
            className={clsx(
              "btn",
              a.danger && "btn-danger",
            )}
          >
            {a.label}
          </button>
        ))}
      </div>
      <button
        type="button"
        className="btn ml-auto inline-flex items-center gap-1"
        onClick={onClear}
        aria-label="Clear selection"
      >
        <X size={12} aria-hidden />
        Clear
      </button>
    </div>
  );
}
