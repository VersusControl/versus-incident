import { useEffect, useId, useLayoutEffect, useRef, useState, type FocusEvent, type KeyboardEvent as ReactKeyboardEvent } from "react";

const focusable = 'a[href], button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])';

/**
 * A details panel anchored to a top-bar trigger, rendered in a portal so a
 * blurred header cannot clip it. It opens on hover, focus and click, closes on
 * Escape (returning focus to the trigger), an outside press, or focus leaving
 * both the trigger and the panel, and it stays below the Kubernetes view tabs
 * and inside the viewport, scrolling when its content is long.
 *
 * With `bridgeFocus`, Tab from the open trigger moves into the panel and Tab
 * past its last control moves on to whatever follows the trigger, so a control
 * inside the portal is reachable from the keyboard in reading order.
 */
export function useAnchoredPopover({ width = 320, bridgeFocus = false }: { width?: number; bridgeFocus?: boolean } = {}) {
  const [open, setOpen] = useState(false);
  const [position, setPosition] = useState({ left: 8, top: 8, maxHeight: 240 });
  const trigger = useRef<HTMLButtonElement>(null);
  const panel = useRef<HTMLDivElement>(null);
  const id = useId();
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
        left: Math.max(8, Math.min(anchor.right - width, window.innerWidth - width - 8)),
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
  }, [open, width]);
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

  const panelControls = () => [...(panel.current?.querySelectorAll<HTMLElement>(focusable) ?? [])];
  // The element after the trigger in document order, skipping the portal.
  const focusAfterTrigger = () => {
    const all = [...document.querySelectorAll<HTMLElement>(focusable)].filter((element) => !panel.current?.contains(element));
    const index = trigger.current ? all.indexOf(trigger.current) : -1;
    setOpen(false);
    (all[index + 1] ?? trigger.current)?.focus();
  };

  const triggerProps = {
    ref: trigger,
    "aria-expanded": open,
    "aria-controls": open ? id : undefined,
    onClick: () => setOpen(true),
    onFocus: () => setOpen(true),
    onMouseEnter: () => setOpen(true),
    onBlur: (event: FocusEvent<HTMLButtonElement>) => { if (!panel.current?.contains(event.relatedTarget)) setOpen(false); },
    onKeyDown: (event: ReactKeyboardEvent<HTMLButtonElement>) => {
      if (!bridgeFocus || !open || event.key !== "Tab" || event.shiftKey) return;
      const first = panelControls()[0];
      if (!first) return;
      event.preventDefault();
      first.focus();
    },
  };
  const panelProps = {
    ref: panel,
    id,
    style: { ...position, width: `min(${width}px, calc(100vw - 16px))`, zIndex: 1000 },
    onBlur: (event: FocusEvent<HTMLDivElement>) => { if (!panel.current?.contains(event.relatedTarget) && event.relatedTarget !== trigger.current) setOpen(false); },
    onMouseLeave: () => { if (!panel.current?.contains(document.activeElement)) setOpen(false); },
    onKeyDown: (event: ReactKeyboardEvent<HTMLDivElement>) => {
      if (!bridgeFocus || event.key !== "Tab") return;
      const controls = panelControls();
      const index = controls.indexOf(document.activeElement as HTMLElement);
      if (event.shiftKey && index <= 0) { event.preventDefault(); trigger.current?.focus(); }
      else if (!event.shiftKey && index === controls.length - 1) { event.preventDefault(); focusAfterTrigger(); }
    },
  };
  return { open, setOpen, triggerProps, panelProps };
}
