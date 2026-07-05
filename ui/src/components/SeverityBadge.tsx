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
import { EmptyValue } from "@/components/feedback";

const ICONS: Record<Severity, LucideIcon> = {
  critical: AlertOctagon,
  high: AlertTriangle,
  warn: CircleAlert,
  info: Info,
  ok: CheckCircle2,
};

// SeverityBadge: icon + label + tone — never color-only (WCAG color-not-only).
// Accepts any raw severity string; unknown/absent renders the shared muted
// EmptyValue "—" so a null severity reads the same as any other empty cell
// (and never as a bare dot that looks like a rendering bug).
export function SeverityBadge({
  severity,
  className,
}: {
  severity?: string | null;
  className?: string;
}) {
  const sev = normalizeSeverity(severity);
  if (!sev) {
    return <EmptyValue className={className} />;
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
