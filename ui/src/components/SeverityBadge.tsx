import {
  AlertOctagon,
  AlertTriangle,
  CheckCircle2,
  CircleAlert,
  Info,
  type LucideIcon,
} from "lucide-react";
import clsx from "clsx";
import {
  normalizeSeverity,
  severityChip,
  severityLabel,
  type Severity,
} from "@/lib/severity";

const ICONS: Record<Severity, LucideIcon> = {
  critical: AlertOctagon,
  high: AlertTriangle,
  warn: CircleAlert,
  info: Info,
  ok: CheckCircle2,
};

// SeverityBadge: icon + label + tone — never color-only (WCAG color-not-only).
// Accepts any raw severity string; unknown/absent renders a quiet "—" so the
// list column degrades exactly as specified while the backend lacks the
// severity field on summaries (UX_REDESIGN.md §3.5 ask #1).
export function SeverityBadge({
  severity,
  className,
}: {
  severity?: string | null;
  className?: string;
}) {
  const sev = normalizeSeverity(severity);
  if (!sev) {
    // Unknown severity: a quiet dot, not a dash — a column of "—" reads as
    // a broken UI when the list API doesn't carry severity yet (§3.5 #1).
    return (
      <span
        className={clsx("inline-flex items-center", className)}
        aria-label="severity unknown"
        title="Severity unavailable on list summaries"
      >
        {/* ink-400, not 500 — the dot measured 1.5:1 in dark (invisible). */}
        <span aria-hidden className="h-1.5 w-1.5 rounded-full bg-ink-400" />
      </span>
    );
  }
  const Icon = ICONS[sev];
  return (
    <span
      className={clsx(
        "inline-flex items-center gap-1 rounded-full border px-2 py-0.5 text-2xs font-semibold",
        severityChip[sev],
        className,
      )}
    >
      <Icon size={11} aria-hidden />
      {severityLabel[sev]}
    </span>
  );
}
