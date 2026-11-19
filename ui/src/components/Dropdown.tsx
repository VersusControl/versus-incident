import { useEffect, useId, useRef, useState } from "react";
import { Check, ChevronDown } from "lucide-react";
import clsx from "clsx";

export interface DropdownOption {
  value: string;
  label: string;
}

// Dropdown is a custom, accessible replacement for the native <select> — the
// browser default select renders inconsistently across platforms and can't be
// themed. This one is a WAI-ARIA listbox: opening it moves focus to the list,
// which tracks the highlighted option with aria-activedescendant. ↑/↓ move,
// Home/End jump, Enter/Space pick, Esc closes; picking or Esc returns focus
// to the button. It closes on outside-click and stays inside its container's
// DOM so it works within a focus-trapped Modal.
export function Dropdown({
  value,
  onChange,
  options,
  placeholder = "Select…",
  disabled,
  id,
  className,
  wrapOptions = false,
  listClassName,
  "aria-label": ariaLabel,
}: {
  value: string;
  onChange: (value: string) => void;
  options: DropdownOption[];
  placeholder?: string;
  disabled?: boolean;
  id?: string;
  className?: string;
  /** Wrap long option labels instead of truncating them (narrow screens). */
  wrapOptions?: boolean;
  /** Width of the open list; defaults to the button's width. */
  listClassName?: string;
  "aria-label"?: string;
}) {
  const [open, setOpen] = useState(false);
  const [active, setActive] = useState(0);
  const rootRef = useRef<HTMLDivElement>(null);
  const buttonRef = useRef<HTMLButtonElement>(null);
  const listRef = useRef<HTMLUListElement>(null);
  const listId = useId();

  const selectedIndex = options.findIndex((o) => o.value === value);
  const selected = selectedIndex >= 0 ? options[selectedIndex] : undefined;

  // Close on outside click / focus leaving the widget.
  useEffect(() => {
    if (!open) return;
    const onDocMouseDown = (e: MouseEvent) => {
      if (!rootRef.current?.contains(e.target as Node)) setOpen(false);
    };
    document.addEventListener("mousedown", onDocMouseDown);
    return () => document.removeEventListener("mousedown", onDocMouseDown);
  }, [open]);

  // When opening, highlight the current selection (or the first option).
  useEffect(() => {
    if (open) setActive(selectedIndex >= 0 ? selectedIndex : 0);
  }, [open, selectedIndex]);

  // React applies autoFocus only to form controls, so the list is focused
  // here; without it the list's key handler never receives a key.
  useEffect(() => {
    if (open) listRef.current?.focus({ preventScroll: true });
  }, [open]);

  const close = (refocus: boolean) => {
    setOpen(false);
    if (refocus) buttonRef.current?.focus();
  };

  // Keep the active option scrolled into view.
  useEffect(() => {
    if (!open) return;
    const el = listRef.current?.children[active] as HTMLElement | undefined;
    el?.scrollIntoView({ block: "nearest" });
  }, [open, active]);

  const commit = (index: number) => {
    const opt = options[index];
    if (opt) {
      onChange(opt.value);
      close(true);
    }
  };

  const onButtonKeyDown = (e: React.KeyboardEvent) => {
    if (disabled) return;
    switch (e.key) {
      case "ArrowDown":
      case "ArrowUp":
      case "Enter":
      case " ":
        e.preventDefault();
        setOpen(true);
        break;
      case "Escape":
        if (open) {
          e.preventDefault();
          setOpen(false);
        }
        break;
    }
  };

  const onListKeyDown = (e: React.KeyboardEvent) => {
    switch (e.key) {
      case "ArrowDown":
        e.preventDefault();
        setActive((i) => Math.min(options.length - 1, i + 1));
        break;
      case "ArrowUp":
        e.preventDefault();
        setActive((i) => Math.max(0, i - 1));
        break;
      case "Home":
        e.preventDefault();
        setActive(0);
        break;
      case "End":
        e.preventDefault();
        setActive(options.length - 1);
        break;
      case "Enter":
      case " ":
        e.preventDefault();
        commit(active);
        break;
      case "Escape":
        e.preventDefault();
        close(true);
        break;
      case "Tab":
        close(false);
        break;
    }
  };

  return (
    <div ref={rootRef} className={clsx("relative", className)}>
      <button
        ref={buttonRef}
        type="button"
        id={id}
        disabled={disabled}
        aria-haspopup="listbox"
        aria-expanded={open}
        aria-controls={open ? listId : undefined}
        aria-label={ariaLabel}
        className="input flex w-full items-center justify-between gap-2 pr-2.5 text-left disabled:cursor-not-allowed disabled:opacity-60"
        onClick={() => !disabled && setOpen((o) => !o)}
        onKeyDown={onButtonKeyDown}
      >
        <span className={clsx("truncate", !selected && "text-ink-400")}>
          {selected ? selected.label : placeholder}
        </span>
        <ChevronDown
          size={14}
          className={clsx(
            "shrink-0 text-ink-400 transition-transform",
            open && "rotate-180",
          )}
          aria-hidden
        />
      </button>

      {open && (
        <ul
          ref={listRef}
          id={listId}
          role="listbox"
          tabIndex={-1}
          aria-label={ariaLabel}
          aria-activedescendant={`${listId}-${active}`}
          className={clsx("absolute z-overlay mt-1 max-h-60 overflow-auto rounded-control border border-ink-500 bg-surface-raised p-1 shadow-overlay focus:outline-none", listClassName ?? "w-full")}
          onKeyDown={onListKeyDown}
        >
          {options.map((opt, i) => {
            const isSelected = opt.value === value;
            const isActive = i === active;
            return (
              <li
                key={opt.value}
                id={`${listId}-${i}`}
                role="option"
                aria-selected={isSelected}
                className={clsx(
                  "flex cursor-pointer items-center justify-between gap-2 rounded px-2 py-1.5 text-xs",
                  isActive ? "bg-accent-subtle text-ink-50" : "text-ink-200",
                )}
                onMouseEnter={() => setActive(i)}
                onMouseDown={(e) => {
                  // mousedown (not click) so the outside-click handler doesn't
                  // fire first and swallow the selection.
                  e.preventDefault();
                  commit(i);
                }}
              >
                <span className={wrapOptions ? "min-w-0 break-words" : "truncate"}>{opt.label}</span>
                {isSelected && (
                  <Check size={13} className="shrink-0 text-accent" aria-hidden />
                )}
              </li>
            );
          })}
        </ul>
      )}
    </div>
  );
}
