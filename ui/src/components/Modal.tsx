import { useEffect, useId, useRef } from "react";
import { X } from "lucide-react";
import clsx from "clsx";

// Modal is the single accessible dialog base every overlay builds on
// (ConfirmDialog, AssignDialog, the people/runbook forms, re-auth, the
// shortcut overlay). It provides what the audited plain-div modals lacked:
// role="dialog" + aria-modal + labelled title, Escape-to-close, a focus
// trap, initial focus, focus restore on close, and body scroll lock.
// Dep-free by design.
const FOCUSABLE =
  "a[href], button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex='-1'])";

export function Modal({
  title,
  onClose,
  children,
  footer,
  size = "md",
  closeDisabled,
}: {
  title: string;
  onClose: () => void;
  children: React.ReactNode;
  footer?: React.ReactNode;
  size?: "sm" | "md" | "lg";
  /** Guard close while a mutation is pending (kept from ConfirmDialog). */
  closeDisabled?: boolean;
}) {
  const panelRef = useRef<HTMLDivElement>(null);
  const titleId = useId();

  useEffect(() => {
    const prev = document.activeElement as HTMLElement | null;
    const panel = panelRef.current;
    // Initial focus: first focusable inside, else the panel itself.
    const first = panel?.querySelector<HTMLElement>(FOCUSABLE);
    (first ?? panel)?.focus();

    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape" && !closeDisabled) {
        e.stopPropagation();
        onClose();
        return;
      }
      if (e.key !== "Tab" || !panel) return;
      // Focus trap: cycle within the panel.
      const items = Array.from(panel.querySelectorAll<HTMLElement>(FOCUSABLE));
      if (items.length === 0) return;
      const firstEl = items[0];
      const lastEl = items[items.length - 1];
      if (e.shiftKey && document.activeElement === firstEl) {
        e.preventDefault();
        lastEl.focus();
      } else if (!e.shiftKey && document.activeElement === lastEl) {
        e.preventDefault();
        firstEl.focus();
      }
    };
    document.addEventListener("keydown", onKey, true);

    const prevOverflow = document.body.style.overflow;
    document.body.style.overflow = "hidden";
    return () => {
      document.removeEventListener("keydown", onKey, true);
      document.body.style.overflow = prevOverflow;
      prev?.focus();
    };
  }, [onClose, closeDisabled]);

  return (
    <div
      className="fixed inset-0 z-modal flex items-center justify-center bg-black/50 p-4"
      onClick={() => !closeDisabled && onClose()}
    >
      <div
        ref={panelRef}
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleId}
        tabIndex={-1}
        onClick={(e) => e.stopPropagation()}
        className={clsx(
          "motion-safe:animate-[modal-in_180ms_ease-out] w-full rounded-card border border-ink-600 bg-surface-raised shadow-modal",
          size === "sm" && "max-w-sm",
          size === "md" && "max-w-md",
          size === "lg" && "max-w-lg",
        )}
      >
        <div className="flex items-center justify-between border-b border-ink-600 px-4 py-3">
          <h2 id={titleId} className="text-sm font-semibold text-ink-50">
            {title}
          </h2>
          <button
            aria-label="Close dialog"
            className="rounded-control p-1 text-ink-300 hover:bg-ink-600 hover:text-ink-100"
            onClick={onClose}
            disabled={closeDisabled}
          >
            <X size={14} />
          </button>
        </div>
        <div className="p-4">{children}</div>
        {footer && (
          <div className="flex justify-end gap-2 border-t border-ink-600 px-4 py-3">
            {footer}
          </div>
        )}
      </div>
    </div>
  );
}
