import { useEffect, useId, useRef, useState } from "react";
import clsx from "clsx";
import { ChevronDown } from "lucide-react";
import type { FinOpsExportAction, FinOpsExportHandlers } from "./exportHandlers";

// Export ▾: PDF, CSV, Copy summary, Save as monthly report. Keyboard: Enter
// or ↓ opens, ↑/↓ move, Escape closes and returns focus to the button.

interface Item {
  action: FinOpsExportAction;
  label: string;
  hint?: string;
  disabledReason?: string;
}

export function ExportMenu({
  handlers,
  notFinal,
  saveDisabledReason,
  canSave,
  disabled,
}: {
  handlers: FinOpsExportHandlers;
  notFinal: boolean;
  /** Set when the month cannot be saved yet (bill not closed, period open). */
  saveDisabledReason?: string;
  /** Admins only: viewers never see Save as monthly report. */
  canSave: boolean;
  disabled?: boolean;
}) {
  const [open, setOpen] = useState(false);
  const menuId = useId();
  const buttonRef = useRef<HTMLButtonElement>(null);
  const listRef = useRef<HTMLDivElement>(null);
  const items: Item[] = [
    { action: "pdf", label: "PDF", hint: notFinal ? "Not final" : undefined },
    { action: "csv", label: "CSV", hint: notFinal ? "Not final" : undefined },
    { action: "copy_summary", label: "Copy summary" },
  ];
  if (canSave) items.push({ action: "save_monthly_report", label: "Save as monthly report", disabledReason: saveDisabledReason });

  useEffect(() => {
    if (!open) return;
    listRef.current?.querySelector<HTMLButtonElement>("[role=menuitem]:not([aria-disabled=true])")?.focus();
    const onDown = (e: MouseEvent) => {
      if (!listRef.current?.contains(e.target as Node) && !buttonRef.current?.contains(e.target as Node)) setOpen(false);
    };
    document.addEventListener("mousedown", onDown);
    return () => document.removeEventListener("mousedown", onDown);
  }, [open]);

  const close = () => {
    setOpen(false);
    buttonRef.current?.focus();
  };

  const onKeyDown = (e: React.KeyboardEvent) => {
    const els = Array.from(listRef.current?.querySelectorAll<HTMLButtonElement>("[role=menuitem]") ?? []);
    const i = els.indexOf(document.activeElement as HTMLButtonElement);
    if (e.key === "Escape") {
      e.preventDefault();
      close();
    } else if (e.key === "ArrowDown") {
      e.preventDefault();
      els[(i + 1) % els.length]?.focus();
    } else if (e.key === "ArrowUp") {
      e.preventDefault();
      els[(i - 1 + els.length) % els.length]?.focus();
    } else if (e.key === "Tab") {
      setOpen(false);
    }
  };

  return (
    <div className="relative">
      <button
        ref={buttonRef}
        type="button"
        className="btn"
        aria-haspopup="menu"
        aria-expanded={open}
        aria-controls={open ? menuId : undefined}
        disabled={disabled}
        onClick={() => setOpen((v) => !v)}
        onKeyDown={(e) => {
          if (e.key === "ArrowDown") {
            e.preventDefault();
            setOpen(true);
          }
        }}
      >
        Export
        <ChevronDown size={12} aria-hidden="true" />
      </button>
      {open && (
        <div
          ref={listRef}
          id={menuId}
          role="menu"
          aria-label="Export"
          onKeyDown={onKeyDown}
          className="absolute right-0 top-full z-dropdown mt-1 w-64 rounded-control border border-ink-500 bg-surface-raised p-1 shadow-overlay"
        >
          {items.map((it) => {
            const off = Boolean(it.disabledReason);
            return (
              <button
                key={it.action}
                type="button"
                role="menuitem"
                aria-disabled={off || undefined}
                className={clsx(
                  "flex w-full items-center justify-between gap-2 rounded px-2.5 py-1.5 text-left text-xs focus:bg-ink-600 focus:outline-none",
                  off ? "cursor-not-allowed text-ink-400" : "text-ink-100 hover:bg-ink-600",
                )}
                onClick={() => {
                  if (off) return;
                  void handlers[it.action]();
                  close();
                }}
              >
                <span>{it.label}</span>
                {(it.hint || it.disabledReason) && (
                  <span className={clsx("text-2xs", it.hint ? "text-sev-warn" : "text-ink-400")}>
                    {it.hint ?? it.disabledReason}
                  </span>
                )}
              </button>
            );
          })}
        </div>
      )}
    </div>
  );
}
