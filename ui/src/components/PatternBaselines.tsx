import type { SeasonalBucket } from "@/lib/api";

// PatternBaselines — the operator-facing view of the THREE baselines the spike
// detector can score a log pattern against, one block per spike_baseline_mode
// option so it's obvious which number belongs to which mode:
//   • Default (global baseline)   → the smoothed EWMA per-second rate + its σ.
//   • Average (cumulative mean)    → the never-decaying mean rate.
//   • Time of day (hour-of-day)    → the 24 learned hourly rates.
// Every field is optional so the SAME block renders from a full Pattern detail
// read and from a leaner service-detail ServicePattern; a missing number reads
// "—" rather than a fabricated zero.
export function PatternBaselines({
  frequency,
  variance,
  avg,
  seasonal,
}: {
  frequency?: number;
  variance?: number;
  avg?: number;
  seasonal?: SeasonalBucket[];
}) {
  const std =
    variance !== undefined && variance >= 0 ? Math.sqrt(variance) : undefined;

  return (
    <div className="space-y-3">
      <dl className="grid grid-cols-1 gap-3">
        <BaselineFact
          label="Default (global baseline)"
          hint="The smoothed normal rate — a spike runs well above this."
        >
          {frequency !== undefined ? (
            <span className="tabular-nums">
              ≈ {frequency.toFixed(1)}/s
              {std !== undefined && (
                <span className="text-ink-300"> ± {std.toFixed(1)}/s</span>
              )}
            </span>
          ) : (
            <span className="text-ink-400">—</span>
          )}
        </BaselineFact>
        <BaselineFact
          label="Average (cumulative mean)"
          hint="The all-time mean rate, which never decays."
        >
          {avg !== undefined ? (
            <span className="tabular-nums">≈ {avg.toFixed(1)}/s</span>
          ) : (
            <span className="text-ink-400">—</span>
          )}
        </BaselineFact>
      </dl>

      <div>
        <div className="mb-1 text-2xs uppercase tracking-wider text-ink-300">
          Time of day (hour-of-day)
        </div>
        <p className="mb-2 text-2xs text-ink-400">
          The normal rate learned for each hour. Hours with no data yet read “—”.
        </p>
        <SeasonalGrid seasonal={seasonal} />
      </div>
    </div>
  );
}

function BaselineFact({
  label,
  hint,
  children,
}: {
  label: string;
  hint: string;
  children: React.ReactNode;
}) {
  return (
    <div>
      <dt className="text-2xs uppercase tracking-wider text-ink-300">
        {label}
      </dt>
      <dd className="mt-0.5 font-mono text-xs text-ink-100">{children}</dd>
      <p className="mt-0.5 text-2xs text-ink-400">{hint}</p>
    </div>
  );
}

// SeasonalGrid renders the 24 hour-of-day buckets as a compact hour→rate grid.
// An absent or count-0 bucket reads "—"; a warmed one shows its per-second
// rate, with the sample count and σ in the cell title for the curious.
function SeasonalGrid({ seasonal }: { seasonal?: SeasonalBucket[] }) {
  const hours = Array.from({ length: 24 }, (_, h) => {
    const b = seasonal?.[h];
    const warmed = !!b && b.count > 0;
    const hourLabel = `${h.toString().padStart(2, "0")}h`;
    const std = b && b.variance >= 0 ? Math.sqrt(b.variance) : 0;
    const title = warmed
      ? `${hourLabel}: ≈ ${b.mean.toFixed(1)}/s ± ${std.toFixed(1)}/s · ${b.count} sample${b.count === 1 ? "" : "s"}`
      : `${hourLabel}: no data yet`;
    return { h, hourLabel, warmed, value: b?.mean ?? 0, title };
  });

  return (
    <ul
      aria-label="Learned rate by hour of day"
      className="grid grid-cols-4 gap-1 sm:grid-cols-6"
    >
      {hours.map((cell) => (
        <li
          key={cell.h}
          title={cell.title}
          className="rounded-control border border-ink-600 bg-surface-sunken px-1.5 py-1 text-center"
        >
          <div className="text-2xs tabular-nums text-ink-400">
            {cell.hourLabel}
          </div>
          <div className="font-mono text-2xs tabular-nums text-ink-100">
            {cell.warmed ? cell.value.toFixed(1) : "—"}
          </div>
        </li>
      ))}
    </ul>
  );
}
