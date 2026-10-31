import type { KubernetesChange, KubernetesChangesPage, KubernetesCrashEvent, KubernetesCrashPage, KubernetesSync } from "./api";
import { kubernetesAccessLost } from "./kubernetesContinuity";

// Match the existing bounded history scan: five requests, never unlimited history.
export const focusedHistoryRequests = 5;
export type FocusedCoverage = { incomplete_range: boolean; continuation_failed?: boolean };

type Page<T> = { items: T[]; next?: string; sync: KubernetesSync };
function unique<T>(values: T[]): T[] {
  return [...new Map(values.map(value => [JSON.stringify(value), value])).values()];
}

async function collect<T extends { id: string; at: string }, P extends Page<T>>(
  fetch: (cursor?: string) => Promise<P>, signal: AbortSignal, itemBudget: number,
) {
  const pages: P[] = [];
  const items = new Map<string, T>();
  const seen = new Set<string>();
  let next: string | undefined;
  let limited = false;
  let failed = false;
  for (let request = 0; request < focusedHistoryRequests; request++) {
    signal.throwIfAborted();
    let page: P;
    try { page = await fetch(next); }
    catch (error) {
      signal.throwIfAborted();
      if (!pages.length || kubernetesAccessLost(error)) throw error;
      failed = true;
      break;
    }
    signal.throwIfAborted();
    pages.push(page);
    for (const item of page.items) {
      if (items.has(item.id)) continue;
      if (items.size === itemBudget) { limited = true; break; }
      items.set(item.id, item);
    }
    next = page.next || undefined;
    if (!next || limited) break;
    if (seen.has(next)) { limited = true; break; }
    seen.add(next);
  }
  return {
    pages, items: [...items.values()].sort((a, b) => Date.parse(a.at) - Date.parse(b.at) || a.id.localeCompare(b.id)),
    next, incomplete_range: Boolean(next || limited || failed), continuation_failed: failed,
  };
}

function syncOf(pages: Array<{ sync: KubernetesSync }>): KubernetesSync {
  const incomplete = pages.find(page => page.sync.state !== "ready" || page.sync.partial);
  return { ...(incomplete ?? pages[0]).sync, age_s: Math.max(...pages.map(page => page.sync.age_s)),
    partial: pages.some(page => page.sync.partial) };
}

export async function focusedChanges(fetch: (cursor?: string) => Promise<KubernetesChangesPage>, signal: AbortSignal): Promise<KubernetesChangesPage & FocusedCoverage> {
  const { pages, ...result } = await collect<KubernetesChange, KubernetesChangesPage>(fetch, signal, 500);
  return { ...pages[0], ...result, sync: syncOf(pages),
    truncated: result.incomplete_range || pages.some(page => page.truncated),
    gaps: unique(pages.flatMap(page => page.gaps ?? [])),
    partial_failures: unique(pages.flatMap(page => page.partial_failures ?? [])) };
}

export async function focusedCrashes(fetch: (cursor?: string) => Promise<KubernetesCrashPage>, signal: AbortSignal): Promise<KubernetesCrashPage & FocusedCoverage> {
  const { pages, ...result } = await collect<KubernetesCrashEvent, KubernetesCrashPage>(fetch, signal, 250);
  const first = pages[0];
  const captureFailure = pages.find(page => page.capture.evidence_error);
  // Coverage is an intersection: later pages cannot erase an earlier gap.
  const coverage = { ...first.coverage };
  for (const page of pages) {
    for (const kind of Object.keys(coverage)) {
      const a = coverage[kind], b = page.coverage[kind];
      if (!b || !Number.isFinite(Date.parse(a.from)) || !Number.isFinite(Date.parse(b.from))
        || !Number.isFinite(Date.parse(a.to)) || !Number.isFinite(Date.parse(b.to))) { delete coverage[kind]; continue; }
      const from = Math.max(Date.parse(a.from), Date.parse(b.from));
      const to = Math.min(Date.parse(a.to), Date.parse(b.to));
      if (from > to) { delete coverage[kind]; continue; }
      coverage[kind] = { from: new Date(from).toISOString(), to: new Date(to).toISOString() };
    }
  }
  return { ...first, ...result, sync: syncOf(pages), coverage,
    incomplete_range: result.incomplete_range || Boolean(captureFailure),
    crash_count: result.items.filter(item => item.type === "termination").length,
    gaps: unique(pages.flatMap(page => page.gaps ?? [])),
    capture_gaps: unique(pages.flatMap(page => page.capture_gaps ?? [])),
    limitations: unique(pages.flatMap(page => page.limitations ?? [])),
    capture: { ...(captureFailure ?? first).capture, behind: pages.some(page => page.capture.behind),
      mode: pages.every(page => page.capture.mode === first.capture.mode) ? first.capture.mode : "partial" } };
}
