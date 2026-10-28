import type { CountWindow } from "@/lib/api";

// COUNT_WINDOW_LABELS is the single naming of each incident-count lookback:
// `long` for the settings dropdown, `short` for the top bar where space is
// tight. Both live here so the setting and the surfaces that honour it can
// never drift apart.
export const COUNT_WINDOW_LABELS: Record<
  CountWindow,
  { long: string; short: string }
> = {
  "24h": { long: "Last 24 hours", short: "last 24h" },
  "7d": { long: "Last 7 days", short: "last 7d" },
  "30d": { long: "Last 30 days", short: "last 30d" },
  "90d": { long: "Last 90 days", short: "last 90d" },
  all: { long: "All time", short: "all time" },
};

// DEFAULT_COUNT_WINDOW mirrors the server's default so a surface rendering
// before the setting has loaded shows the right thing rather than flickering.
export const DEFAULT_COUNT_WINDOW: CountWindow = "7d";

// countWindowLabel renders a window for display, falling back to the default
// while the setting is still loading or if the server returns an unknown value.
export function countWindowLabel(
  w: CountWindow | string | undefined,
  form: "long" | "short" = "long",
): string {
  const entry =
    COUNT_WINDOW_LABELS[w as CountWindow] ??
    COUNT_WINDOW_LABELS[DEFAULT_COUNT_WINDOW];
  return entry[form];
}
