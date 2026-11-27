import type { ReactNode } from "react";
import { Link } from "react-router-dom";
import clsx from "clsx";
import { AlertTriangle } from "lucide-react";
import { coverageWarningText, type CountWords, type CoverageWarning } from "@/lib/awsEstate";

// The summary card every AWS page shares: one row of plain numbers, a small
// grey label above each, and, only when data is incomplete, one warning
// line at the bottom. A number appears once on a page; the warning line
// appears here and nowhere else.

/** The card. `loading` marks an answer that stands in while the next one loads. */
export function SummaryCard({ loading = false, children }: { loading?: boolean; children: ReactNode }) {
  return (
    <section className="card pb-3" aria-label="Summary" aria-busy={loading || undefined} data-summary-card data-loading={loading || undefined}>
      {children}
    </section>
  );
}

/**
 * One number. `words` carries a capped count's "more than … not all read"
 * and a not-read dash; `to` links the number to the page that owns it.
 */
export function SummaryNumber({
  k,
  label,
  value,
  bad = false,
  words,
  to,
  title,
}: {
  k: string;
  label: string;
  value?: string;
  bad?: boolean;
  words?: CountWords;
  to?: string;
  /** Hover words, such as why a number is not read. */
  title?: string;
}) {
  // Not read and not all read share the grey style; only a whole count is drawn bright.
  const notRead = words?.state === "not_read" || words?.state === "capped";
  // A count with no number to stand on ("Not all read") is words, not a figure: small and grey.
  const wordsOnly = words?.state === "capped" && !words.prefix;
  const shown = words ? words.value : value;
  const body = (
    <>
      {words?.prefix && <span className="mr-1 font-sans text-xs font-normal text-ink-300" data-count-prefix>{words.prefix}</span>}
      {shown}
      {words?.suffix && <span className="ml-1 font-sans text-xs font-normal text-ink-300" data-count-suffix>{words.suffix}</span>}
    </>
  );
  return (
    <div data-number={k} data-state={words?.state} className="min-w-[4.5rem]" title={title}>
      <dt className="text-2xs text-ink-300">{label}</dt>
      <dd
        className={clsx(
          "mt-0.5",
          wordsOnly ? "pt-2 text-xs text-ink-300" : "font-mono text-2xl font-semibold tabular-nums",
          !wordsOnly && (bad ? "text-sev-critical" : notRead ? "text-ink-300" : "text-ink-50"),
        )}
        data-words-only={wordsOnly || undefined}
      >
        {to && !notRead ? (
          <Link to={to} className="rounded hover:underline focus:outline-none focus-visible:ring-2 focus-visible:ring-accent">
            {body}
          </Link>
        ) : (
          body
        )}
      </dd>
    </div>
  );
}

/** The warning line's frame: a hairline above, at the card's foot. */
export function WarningLine({ children }: { children: ReactNode }) {
  return (
    <div className="mx-4 mt-3 flex flex-wrap items-center gap-x-2 gap-y-1 border-t border-ink-500/40 pt-3 text-xs text-ink-100" data-warning-line>
      {children}
    </div>
  );
}

/** "Data not complete: 3 sources not read · Details ›", or nothing when data is complete. */
export function CoverageWarningLine({ warning }: { warning: CoverageWarning | null | undefined }) {
  const text = coverageWarningText(warning);
  if (!text) return null;
  return (
    <WarningLine>
      <p className="inline-flex items-center gap-2" data-warning-text>
        <AlertTriangle size={14} aria-hidden="true" className="shrink-0 text-sev-warn" />
        {text}
      </p>
      <Link to="/aws/coverage" className="ml-4 text-link hover:underline" data-details>
        Details ›
      </Link>
    </WarningLine>
  );
}
