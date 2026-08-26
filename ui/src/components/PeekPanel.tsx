import { useEffect, useRef } from "react";
import { X } from "lucide-react";
import clsx from "clsx";

// PeekPanel — right slide-over for quick inspection without losing list
// position (the Patterns curation flow: 5 steps → 1). Escape or scrim click
// closes; focus lands on the close button and is restored on close. Lighter
// scrim than Modal because the list behind stays meaningful context.
export function PeekPanel({
  open,
  onClose,
  title,
  children,
  footer,
}: {
  open: boolean;
  onClose: () => void;
  title: React.ReactNode;
  children: React.ReactNode;
  footer?: React.ReactNode;
}) {
  const closeRef = useRef<HTMLButtonElement>(null);

  useEffect(() => {
    if (!open) return;
    const prev = document.activeElement as HTMLElement | null;
    closeRef.current?.focus();
    const onKey = (e: KeyboardEvent) => {
      // A modal stacked above this panel owns Escape; without the guard the
      // one keypress would dismiss both overlays.
      if (document.body.dataset.modalDepth) return;
      if (e.key === "Escape") {
        e.stopPropagation();
        onClose();
      }
    };
    document.addEventListener("keydown", onKey, true);
    return () => {
      document.removeEventListener("keydown", onKey, true);
      prev?.focus();
    };
  }, [open, onClose]);

  if (!open) return null;

  return (
    <div className="fixed inset-0 z-overlay" role="dialog" aria-label="Details panel">
      <div className="absolute inset-0 bg-black/30" onClick={onClose} />
      <aside
        className={clsx(
          "absolute bottom-0 right-0 top-0 flex w-full max-w-lg flex-col",
          "border-l border-ink-600 bg-surface-raised shadow-overlay",
          "motion-safe:animate-[peek-in_200ms_ease-out]",
        )}
      >
        <div className="flex items-center justify-between border-b border-ink-600 px-4 py-3">
          <h2 className="min-w-0 truncate text-sm font-semibold text-ink-50">
            {title}
          </h2>
          <button
            ref={closeRef}
            aria-label="Close panel"
            className="rounded-control p-1 text-ink-300 hover:bg-ink-600 hover:text-ink-100"
            onClick={onClose}
          >
            <X size={14} />
          </button>
        </div>
        <div className="overlay-body min-h-0 flex-1 overflow-y-auto p-4">
          {children}
        </div>
        {footer && (
          <div className="flex min-w-0 flex-wrap items-center justify-end gap-2 border-t border-ink-600 px-4 py-3">
            {footer}
          </div>
        )}
      </aside>
    </div>
  );
}

// PeekField — one labelled fact inside a PeekPanel body. Shared by the peeks
// added to the incident / decision / analysis tables so their detail slide-outs
// read identically to the logs and metrics/traces peeks.
export function PeekField({
  label,
  children,
}: {
  label: string;
  children: React.ReactNode;
}) {
  return (
    <div className="min-w-0">
      <dt className="text-2xs uppercase tracking-wide text-ink-400">{label}</dt>
      <dd className="mt-0.5 min-w-0 [overflow-wrap:anywhere] text-ink-100">
        {children}
      </dd>
    </div>
  );
}
