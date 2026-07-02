import { Check } from "lucide-react";
import type { Readiness } from "@/lib/api";
import { deriveReadiness } from "@/lib/readiness";

// ReadinessProgress — the "how close to known" indicator for peek panels and
// detail pages. A slim progress bar + a <seen>/<needed> count, NO time estimate
// (matches the Logs Verdict cell). States:
//   ready          → full bar + check + "Ready"
//   learning       → accent bar + "seen / needed"
//   indeterminate  → seen count + "mark known by hand" (auto-promotion off)
//   absent         → "—"
// It reads the same server Readiness facts as ReadinessCell (via
// lib/readiness.ts) so the number can never drift from the table.
export function ReadinessProgress({ readiness }: { readiness?: Readiness }) {
  if (!readiness) return <span className="text-ink-400">—</span>;
  const d = deriveReadiness(readiness);

  if (d.ready) {
    return (
      <span className="inline-flex items-center gap-2 text-sev-ok">
        <Track pct={100} tone="ok" />
        <span className="inline-flex items-center gap-1 text-2xs">
          <Check size={12} aria-hidden /> Ready
        </span>
      </span>
    );
  }

  if (d.indeterminate) {
    return (
      <span className="text-2xs text-ink-300">
        <span className="tabular-nums text-ink-100">{readiness.seen}</span> seen
        <span className="text-ink-500"> · mark known by hand</span>
      </span>
    );
  }

  const pct = Math.min(
    100,
    Math.round((readiness.seen / readiness.needed) * 100),
  );
  return (
    <span className="inline-flex items-center gap-2">
      <Track pct={pct} tone="warn" />
      <span className="text-2xs tabular-nums text-ink-300">
        <span className="text-ink-100">{readiness.seen}</span>
        <span className="text-ink-600">/</span>
        {readiness.needed}
      </span>
    </span>
  );
}

function Track({ pct, tone }: { pct: number; tone: "warn" | "ok" }) {
  return (
    <span
      aria-hidden
      className="inline-block h-1.5 w-16 shrink-0 overflow-hidden rounded-full bg-ink-700"
    >
      <span
        className={
          tone === "ok"
            ? "block h-full rounded-full bg-sev-ok"
            : "block h-full rounded-full bg-accent transition-[width]"
        }
        style={{ width: `${Math.min(100, Math.max(0, pct))}%` }}
      />
    </span>
  );
}
