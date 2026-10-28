import clsx from "clsx";

const BAR_TONES = ["bg-sev-info", "bg-sev-ok", "bg-sev-warn", "bg-ink-300"];

export function ProportionalBarList({
  rows,
  label,
}: {
  rows: Array<[string, number]>;
  label: string;
}) {
  const max = Math.max(1, ...rows.map(([, count]) => count));

  return (
    <div role="list" aria-label={label} className="min-w-0">
      <div className="relative h-40 border-b border-ink-500">
        {[0.25, 0.5, 0.75, 1].map((fraction) => (
          <div
            key={fraction}
            aria-hidden
            className="absolute left-0 right-0 border-t border-ink-700/70"
            style={{ bottom: `${fraction * 100}%` }}
          />
        ))}
        <div
          className="absolute inset-0 grid items-end gap-3 px-2"
          style={{
            gridTemplateColumns: `repeat(${rows.length}, minmax(0, 1fr))`,
          }}
        >
          {rows.map(([key, count], index) => {
            const height = (count / max) * 100;
            return (
              <div
                role="listitem"
                key={key}
                className="flex h-full min-w-0 flex-col items-center justify-end"
              >
                <span className="mb-1 shrink-0 tabular-nums text-2xs font-medium text-ink-100">
                  {count.toLocaleString()}
                </span>
                <div
                  role="progressbar"
                  aria-label={`${key}: ${count.toLocaleString()}`}
                  aria-valuemin={0}
                  aria-valuemax={max}
                  aria-valuenow={count}
                  className={clsx(
                    "w-full max-w-16 rounded-t-control",
                    BAR_TONES[index % BAR_TONES.length],
                  )}
                  style={{ height: `${height}%` }}
                />
              </div>
            );
          })}
        </div>
      </div>
      <div
        className="mt-2 grid gap-3 px-2"
        style={{ gridTemplateColumns: `repeat(${rows.length}, minmax(0, 1fr))` }}
      >
        {rows.map(([key]) => (
          <span
            key={key}
            className="min-w-0 text-center [overflow-wrap:anywhere] font-mono text-2xs leading-tight text-ink-300"
          >
            {key}
          </span>
        ))}
      </div>
    </div>
  );
}