import { useEffect, useId, useLayoutEffect, useRef, useState, type ReactNode } from "react";
import { createPortal } from "react-dom";

/** A single, keyboard- and touch-accessible disclosure for unreadable evidence. */
export function CoverageMarker({ reasons }: { reasons: ReactNode[] }) {
  const [open, setOpen] = useState(false);
  const [position, setPosition] = useState({ left: 8, top: 8, maxHeight: 240 });
  const trigger = useRef<HTMLButtonElement>(null);
  const panel = useRef<HTMLDivElement>(null);
  const id = useId();
  const unique = [...new Set(reasons)].filter(Boolean);
  useLayoutEffect(() => {
    if (!open) return;
    const place = () => {
      const anchor = trigger.current?.getBoundingClientRect();
      if (!anchor) return;
      const tabs = document.querySelector('[aria-label="Kubernetes views"]')?.getBoundingClientRect();
      const floor = tabs && tabs.bottom > 0 && tabs.top < window.innerHeight ? tabs.bottom + 8 : 8;
      const height = panel.current?.getBoundingClientRect().height || 120;
      const below = Math.max(floor, anchor.bottom + 8);
      const top = below + height <= window.innerHeight - 8 ? below : Math.max(floor, anchor.top - height - 8);
      setPosition({
        left: Math.max(8, Math.min(anchor.right - 320, window.innerWidth - 328)),
        top,
        maxHeight: Math.max(40, window.innerHeight - top - 8),
      });
    };
    place();
    window.addEventListener("resize", place);
    window.addEventListener("scroll", place, true);
    return () => {
      window.removeEventListener("resize", place);
      window.removeEventListener("scroll", place, true);
    };
  }, [open]);
  useEffect(() => {
    if (!open) return;
    const outside = (event: PointerEvent) => {
      const target = event.target as Node;
      if (!trigger.current?.contains(target) && !panel.current?.contains(target)) setOpen(false);
    };
    const escape = (event: KeyboardEvent) => {
      if (event.key === "Escape") { trigger.current?.focus(); setOpen(false); }
    };
    document.addEventListener("pointerdown", outside);
    document.addEventListener("keydown", escape);
    return () => {
      document.removeEventListener("pointerdown", outside);
      document.removeEventListener("keydown", escape);
    };
  }, [open]);
  if (!unique.length) return null;
  return <>
    <button ref={trigger} type="button" aria-label="Resource coverage" aria-expanded={open} aria-controls={open ? id : undefined}
      onClick={() => setOpen(true)} onFocus={() => setOpen(true)} onMouseEnter={() => setOpen(true)}
      onBlur={(event) => { if (!panel.current?.contains(event.relatedTarget)) setOpen(false); }}
      className="inline-flex size-7 shrink-0 items-center justify-center rounded-control text-ink-400 hover:bg-ink-700 focus-visible:outline focus-visible:outline-2 focus-visible:outline-accent">
      <span aria-hidden="true" className="size-1.5 rounded-full bg-current" />
    </button>
    {open && createPortal(<div ref={panel} id={id} role="region" aria-label="Resource coverage details"
      onBlur={(event) => { if (!panel.current?.contains(event.relatedTarget) && event.relatedTarget !== trigger.current) setOpen(false); }}
      style={{ ...position, width: "min(320px, calc(100vw - 16px))", zIndex: 1000 }}
      onMouseLeave={() => { if (!panel.current?.contains(document.activeElement)) setOpen(false); }}
      className="fixed overflow-auto rounded-control border border-ink-600 bg-surface-raised p-3 text-xs text-ink-200 shadow-overlay">
      <p className="font-medium">Some resources could not be read</p>
      <ul tabIndex={unique.length > 3 ? 0 : undefined} aria-label="Coverage reasons" className="mt-2 max-h-14 space-y-1 overflow-y-auto break-words">{unique.map((reason, index) => <li key={typeof reason === "string" ? reason : index}>{reason}</li>)}</ul>
    </div>, document.body)}
  </>;
}
