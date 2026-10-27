// A quiet, non-warning note that more resource types are still loading after
// the core set published. It uses the same dot as the coverage marker.
export function KubernetesPendingNote({ count, kinds, label }: { count?: number; kinds?: string[]; label?: string }) {
  const named = kinds?.filter(Boolean) ?? [];
  if (!label && !named.length && !(count && count > 0)) return null;
  const text = label ? label : named.length ? `Loading ${named.join(", ")}` : `Loading ${count} more resource ${count === 1 ? "type" : "types"}`;
  return <p role="status" data-pending-note className="flex min-w-0 items-center gap-2 text-2xs text-ink-400">
    <span aria-hidden="true" className="inline-flex size-7 shrink-0 items-center justify-center"><span className="size-1.5 rounded-full bg-current" /></span>
    <span className="min-w-0">{text}</span>
  </p>;
}
