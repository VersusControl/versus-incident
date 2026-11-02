import type { InfiniteData } from "@tanstack/react-query";
import type { KubernetesBreakLogPage } from "./api";

export type BreakLogData = InfiniteData<KubernetesBreakLogPage, string | undefined>;
// Keep transport pages separate from the append-only presentation page.
// Collapsing the display must not turn a replay into a newly fetched page.
const provenance = new WeakMap<BreakLogData, BreakLogData>();

const linkedIdentity = (page: KubernetesBreakLogPage) => JSON.stringify(page.sources.filter(source => source.kind === "linked").map(source => [source.name, source.type]).sort());
const lineIdentity = (line: KubernetesBreakLogPage["lines"][number]) => JSON.stringify([line.at, line.pod, line.pod_uid, line.container, line.init, line.slot, line.level, line.text, line.source]);

/** A replay may overlap a prior page, but repeated identical records are real evidence. */
export function appendBreakLogLines(previous: KubernetesBreakLogPage["lines"], incoming: KubernetesBreakLogPage["lines"]) {
  const counts = new Map<string, number>();
  for (const line of previous) {
    const id = lineIdentity(line);
    counts.set(id, (counts.get(id) ?? 0) + 1);
  }
  const seen = new Map<string, number>();
  const added = incoming.filter(line => {
    const id = lineIdentity(line), count = (seen.get(id) ?? 0) + 1;
    seen.set(id, count);
    return count > (counts.get(id) ?? 0);
  });
  return [...previous, ...added];
}

/** Replacement coverage is authoritative; a mature empty read cannot erase useful lines. */
export function mergeBreakLogData(previous: BreakLogData | undefined, incoming: BreakLogData): BreakLogData {
  const prior = previous?.pages[0], next = incoming.pages[0];
  if (!prior || !next || prior.occurrence_id !== next.occurrence_id
    || prior.window.from !== next.window.from || prior.window.to !== next.window.to
    || linkedIdentity(prior) !== "[]" && linkedIdentity(next) !== "[]" && linkedIdentity(prior) !== linkedIdentity(next)) return incoming;
  const transport = provenance.get(previous!) ?? previous!;
  const appending = incoming.pages.length > 1;
  // A pending first-page replay cannot rewind a live continuation. Once the
  // old chain is exhausted a new first-page chain may overlap retained lines.
  const advanced = !appending && transport.pages.length > 1 && Boolean(prior.next);
  const raw: BreakLogData = appending
    ? { pages: [...transport.pages, ...incoming.pages.slice(1)], pageParams: [...transport.pageParams, ...incoming.pageParams.slice(1)] }
    : { pages: [next, ...(advanced ? transport.pages.slice(1) : [])], pageParams: [undefined, ...(advanced ? transport.pageParams.slice(1) : [])] };
  const lines = appendBreakLogLines(previous!.pages.flatMap(page => page.lines ?? []), raw.pages.flatMap(page => page.lines ?? []));
  const result: BreakLogData = { pages: [{ ...next, next: advanced ? prior.next : raw.pages.at(-1)?.next, lines, markers: prior.markers, sources: next.sources.length ? next.sources : prior.sources, pods: next.pods.length ? next.pods : prior.pods,
    containers: next.containers?.length ? next.containers : prior.containers }], pageParams: [undefined] };
  provenance.set(result, raw);
  return result;
}
