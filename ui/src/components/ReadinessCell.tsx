import { Check } from "lucide-react";
import type { Readiness } from "@/lib/api";
import { deriveReadiness, humanizeMinutes } from "@/lib/readiness";

// ReadinessCell — the ONE "learning progress / time-to-known" cell, rendered
// identically in the logs, metrics and traces tables. It shows QUANTITATIVE
// PROGRESS toward the point where the agent treats a signal as known — a
// "<seen> / <needed>" meter plus how much longer at the recent arrival rate —
// NOT a status label. The Verdict column already carries the classification
// (uncurated / known / spike); this column answers a different question: "how
// much more evidence until the agent stops treating this as new?" So there are
// no "Learning" / "Ready" pills here — those merely echoed the Verdict.
//
// Everything shown (remaining evidence, progress fraction, caveated ETA) is
// derived from the server's Readiness facts via lib/readiness.ts so the three
// tables can never drift. The per-type honesty note (logs are suppressed-as-
// routine once known, while metrics/traces START alerting once ready) lives in
// the column header <InfoHint>, not here — this cell is type-agnostic on purpose.
//
// Degrades cleanly: if `readiness` is absent (an older server, or a metric/
// trace row before the enterprise brain supplies it) it renders a quiet "—".

// The cell's own hover tooltip (native title) — short, per state. The longer,
// per-type explanation lives in the column header InfoHint.
const READY_TITLE = "Learned — reached its target.";
const INDETERMINATE_TITLE =
  "Auto-promotion is off (auto_promote_after ≤ 0) — mark this known by hand.";
const STALLED_TITLE = "No new data recently, so there's no time estimate.";
const ETA_CAVEAT = "at the recent arrival rate — an estimate, bursty signals vary.";

// ProgressBar — the shared little meter. `pct` is clamped 0..100; `tone` picks
// the fill colour (warn while learning, ok once complete).
function ProgressBar({ pct, tone }: { pct: number; tone: "warn" | "ok" }) {
  const clamped = Math.min(100, Math.max(0, pct));
  return (
    <span
      aria-hidden
      className="inline-block h-1 w-10 overflow-hidden rounded-full bg-ink-600"
    >
      <span
        className={
          tone === "ok"
            ? "block h-full rounded-full bg-sev-ok"
            : "block h-full rounded-full bg-sev-warn"
        }
        style={{ width: `${clamped}%` }}
      />
    </span>
  );
}

export function ReadinessCell({ readiness }: { readiness?: Readiness }) {
  if (!readiness) {
    return <span className="text-ink-400">—</span>;
  }

  const d = deriveReadiness(readiness);

  // Already known: no "Ready" label (that just echoes Verdict=known). Show the
  // progress COMPLETE — a full bar with a subtle check.
  if (d.ready) {
    return (
      <span
        className="inline-flex items-center gap-1.5 text-sev-ok"
        title={READY_TITLE}
      >
        <ProgressBar pct={100} tone="ok" />
        <Check size={12} aria-hidden />
      </span>
    );
  }

  // Auto-promotion disabled (needed === 0): no count gate applies, so there is
  // no progress to show. Surface the raw seen count with a muted "no target"
  // affordance — never the word "Learning" (that echoes Verdict).
  if (d.indeterminate) {
    return (
      <span
        className="inline-flex items-center gap-1.5"
        title={INDETERMINATE_TITLE}
      >
        <span className="tabular-nums text-ink-100">{readiness.seen}</span>
        <span className="text-ink-400">· no target</span>
      </span>
    );
  }

  // Learning toward a known target (learning or stalled). The NUMBER is the
  // message: lead with "<seen> / <needed>", append the caveated ETA as "how
  // much longer" when it's honest, and show the little progress bar.
  const count = `${readiness.seen} / ${readiness.needed}`;
  const eta = d.etaMinutes !== null ? humanizeMinutes(d.etaMinutes) : null;
  const title =
    eta !== null
      ? `Seen ${count}. About ${eta} to known ${ETA_CAVEAT}`
      : `Seen ${count}. ${STALLED_TITLE}`;
  const pct = d.progress !== null ? d.progress * 100 : 0;

  return (
    <span className="inline-flex items-center gap-1.5" title={title}>
      <span className="tabular-nums text-ink-100">{count}</span>
      {eta !== null && <span className="text-ink-300">· {eta}</span>}
      <ProgressBar pct={pct} tone="warn" />
    </span>
  );
}
