import type { ReactNode } from "react";
import clsx from "clsx";
import { ChevronDown, ChevronUp } from "lucide-react";
import type { SortDir } from "@/lib/sortRows";

// SortHeader is the click-to-sort cell for the dense agent tables (.ddt). It
// renders a real <th> (carrying aria-sort for assistive tech and the column
// width class) whose label is a keyboard-accessible button; clicking toggles
// descending ↔ ascending on that column. The chevron shows the active
// direction, or a muted down-chevron when the column is inactive so the column
// still reads as sortable.
//
// `label` is plain header content — never a nested <button> (an InfoHint is a
// button, so it is passed via `hint` and rendered OUTSIDE the sort button to
// keep the markup valid). `align="right"` matches the right-aligned numeric
// columns (Seen / Count / Freq).
export function SortHeader({
  label,
  hint,
  active,
  dir,
  onSort,
  className,
  align = "left",
}: {
  label: ReactNode;
  hint?: ReactNode;
  active: boolean;
  dir: SortDir;
  onSort: () => void;
  className?: string;
  align?: "left" | "right";
}) {
  const ariaSort = active
    ? dir === "asc"
      ? "ascending"
      : "descending"
    : "none";

  return (
    <th className={className} aria-sort={ariaSort}>
      <div
        className={clsx(
          "flex items-center gap-0.5",
          align === "right" && "justify-end",
        )}
      >
        <button
          type="button"
          onClick={onSort}
          title="Sort by this column"
          className={clsx(
            "inline-flex items-center gap-1 rounded transition-colors focus:outline-none focus-visible:text-ink-100",
            active ? "text-ink-100" : "text-ink-300 hover:text-ink-100",
          )}
        >
          <span>{label}</span>
          <SortChevron active={active} dir={dir} />
        </button>
        {hint}
      </div>
    </th>
  );
}

function SortChevron({ active, dir }: { active: boolean; dir: SortDir }) {
  if (!active) {
    // Inactive: a muted down-chevron marks the column as sortable without
    // implying a direction.
    return (
      <ChevronDown
        size={12}
        aria-hidden
        className="text-ink-500/70"
      />
    );
  }
  return dir === "asc" ? (
    <ChevronUp size={12} aria-hidden className="text-accent" />
  ) : (
    <ChevronDown size={12} aria-hidden className="text-accent" />
  );
}
