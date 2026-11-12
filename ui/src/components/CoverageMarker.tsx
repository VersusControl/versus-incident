import { type ReactNode } from "react";
import { createPortal } from "react-dom";
import { useAnchoredPopover } from "@/lib/useAnchoredPopover";

/** A single, keyboard- and touch-accessible disclosure for unreadable evidence. */
export function CoverageMarker({ reasons }: { reasons: ReactNode[] }) {
  const { open, triggerProps, panelProps } = useAnchoredPopover();
  const unique = [...new Set(reasons)].filter(Boolean);
  if (!unique.length) return null;
  return <>
    <button {...triggerProps} type="button" aria-label="Resource coverage"
      className="inline-flex size-7 shrink-0 items-center justify-center rounded-control text-ink-400 hover:bg-ink-700 focus-visible:outline focus-visible:outline-2 focus-visible:outline-accent">
      <span aria-hidden="true" className="size-1.5 rounded-full bg-current" />
    </button>
    {open && createPortal(<div {...panelProps} role="region" aria-label="Resource coverage details"
      className="fixed overflow-auto rounded-control border border-ink-600 bg-surface-raised p-3 text-xs text-ink-200 shadow-overlay">
      <p className="font-medium">Some data is incomplete</p>
      <ul tabIndex={unique.length > 3 ? 0 : undefined} aria-label="Coverage reasons" className="mt-2 max-h-14 space-y-1 overflow-y-auto break-words">{unique.map((reason, index) => <li key={typeof reason === "string" ? reason : index}>{reason}</li>)}</ul>
    </div>, document.body)}
  </>;
}
