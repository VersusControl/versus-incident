import { useState } from "react";
import { ExternalLink, Info } from "lucide-react";
import clsx from "clsx";
import { PeekPanel } from "./PeekPanel";

// InfoHint is a small info icon in a table header. Clicking it opens the
// explanation in the same right-hand slide-over panel used when you click a
// table row (PeekPanel) — a consistent, unclipped surface that works on touch
// and keyboard. Escape or a scrim click closes it.
//
// `example` is optional: when set it renders below the main text as a
// visually distinct, muted "Example" block so a non-expert operator can
// anchor the explanation to a concrete case.
//
// `href` is optional: when set the panel renders a "Learn more" link at the
// bottom (opens in a new tab) so the icon doubles as a discoverable pointer to
// the full documentation page — used by the settings sections that link into
// the docsify docs site.
export function InfoHint({
  text,
  label,
  example,
  href,
  linkLabel = "Learn more",
}: {
  text: string;
  label?: string;
  example?: string;
  href?: string;
  linkLabel?: string;
}) {
  const [open, setOpen] = useState(false);

  return (
    <>
      <button
        type="button"
        aria-label={label ?? "Column info"}
        aria-expanded={open}
        className={clsx(
          "ml-1 inline-flex align-middle focus:outline-none focus-visible:text-ink-100",
          open ? "text-accent" : "text-ink-400 hover:text-ink-100",
        )}
        onClick={(e) => {
          // Don't let the header's own click (sort/select) fire.
          e.stopPropagation();
          setOpen(true);
        }}
      >
        <Info size={12} aria-hidden />
      </button>
      <PeekPanel
        open={open}
        onClose={() => setOpen(false)}
        title={
          <span className="block text-left normal-case">
            {label ?? "About this column"}
          </span>
        }
      >
        <p className="whitespace-normal break-words text-left text-xs font-normal normal-case leading-relaxed text-ink-200">
          {text}
        </p>
        {example && (
          <div className="mt-2 text-left">
            <span className="block text-2xs font-medium uppercase tracking-wide text-ink-400">
              Example
            </span>
            <p className="whitespace-normal break-words text-2xs font-normal normal-case leading-relaxed text-ink-400">
              {example}
            </p>
          </div>
        )}
        {href && (
          <a
            href={href}
            target="_blank"
            rel="noreferrer"
            className="mt-3 inline-flex items-center gap-1 text-xs font-medium normal-case text-link hover:underline"
          >
            {linkLabel}
            <ExternalLink size={12} aria-hidden />
          </a>
        )}
      </PeekPanel>
    </>
  );
}


