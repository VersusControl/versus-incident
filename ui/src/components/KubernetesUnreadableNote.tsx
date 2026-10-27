import type { KubernetesUnreadableSummary } from "@/lib/kubernetesSyncHealth";

/**
 * The kinds a connector identity is not allowed to read.
 *
 * A permission gap is not a sync fault, so it never closes a view — but the data
 * those kinds would carry is genuinely missing, so the note is permanent and
 * quiet rather than a warning the operator can dismiss. It uses the coverage
 * marker's muted dot, names each kind once, and never separates names with a
 * dot.
 */
export function KubernetesUnreadableNote({ kinds }: { kinds: KubernetesUnreadableSummary[] }) {
  if (!kinds.length) return null;
  const names = kinds.map((kind) => kind.kind).join(", ");
  const reasons = [...new Set(kinds.map((kind) => kind.reason))].join(", ");
  return <p role="status" data-unreadable-note className="flex min-w-0 items-start gap-2 text-2xs text-ink-400">
    <span aria-hidden="true" className="mt-1.5 inline-flex size-1.5 shrink-0 rounded-full bg-current" />
    <span className="min-w-0">Not readable with this connector identity: <span className="text-ink-300">{names}</span> ({reasons}). Everything else is shown.</span>
  </p>;
}
