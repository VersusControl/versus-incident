import clsx from "clsx";
import { Loader2 } from "lucide-react";

export function Spinner({ className }: { className?: string }) {
  return (
    <Loader2
      size={14}
      className={clsx("animate-spin text-ink-300", className)}
    />
  );
}

export function EmptyState({
  title,
  hint,
  action,
}: {
  title: string;
  hint?: string;
  /** Optional CTA (button/Link) so empty surfaces lead somewhere. */
  action?: React.ReactNode;
}) {
  return (
    <div className="flex flex-col items-center justify-center gap-1 py-12 text-ink-400">
      {/* Title gets an explicit readable tier — inherited ink-400 measures
          3.7:1 on dark cards, under AA for 14px text. */}
      <div className="text-sm font-medium text-ink-200">{title}</div>
      {hint && <div className="text-xs text-ink-300">{hint}</div>}
      {action && <div className="mt-3">{action}</div>}
    </div>
  );
}

export function ErrorBox({ error }: { error: unknown }) {
  const msg = error instanceof Error ? error.message : String(error);
  return (
    <div className="rounded-md border border-bad/40 bg-bad/5 p-3 text-xs text-bad">
      {msg}
    </div>
  );
}
