import { useSearchParams } from "react-router-dom";
import clsx from "clsx";
import { CountUnavailable } from "@/components/CountUnavailable";
import { compactCount } from "@/lib/format";

// SegmentedControl — URL-synced filter tabs. The root cause
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
  // badge: undefined hides it (still loading); null means the server has no
  // count, shown as "—". badgeTitle is the hover hint, e.g. "Counted at 14:02".
  options: Array<{
    value: string;
    label: string;
    badge?: number | null;
    badgeTitle?: string;
  }>;
  defaultValue: string;
  "aria-label": string;
}) {
  const [params, setParams] = useSearchParams();
  const requested = params.get(param);
  const current = options.some((option) => option.value === requested)
    ? requested
    : defaultValue;

  return (
    <div
      role="tablist"
      aria-label={ariaLabel}
      // A narrow screen scrolls the bar sideways instead of pushing tabs past
      // the edge; the scrollbar is hidden so the bar keeps one height.
      className="scrollbar-none inline-flex max-w-full overflow-x-auto rounded-control border border-ink-500 bg-surface-raised p-0.5"
    >
      {options.map((o) => {
        const active = current === o.value;
        return (
          <button
            key={o.value}
            role="tab"
            aria-selected={active}
            className={clsx(
              "inline-flex min-h-8 shrink-0 items-center gap-1.5 whitespace-nowrap rounded px-2 py-1 text-xs font-medium transition-colors sm:px-3",
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
                title={badgeTitle(o.badge, o.badgeTitle)}
                className={clsx(
                  "rounded-full px-1.5 text-2xs tabular-nums",
                  active ? "bg-accent text-white" : "bg-ink-600 text-ink-200",
                )}
              >
                {o.badge === null ? (
                  <CountUnavailable />
                ) : o.badge >= 1000 ? (
                  // Compact on screen ("1.2k") so four tabs fit a phone; the
                  // full number is in the title and the accessible name.
                  <>
                    <span aria-hidden="true">{compactCount(o.badge)}</span>
                    <span className="sr-only">{o.badge}</span>
                  </>
                ) : (
                  o.badge
                )}
              </span>
            )}
          </button>
        );
      })}
    </div>
  );
}

// badgeTitle is the badge tooltip: the full number when the badge shows a
// compact one, followed by any hint (e.g. "1,234 · Counted at 14:02").
function badgeTitle(
  badge: number | null,
  hint: string | undefined,
): string | undefined {
  if (badge === null) return undefined;
  if (badge < 1000) return hint;
  const full = badge.toLocaleString();
  return hint ? `${full} · ${hint}` : full;
}
