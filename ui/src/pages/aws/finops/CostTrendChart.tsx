import type { FinOpsDayAmount } from "@/lib/api";
import { fmtDay, fmtMoney, fmtMoneyAxis } from "@/lib/finops";

// Daily cost bars. Shown in place of the bridge when there is no full
// previous period to compare with. A partial day is a dashed outline and is
// never counted as a complete day.

export function CostTrendChart({
  days,
  currency,
  title = "Daily cost",
}: {
  days: FinOpsDayAmount[];
  currency?: string;
  title?: string;
}) {
  if (days.length === 0) return null;
  const w = 600;
  const h = 140;
  const left = 44;
  const bottom = 18;
  const max = Math.max(...days.map((d) => d.amount_micros), 1);
  const plotW = w - left;
  const plotH = h - bottom - 6;
  const slot = plotW / days.length;
  const barW = Math.max(Math.min(slot * 0.7, 40), 2);
  const y = (v: number) => 6 + plotH - (Math.max(v, 0) / max) * plotH;
  const ticks = [0, max / 2, max];
  const labelEvery = Math.max(1, Math.ceil(days.length / 7));
  return (
    <figure className="m-0">
      <figcaption className="mb-1 text-2xs uppercase tracking-wider text-ink-300">{title}</figcaption>
      <svg role="img" aria-label={`${title}: ${days.length} days`} viewBox={`0 0 ${w} ${h}`} className="h-40 w-full">
        {ticks.map((t) => (
          <g key={t}>
            <line x1={left} x2={w} y1={y(t)} y2={y(t)} className="stroke-ink-500/50" strokeWidth={0.5} />
            <text x={left - 6} y={y(t) + 3} textAnchor="end" className="fill-ink-400 font-mono text-[9px]">
              {fmtMoneyAxis(t, currency)}
            </text>
          </g>
        ))}
        {days.map((d, i) => {
          const x = left + i * slot + (slot - barW) / 2;
          const top = y(d.amount_micros);
          const label = `${fmtDay(d.day)}: ${fmtMoney(d.amount_micros, { currency })}${d.partial ? ", partial" : ""}`;
          return (
            <g key={d.day}>
              <rect
                x={x}
                y={top}
                width={barW}
                height={Math.max(6 + plotH - top, 0.5)}
                className={d.partial ? "fill-transparent stroke-ink-300" : "fill-ink-400"}
                strokeDasharray={d.partial ? "3 2" : undefined}
              >
                <title>{label}</title>
              </rect>
              {i % labelEvery === 0 && (
                <text x={x + barW / 2} y={h - 4} textAnchor="middle" className="fill-ink-400 text-[9px]">
                  {fmtDay(d.day)}
                </text>
              )}
            </g>
          );
        })}
      </svg>
    </figure>
  );
}
