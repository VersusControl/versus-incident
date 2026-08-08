import type { ReactNode } from "react";
import clsx from "clsx";

// FilterBar — the one filter row every list page shares. It enforces a single
// canonical left-to-right order so users find filters in the same place on
// every page: tabs → search → actions. Each page used to hand-roll this row
// (`mb-3 flex flex-wrap items-center gap-2`) and arrange the slots differently.
//
// Slots are all optional: tabs render first (one or more SegmentedControls),
// search flexes to fill the middle, and actions are pushed to the right edge.
// On narrow widths the row wraps, keeping tabs above search.
export function FilterBar({
  tabs,
  search,
  actions,
  className,
}: {
  tabs?: ReactNode;
  search?: ReactNode;
  actions?: ReactNode;
  className?: string;
}) {
  return (
    <div className={clsx("mb-3 flex flex-wrap items-center gap-2", className)}>
      {tabs}
      {search}
      {actions != null && (
        <div className="ml-auto flex items-center gap-2">{actions}</div>
      )}
    </div>
  );
}
