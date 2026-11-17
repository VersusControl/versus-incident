import clsx from "clsx";
import {
  Check,
  CircleSlash,
  Loader2,
  Minus,
  X,
} from "lucide-react";
import type { AwsDraftService, AwsServiceStatus } from "@/lib/api";
import {
  AWS_SERVICE_CATALOG,
  SERVICE_STATUS_TONES,
  deriveDraftCounts,
  legendWords,
  notCheckedReason,
  serviceStatusLabel,
  serviceStatusText,
  type AwsGridCounts,
  type AwsTone,
} from "@/lib/awsSettings";

const TONE_TEXT: Record<AwsTone, string> = {
  ok: "text-sev-ok",
  warn: "text-sev-warn",
  critical: "text-sev-critical",
  muted: "text-ink-300",
};

/** Every row's reason when the role was never assumed, so no probe ran. */
export const ASSUME_FAILED_REASON = "Role could not be assumed";

// Every result carries an icon AND a word. The icon only repeats the word, so
// a colour-blind operator or a greyscale screenshot loses nothing.
function StatusIcon({
  status,
  running,
}: {
  status: AwsServiceStatus | "checking";
  /** A settled screen (a failed assume) never animates. */
  running: boolean;
}) {
  const size = 13;
  if (status === "checking") {
    return (
      <Loader2
        size={size}
        aria-hidden="true"
        className="shrink-0 animate-spin text-ink-300 motion-reduce:animate-none"
      />
    );
  }
  switch (status) {
    case "readable":
      return <Check size={size} aria-hidden="true" className="shrink-0 text-sev-ok" />;
    case "denied":
      return <X size={size} aria-hidden="true" className="shrink-0 text-sev-critical" />;
    case "not_checked":
      return running ? (
        <Loader2 size={size} aria-hidden="true" className="shrink-0 animate-spin text-sev-warn motion-reduce:animate-none" />
      ) : (
        <CircleSlash size={size} aria-hidden="true" className="shrink-0 text-sev-warn" />
      );
    case "not_on_plan":
      return <CircleSlash size={size} aria-hidden="true" className="shrink-0 text-ink-300" />;
    default:
      return <Minus size={size} aria-hidden="true" className="shrink-0 text-ink-300" />;
  }
}

function ResultRow({
  label,
  status,
  errorCode,
  assumeFailed,
}: {
  label: string;
  status: AwsServiceStatus | "checking";
  errorCode?: string | null;
  assumeFailed: boolean;
}) {
  const reason =
    status === "not_checked"
      ? assumeFailed
        ? ASSUME_FAILED_REASON
        : notCheckedReason(errorCode)
      : null;
  return (
    <div className="flex min-w-0 flex-col gap-0.5">
      <span className="flex min-w-0 items-center gap-1.5">
        <StatusIcon status={status} running={!assumeFailed} />
        <span className="min-w-0 truncate text-ink-100" title={label}>
          {label}
        </span>
        <span
          className={clsx(
            "shrink-0 text-2xs",
            status === "checking"
              ? "text-ink-300"
              : TONE_TEXT[SERVICE_STATUS_TONES[status]],
          )}
        >
          {status === "checking" ? "Checking…" : serviceStatusLabel(status, errorCode)}
        </span>
      </span>
      {reason && <span className="text-2xs text-ink-300">{reason}</span>}
    </div>
  );
}

// AwsServiceAccessGrid — the 24 probe results, in catalog order. Results that
// have not landed yet read "Checking…", so the grid never shows a fake zero or
// a premature Denied. The live region announces each result as it arrives.
export function AwsServiceAccessGrid({
  services = [],
  columns = 4,
  className,
  counts: countsOverride,
  assumeFailed = false,
}: {
  services?: AwsDraftService[];
  columns?: 2 | 4;
  className?: string;
  counts?: AwsGridCounts;
  /** The role was never assumed, so every service is settled as not checked. */
  assumeFailed?: boolean;
}) {
  const counts =
    countsOverride ??
    (assumeFailed
      ? {
          total: AWS_SERVICE_CATALOG.length,
          readable: 0,
          not_used: 0,
          denied: 0,
          not_on_plan: 0,
          not_checked: AWS_SERVICE_CATALOG.length,
          pending: 0,
        }
      : deriveDraftCounts(services));
  const latest = services[services.length - 1];
  const settled = counts.total - counts.pending;

  return (
    <div className={clsx("min-w-0", className)}>
      <ul
        aria-label="Service access"
        className={clsx(
          "grid gap-x-4 gap-y-1.5 text-xs",
          columns === 4
            ? "grid-cols-2 lg:grid-cols-4"
            : "grid-cols-1 sm:grid-cols-2",
        )}
      >
        {AWS_SERVICE_CATALOG.map((meta) => {
          const result = services.find((s) => s.service === meta.key);
          const status: AwsServiceStatus | "checking" = assumeFailed
            ? "not_checked"
            : result?.status ?? "checking";
          return (
            <li
              key={meta.key}
              aria-label={
                assumeFailed
                  ? `${meta.label}: Not checked: ${ASSUME_FAILED_REASON}`
                  : result
                    ? serviceStatusText(meta.key, result.status, result.error_code)
                    : `${meta.label}: checking`
              }
              className="min-w-0"
            >
              <ResultRow
                label={meta.label}
                status={status}
                errorCode={assumeFailed ? undefined : result?.error_code}
                assumeFailed={assumeFailed}
              />
            </li>
          );
        })}
      </ul>
      <p
        role="status"
        aria-live="polite"
        className="sr-only"
        data-testid="access-progress"
      >
        {assumeFailed
          ? `${counts.total} of ${counts.total} services checked. ${ASSUME_FAILED_REASON}`
          : latest
            ? `${settled} of ${counts.total} services checked. ${
                latest.status === "not_checked"
                  ? serviceStatusText(latest.service, latest.status, latest.error_code)
                  : serviceStatusText(latest.service, latest.status)
              }`
            : "Checking service access"}
      </p>
      <p className="mt-2 text-2xs text-ink-300">
        <span className="sr-only">Service results: </span>
        {legendWords(counts)}
      </p>
    </div>
  );
}

/** Visible legend with the same counts, for the panel's own reading order. */
export function AwsServiceLegend({ counts }: { counts: AwsGridCounts }) {
  return (
    <p className="text-2xs text-ink-300">
      <span className="sr-only">Service results: </span>
      {legendWords(counts)}
    </p>
  );
}
