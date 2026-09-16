import { useEffect, useRef, useState } from "react";
import { createPortal } from "react-dom";
import { Maximize2, Minimize2, X } from "lucide-react";
import clsx from "clsx";
import { tabbableCandidates } from "./peekPanelFocus";

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
  size = "default",
  expandable = false,
  ariaLabel = "Details panel",
}: {
  open: boolean;
  onClose: () => void;
  title: React.ReactNode;
  children: React.ReactNode;
  footer?: React.ReactNode;
  size?: "default" | "wide";
  expandable?: boolean;
  ariaLabel?: string;
}) {
  const closeRef = useRef<HTMLButtonElement>(null);
  const overlayRef = useRef<HTMLDivElement>(null);
  const panelRef = useRef<HTMLElement>(null);
  const onCloseRef = useRef(onClose);
  const [expanded, setExpanded] = useState(false);
  onCloseRef.current = onClose;

  useEffect(() => {
    if (!open) return;
    const prev = document.activeElement as HTMLElement | null;
    closeRef.current?.focus();
    const onKey = (e: KeyboardEvent) => {
      // A modal stacked above this panel owns keyboard interaction.
      if (document.body.dataset.modalDepth) return;
      if (e.key === "Escape") {
        e.stopPropagation();
        onCloseRef.current();
        return;
      }
      if (e.key !== "Tab" || !panelRef.current) return;
      const items = tabbableCandidates(panelRef.current);
      if (items.length === 0) return;
      const first = items[0];
      const last = items[items.length - 1];
      if (e.shiftKey && document.activeElement === first) {
        e.preventDefault();
        last.focus();
      } else if (!e.shiftKey && document.activeElement === last) {
        e.preventDefault();
        first.focus();
      }
    };
    document.addEventListener("keydown", onKey, true);

    const previousOverflow = document.body.style.overflow;
    document.body.style.overflow = "hidden";
    const siblings = Array.from(document.body.children).filter((element) =>
      element !== overlayRef.current && !element.querySelector('[role="dialog"][aria-modal="true"]'));
    const previousInert = siblings.map((element) => ({ element, inert: element.getAttribute("inert") }));
    previousInert.forEach(({ element }) => { element.setAttribute("inert", ""); });
    return () => {
      document.removeEventListener("keydown", onKey, true);
      document.body.style.overflow = previousOverflow;
      previousInert.forEach(({ element, inert }) => {
        if (inert == null) element.removeAttribute("inert");
        else element.setAttribute("inert", inert);
      });
      setExpanded(false);
      prev?.focus();
    };
  }, [open]);

  if (!open) return null;

  return createPortal(
    <div ref={overlayRef} className="fixed inset-0 z-overlay" role="dialog" aria-modal="true" aria-label={ariaLabel}>
      <button type="button" tabIndex={-1} className="absolute inset-0 cursor-default bg-black/30" aria-label="Dismiss details" onClick={onClose} />
      <aside
        ref={panelRef}
        tabIndex={-1}
        className={clsx(
          "absolute bottom-0 right-0 top-0 flex w-full flex-col",
          "border-l border-ink-600 bg-surface-raised shadow-overlay",
          "motion-safe:animate-[peek-in_200ms_ease-out]",
          size === "default" && "max-w-lg",
          size === "wide" && !expanded && "sm:max-w-[min(46rem,calc(100vw-2rem))]",
          size === "wide" && expanded && "sm:max-w-[min(72rem,calc(100vw-2rem))]",
        )}
      >
        <div className="flex shrink-0 items-start justify-between gap-4 border-b border-ink-600 px-5 py-4">
          <h2 className="min-w-0 break-words text-base font-semibold leading-snug text-ink-50 [overflow-wrap:anywhere]">
            {title}
          </h2>
          <div className="flex shrink-0 items-center gap-1">
            {expandable && <button
              type="button"
              aria-label={expanded ? "Collapse panel" : "Expand panel"}
              title={expanded ? "Collapse panel" : "Expand panel"}
              aria-pressed={expanded}
              className="hidden rounded-control p-1.5 text-ink-300 hover:bg-ink-600 hover:text-ink-100 sm:inline-flex"
              onClick={() => setExpanded((value) => !value)}
            >
              {expanded ? <Minimize2 size={16} aria-hidden /> : <Maximize2 size={16} aria-hidden />}
            </button>}
            <button
              ref={closeRef}
              type="button"
              aria-label="Close panel"
              title="Close panel"
              className="rounded-control p-1.5 text-ink-300 hover:bg-ink-600 hover:text-ink-100"
              onClick={onClose}
            >
              <X size={16} aria-hidden />
            </button>
          </div>
        </div>
        <div className="overlay-body min-h-0 flex-1 overflow-y-auto p-5">
          {children}
        </div>
        {footer && (
          <div className="flex min-w-0 shrink-0 flex-wrap items-center justify-end gap-2 border-t border-ink-600 px-5 py-3">
            {footer}
          </div>
        )}
      </aside>
    </div>,
    document.body,
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
