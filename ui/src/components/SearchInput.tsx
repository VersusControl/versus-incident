import { useRef } from "react";
import { Search, X } from "lucide-react";
import clsx from "clsx";

// SearchInput — the one search box every list page shares. It replaces the
// hand-rolled `Search icon + input pl-7` block that each page used to inline.
// Controlled and debounce-free: callers already own their `useDebounced`.
//
// The `/`-to-focus global shortcut (useShortcuts in lib/hooks) finds the box
// via [data-page-search]; we keep that marker when `shortcut` is on and render
// a subtle `/` kbd hint instead of stuffing "( / )" into placeholder text — so
// placeholders stay clean and the hint is consistent everywhere.
export function SearchInput({
  value,
  onChange,
  placeholder,
  ariaLabel,
  shortcut = true,
  className,
  "data-testid": dataTestid,
}: {
  value: string;
  onChange: (v: string) => void;
  placeholder: string;
  ariaLabel?: string;
  shortcut?: boolean;
  className?: string;
  "data-testid"?: string;
}) {
  const ref = useRef<HTMLInputElement>(null);
  const label = ariaLabel ?? placeholder;

  return (
    <div className={clsx("relative", className ?? "max-w-md flex-1")}>
      <Search
        size={12}
        aria-hidden
        className="absolute left-2.5 top-1/2 -translate-y-1/2 text-ink-400"
      />
      <input
        ref={ref}
        className={clsx("input pl-7", (shortcut || value) && "pr-8")}
        aria-label={label}
        placeholder={placeholder}
        value={value}
        data-testid={dataTestid}
        data-page-search={shortcut ? "" : undefined}
        onChange={(e) => onChange(e.target.value)}
      />
      {/* Clear takes the right slot whenever there's text; the kbd hint shows
          only in the empty state, so the two never collide. */}
      {value ? (
        <button
          type="button"
          aria-label="Clear search"
          className="absolute right-2 top-1/2 -translate-y-1/2 rounded p-0.5 text-ink-400 hover:text-ink-100"
          onClick={() => {
            onChange("");
            ref.current?.focus();
          }}
        >
          <X size={12} aria-hidden />
        </button>
      ) : (
        shortcut && (
          <kbd
            aria-hidden
            className="pointer-events-none absolute right-2 top-1/2 -translate-y-1/2 rounded border border-ink-600 bg-surface-raised px-1 text-2xs leading-none text-ink-400"
          >
            /
          </kbd>
        )
      )}
    </div>
  );
}
