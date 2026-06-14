import { Link } from "react-router-dom";
import clsx from "clsx";
import type { LucideIcon } from "lucide-react";
import { Sparkline } from "@/components/Sparkline";
import { useCountUp } from "@/lib/hooks";

// KpiTile replaces the old Dashboard Tile. Two audited bugs designed out:
// (1) loading was gated on `a.isLoading && b.isLoading` so one settled query
//     flashed wrong data — each tile now takes ONE loading flag for ALL the
//     data it shows; pass `loading` until every source has settled.
// (2) `?? 0` fallbacks rendered a false "0 = all quiet" mid-load — while
//     loading we render a shimmer block, never a number.
export function KpiTile({
  label,
  value,
  loading,
  to,
  icon: Icon,
  tone,
  foot,
  spark,
  sparkLabel,
}: {
  label: string;
  value: React.ReactNode;
  loading?: boolean;
  /** Deep link WITH its filter (e.g. /incidents?status=open) — audit D5. */
  to?: string;
  icon?: LucideIcon;
  tone?: "critical" | "warn" | "ok" | "info";
  foot?: React.ReactNode;
  /** Real time-series only (e.g. hourly buckets from incident timestamps) —
      Sparkline renders nothing for empty/flat data, never a fabricated line. */
  spark?: number[];
  /** Required with `spark`: text summary for screen readers. */
  sparkLabel?: string;
}) {
  const sparkColor = tone
    ? {
        critical: "text-sev-critical/80",
        warn: "text-sev-warn/80",
        ok: "text-sev-ok/80",
        info: "text-sev-info/80",
      }[tone]
    : "text-ink-300";

  // Numeric values ease toward changes (live refetch ticking 4→5);
  // non-numeric values (mode strings, pre-formatted text) pass through.
  // CountUpValue mounts only once a real number exists, so the hook's
  // state seeds at the true value — never a 0→N sweep on first load
  // (the "false 0 mid-load" class banned in the header comment).
  const shown =
    typeof value === "number" ? <CountUpValue n={value} /> : (value ?? "—");

  const body = (
    <>
      <div className="flex items-center justify-between">
        <span className="stat-label">{label}</span>
        {Icon && <Icon size={14} className="text-ink-400" aria-hidden />}
      </div>
      {loading ? (
        <div aria-hidden className="sk mt-1 h-7 w-12" />
      ) : (
        <div className="flex items-end justify-between gap-2">
          <span
            className={clsx(
              "stat-value",
              tone === "critical" && "text-sev-critical",
              tone === "warn" && "text-sev-warn",
              tone === "ok" && "text-sev-ok",
              tone === "info" && "text-sev-info",
            )}
          >
            {shown}
          </span>
          {spark && sparkLabel && (
            <Sparkline
              data={spark}
              aria-label={sparkLabel}
              width={72}
              height={22}
              className={clsx("mb-1 shrink-0", sparkColor)}
            />
          )}
        </div>
      )}
      {foot && <span className="stat-foot">{foot}</span>}
    </>
  );

  if (to) {
    return (
      <Link
        to={to}
        className="stat-card transition-colors hover:border-accent/50"
      >
        {body}
      </Link>
    );
  }
  return <div className="stat-card">{body}</div>;
}

function CountUpValue({ n }: { n: number }) {
  const counted = useCountUp(n);
  return <>{counted.toLocaleString()}</>;
}
