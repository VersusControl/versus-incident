import { useSearchParams } from "react-router-dom";
import clsx from "clsx";

// SegmentedControl — URL-synced filter tabs. The audit's D5/D6 root cause
// was filters living in useState: dashboard tiles couldn't deep-link and
// back-navigation lost state. The param IS the state here, so every filter
// view is shareable and restorable.
export function SegmentedControl({
  param,
  options,
  defaultValue,
  "aria-label": ariaLabel,
}: {
  param: string;
  options: Array<{ value: string; label: string; badge?: number }>;
  defaultValue: string;
  "aria-label": string;
}) {
  const [params, setParams] = useSearchParams();
  const current = params.get(param) ?? defaultValue;

  return (
    <div
      role="tablist"
      aria-label={ariaLabel}
      className="inline-flex rounded-control border border-ink-500 bg-surface-raised p-0.5"
    >
      {options.map((o) => {
        const active = current === o.value;
        return (
          <button
            key={o.value}
            role="tab"
            aria-selected={active}
            className={clsx(
              "inline-flex min-h-8 items-center gap-1.5 rounded px-3 py-1 text-xs font-medium transition-colors",
              active
                ? "bg-accent-subtle text-ink-50"
                : "text-ink-300 hover:text-ink-100",
            )}
            onClick={() => {
              const next = new URLSearchParams(params);
              if (o.value === defaultValue) next.delete(param);
              else next.set(param, o.value);
              setParams(next, { replace: true });
            }}
          >
            {o.label}
            {/* 0 renders too — a missing badge next to "Open 4" read as a
                broken count, not an empty one. */}
            {o.badge !== undefined && (
              <span
                className={clsx(
                  "rounded-full px-1.5 text-2xs tabular-nums",
                  active ? "bg-accent text-white" : "bg-ink-600 text-ink-200",
                )}
              >
                {o.badge}
              </span>
            )}
          </button>
        );
      })}
    </div>
  );
}
