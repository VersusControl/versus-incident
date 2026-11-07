import clsx from "clsx";

/**
 * Shown when a refresh found that the list behind the page on screen has
 * changed. The rows stay readable; the reader chooses when to start over.
 */
export function ListChangedNotice({ onBack, className }: { onBack: () => void; className?: string }) {
  return (
    <p role="status" className={clsx("px-4 py-2 text-xs text-ink-400", className)}>
      This list changed since this page loaded.
      <button type="button" className="ml-2 text-link" onClick={onBack}>Back to page 1</button>
    </p>
  );
}
