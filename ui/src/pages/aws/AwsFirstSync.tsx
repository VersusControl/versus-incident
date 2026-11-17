import { useState } from "react";
import clsx from "clsx";
import { AlertTriangle, CheckCircle2, Clock, Loader2 } from "lucide-react";
import type { AwsAccountSync } from "@/lib/api";
import { syncProgress, type SyncWordKind } from "@/lib/awsSettings";

// The live first sync of a newly connected account: services fill in one by
// one (Ready / Loading / Waiting), "N of M ready", and a gap is named in
// words. A service with nothing yet shows a word, never a zero count.

const ICON: Record<SyncWordKind, typeof CheckCircle2> = {
  ready: CheckCircle2,
  loading: Loader2,
  waiting: Clock,
  failed: AlertTriangle,
};

const TONE: Record<SyncWordKind, string> = {
  ready: "text-sev-ok",
  loading: "text-ink-200",
  waiting: "text-ink-300",
  failed: "text-sev-warn",
};

export function AwsFirstSync({ sync, name }: { sync: AwsAccountSync; name: string }) {
  // The "of M" never shrinks between polls, even if a later answer lists
  // fewer services for a moment.
  const [peak, setPeak] = useState(0);
  const progress = syncProgress(sync, peak);
  if (progress.total > peak) setPeak(progress.total);

  return (
    <div className="mt-2 rounded-control border border-ink-500/60 bg-surface-sunken/50 px-3 py-2">
      <p role="status" aria-label={`${name} first sync: ${progress.label}`} className="flex items-center gap-2 text-xs">
        <span className="font-medium text-ink-100">First sync</span>
        <span className="text-ink-300">{progress.label}</span>
      </p>
      {progress.rows.length > 0 && (
        <ul aria-label={`${name} services`} className="mt-1.5 grid grid-cols-1 gap-x-4 gap-y-1 sm:grid-cols-2 lg:grid-cols-3">
          {progress.rows.map((r) => {
            const Icon = ICON[r.kind];
            return (
              <li key={r.service} aria-label={`${r.label}: ${r.word}`} className="flex min-w-0 items-baseline gap-2 text-2xs">
                <span className="min-w-0 flex-1 truncate text-ink-100">{r.label}</span>
                <span className={clsx("inline-flex items-center gap-1 whitespace-nowrap", TONE[r.kind])}>
                  <Icon
                    size={11}
                    aria-hidden="true"
                    className={r.kind === "loading" ? "animate-spin motion-reduce:animate-none" : undefined}
                  />
                  {r.word}
                </span>
                {r.detail && <span className="whitespace-nowrap text-ink-400">{r.detail}</span>}
                {r.gaps.length > 0 && <span className="whitespace-nowrap text-ink-300">{r.gaps.join(" · ")}</span>}
              </li>
            );
          })}
        </ul>
      )}
    </div>
  );
}
