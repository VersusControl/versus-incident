import clsx from "clsx";

export type FreshnessState = "live" | "stale" | "disconnected";

/**
 * How old the last successful check is.
 *
 * Same rule as the cluster sync indicator: the state is always written out, so
 * a narrow viewport never leaves an operator with a coloured dot alone. The dot
 * only repeats what the words already say.
 */
function ageWords(seconds: number | undefined): string | undefined {
  if (seconds === undefined || !Number.isFinite(seconds) || seconds < 0) {
    return undefined;
  }
  if (seconds < 60) return `${Math.round(seconds)}s`;
  const minutes = Math.round(seconds / 60);
  return `${minutes} min ago`;
}

function label(state: FreshnessState, ageSeconds: number | undefined): string {
  if (state === "disconnected") return "Disconnected";
  const age = ageWords(ageSeconds);
  if (state === "stale") {
    return age ? `Updated ${age}, retrying` : "Retrying";
  }
  return age ? `Live (${age})` : "Live";
}

function tone(state: FreshnessState) {
  if (state === "disconnected") {
    return { ink: "text-sev-critical", dot: "bg-sev-critical" };
  }
  if (state === "stale") {
    return { ink: "text-sev-warn", dot: "bg-sev-warn" };
  }
  return { ink: "text-sev-ok", dot: "bg-sev-ok" };
}

export function FreshnessIndicator({
  state,
  ageSeconds,
  title,
  className,
}: {
  state: FreshnessState;
  /** Age of the last successful check, in seconds. */
  ageSeconds?: number;
  title?: string;
  className?: string;
}) {
  const { ink, dot } = tone(state);
  return (
    <span
      role="status"
      aria-live="polite"
      data-freshness-state={state}
      title={title}
      className={clsx(
        "inline-flex min-w-0 items-center gap-1.5 whitespace-nowrap text-2xs font-medium",
        ink,
        className,
      )}
    >
      <span aria-hidden="true" className={clsx("size-1.5 shrink-0 rounded-full", dot)} />
      {label(state, ageSeconds)}
    </span>
  );
}
