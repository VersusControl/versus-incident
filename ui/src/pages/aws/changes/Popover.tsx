import { useEffect, useId, useLayoutEffect, useRef, useState, type ReactNode } from "react";
import { createPortal } from "react-dom";
import clsx from "clsx";

/**
 * A click-to-open details panel for the Changes page. The trigger is a
 * button; the panel is a labelled dialog rendered in a portal, so a card that
 * scrolls inside never clips it. Opening moves focus into the panel; Escape
 * closes it and returns focus to the trigger; a press outside or focus
 * leaving the panel closes it.
 */
export function Popover({
  trigger,
  label,
  children,
  triggerClassName,
  width = 360,
  triggerAttrs,
  panelAttrs,
}: {
  /** The trigger's visible content. */
  trigger: ReactNode;
  /** The panel's accessible name. */
  label: string;
  children: ReactNode;
  triggerClassName?: string;
  width?: number;
  triggerAttrs?: Record<string, string>;
  panelAttrs?: Record<string, string>;
}) {
  const [open, setOpen] = useState(false);
  const [pos, setPos] = useState({ left: 8, top: 8, maxHeight: 320 });
  const button = useRef<HTMLButtonElement>(null);
  const panel = useRef<HTMLDivElement>(null);
  const id = useId();

  useLayoutEffect(() => {
    if (!open) return;
    const place = () => {
      const r = button.current?.getBoundingClientRect();
      if (!r) return;
      const h = panel.current?.getBoundingClientRect().height || 160;
      const below = r.bottom + 4;
      const top = below + h <= window.innerHeight - 8 ? below : Math.max(8, r.top - h - 4);
      setPos({
        left: Math.max(8, Math.min(r.left, window.innerWidth - width - 8)),
        top,
        maxHeight: Math.max(120, window.innerHeight - top - 8),
      });
    };
    place();
    window.addEventListener("resize", place);
    window.addEventListener("scroll", place, true);
    return () => {
      window.removeEventListener("resize", place);
      window.removeEventListener("scroll", place, true);
    };
  }, [open, width]);

  useEffect(() => {
    if (!open) return;
    panel.current?.focus({ preventScroll: true });
    const outside = (e: PointerEvent) => {
      const t = e.target as Node;
      if (!button.current?.contains(t) && !panel.current?.contains(t)) setOpen(false);
    };
    const escape = (e: KeyboardEvent) => {
      if (e.key !== "Escape") return;
      setOpen(false);
      button.current?.focus();
    };
    document.addEventListener("pointerdown", outside);
    document.addEventListener("keydown", escape);
    return () => {
      document.removeEventListener("pointerdown", outside);
      document.removeEventListener("keydown", escape);
    };
  }, [open]);

  return (
    <>
      <button
        ref={button}
        type="button"
        aria-haspopup="dialog"
        aria-expanded={open}
        aria-controls={open ? id : undefined}
        className={clsx("focus:outline-none focus-visible:ring-2 focus-visible:ring-accent", triggerClassName)}
        onClick={() => setOpen((o) => !o)}
        {...triggerAttrs}
      >
        {trigger}
      </button>
      {open &&
        createPortal(
          <div
            ref={panel}
            id={id}
            role="dialog"
            aria-label={label}
            tabIndex={-1}
            style={{ position: "fixed", left: pos.left, top: pos.top, maxHeight: pos.maxHeight, width: `min(${width}px, calc(100vw - 16px))` }}
            className="z-overlay overflow-auto rounded-control border border-ink-500 bg-surface-raised p-3 text-xs text-ink-100 shadow-overlay focus:outline-none"
            onBlur={(e) => {
              const next = e.relatedTarget as Node | null;
              if (next && !panel.current?.contains(next) && next !== button.current) setOpen(false);
            }}
            {...panelAttrs}
          >
            {children}
          </div>,
          document.body,
        )}
    </>
  );
}
