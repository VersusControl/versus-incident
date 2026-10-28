import { useEffect, useId, useRef, useState } from "react";
import type { ReactNode } from "react";
import { ListFilter, X } from "lucide-react";
import clsx from "clsx";
import type { FacetChip, FacetView } from "@/lib/listFilters";

const SEARCHABLE_OPTION_COUNT = 8;

// FilterPanel is the "Filters" button plus its popover of facet checkboxes.
export function FilterPanel({
  facets,
  activeCount,
  onToggle,
  onClear,
  controls,
}: {
  facets: FacetView[];
  activeCount: number;
  onToggle: (facetId: string, value: string) => void;
  onClear: () => void;
  controls?: ReactNode;
}) {
  const [open, setOpen] = useState(false);
  const [panelTop, setPanelTop] = useState(0);
  const rootRef = useRef<HTMLDivElement>(null);
  const buttonRef = useRef<HTMLButtonElement>(null);
  const panelId = useId();

  useEffect(() => {
    if (!open) return;
    const updatePosition = () => {
      const buttonBounds = buttonRef.current?.getBoundingClientRect();
      if (buttonBounds) setPanelTop(buttonBounds.bottom + 4);
    };
    updatePosition();
    window.addEventListener("resize", updatePosition);
    window.addEventListener("scroll", updatePosition, true);
    return () => {
      window.removeEventListener("resize", updatePosition);
      window.removeEventListener("scroll", updatePosition, true);
    };
  }, [open]);

  useEffect(() => {
    if (!open) return;
    const onMouseDown = (e: MouseEvent) => {
      if (!rootRef.current?.contains(e.target as Node)) setOpen(false);
    };
    const onKeyDown = (e: KeyboardEvent) => {
      if (e.key !== "Escape") return;
      e.stopPropagation();
      setOpen(false);
      buttonRef.current?.focus();
    };
    document.addEventListener("mousedown", onMouseDown);
    document.addEventListener("keydown", onKeyDown, true);
    return () => {
      document.removeEventListener("mousedown", onMouseDown);
      document.removeEventListener("keydown", onKeyDown, true);
    };
  }, [open]);

  return (
    <div ref={rootRef} className="relative">
      <button
        ref={buttonRef}
        type="button"
        className={clsx("btn", activeCount > 0 && "border-accent")}
        aria-haspopup="dialog"
        aria-expanded={open}
        aria-controls={open ? panelId : undefined}
        onClick={() => setOpen((o) => !o)}
      >
        <ListFilter size={12} aria-hidden />
        Filters
        {activeCount > 0 && (
          <span className="rounded-full bg-accent px-1.5 text-2xs tabular-nums text-white">
            {activeCount}
          </span>
        )}
      </button>

      {open && (
        <div
          id={panelId}
          role="dialog"
          aria-label="Filters"
          className="fixed right-4 z-overlay w-[min(20rem,calc(100vw-2rem))] rounded-card border border-ink-500 bg-surface-raised shadow-overlay"
          style={{ top: panelTop }}
        >
          <div className="max-h-[60vh] space-y-3 overflow-auto p-3">
            {controls}
            {facets.map((f) => (
              <FacetSection key={f.id} facet={f} onToggle={onToggle} />
            ))}
          </div>
          <div className="flex items-center justify-between border-t border-ink-500/50 px-3 py-2">
            <span className="text-2xs text-ink-400">
              {activeCount > 0 ? `${activeCount} applied` : "No filters applied"}
            </span>
            <button
              type="button"
              className="btn px-2 py-1"
              disabled={activeCount === 0}
              onClick={onClear}
            >
              Clear all
            </button>
          </div>
        </div>
      )}
    </div>
  );
}

function FacetSection({
  facet,
  onToggle,
}: {
  facet: FacetView;
  onToggle: (facetId: string, value: string) => void;
}) {
  const [q, setQ] = useState("");
  // Router navigations run in a transition, so hold the clicked state until
  // the URL-backed selection catches up; otherwise the box flickers back.
  const [pending, setPending] = useState<Record<string, boolean>>({});
  const selectedKey = facet.selected.join("\u0000");
  useEffect(() => setPending({}), [selectedKey]);
  const searchable = facet.options.length > SEARCHABLE_OPTION_COUNT;
  const needle = q.trim().toLowerCase();
  const options = needle
    ? facet.options.filter((o) => o.label.toLowerCase().includes(needle))
    : facet.options;

  return (
    <fieldset className="min-w-0">
      <legend className="mb-1 text-2xs font-medium uppercase tracking-wider text-ink-300">
        {facet.label}
        {facet.selected.length > 0 && (
          <span className="ml-1 normal-case tracking-normal text-ink-400">
            ({facet.selected.length})
          </span>
        )}
      </legend>
      {searchable && (
        <input
          type="search"
          className="input mb-1 w-full py-1 text-2xs"
          placeholder={`Filter ${facet.label.toLowerCase()} values…`}
          aria-label={`Filter ${facet.label.toLowerCase()} values`}
          value={q}
          onChange={(e) => setQ(e.target.value)}
        />
      )}
      {facet.options.length === 0 ? (
        <p className="text-2xs text-ink-400">No values yet</p>
      ) : options.length === 0 ? (
        <p className="text-2xs text-ink-400">No matching values</p>
      ) : (
        <ul className="max-h-40 space-y-0.5 overflow-auto">
          {options.map((o) => {
            const checked = pending[o.value] ?? facet.selected.includes(o.value);
            return (
              <li key={o.value}>
                <label className="flex cursor-pointer items-center gap-2 rounded px-1 py-1 text-xs text-ink-200 hover:bg-ink-600/40">
                  <input
                    type="checkbox"
                    checked={checked}
                    onChange={() => {
                      setPending((p) => ({ ...p, [o.value]: !checked }));
                      onToggle(facet.id, o.value);
                    }}
                  />
                  <span className="min-w-0 flex-1 truncate" title={o.label}>
                    {o.label}
                  </span>
                  <span className="text-2xs tabular-nums text-ink-400">
                    {o.count}
                  </span>
                </label>
              </li>
            );
          })}
        </ul>
      )}
    </fieldset>
  );
}

// ActiveFilterChips lists applied facet values as removable chips.
export function ActiveFilterChips({
  chips,
  onRemove,
  onClear,
}: {
  chips: FacetChip[];
  onRemove: (facetId: string, value: string) => void;
  onClear: () => void;
}) {
  if (chips.length === 0) return null;
  return (
    <div
      className="-mt-1 mb-3 flex flex-wrap items-center gap-1.5"
      aria-label="Applied filters"
      role="group"
    >
      {chips.map((c) => (
        <span key={`${c.facetId}:${c.value}`} className="pill pill-accent pr-1">
          <span className="max-w-[16rem] truncate" title={c.label}>
            {c.label}
          </span>
          <button
            type="button"
            className="rounded-full p-0.5 hover:bg-ink-600"
            aria-label={`Remove filter ${c.label}`}
            onClick={() => onRemove(c.facetId, c.value)}
          >
            <X size={10} aria-hidden />
          </button>
        </span>
      ))}
      <button
        type="button"
        className="text-2xs text-link hover:underline"
        onClick={onClear}
      >
        Clear all
      </button>
    </div>
  );
}
