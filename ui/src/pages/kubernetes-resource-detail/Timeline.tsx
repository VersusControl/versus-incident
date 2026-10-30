import { useState } from "react";
import type { KubernetesChange, KubernetesCrashEvent, KubernetesResource } from "@/lib/api";
import { CoverageMarker } from "@/components/CoverageMarker";
import { CursorPagination } from "@/components/Pagination";
import { useCursorPagination } from "@/lib/pagination";
import { incompleteKubernetesSnapshot } from "@/lib/kubernetesContinuity";
import { clock } from "@/lib/kubernetesResourcePresentation";
import { counted } from "@/lib/kubernetesEvidence";
import { useKubernetesCluster, useKubernetesFirstReady, useKubernetesQuery } from "@/lib/useKubernetesCluster";
import { failureWarnings } from "../kubernetesWarningItems";

export function ResourceTimeline({
  resource,
  onCrash,
}: {
  resource: KubernetesResource;
  onCrash: (crash: KubernetesCrashEvent) => void;
}) {
  const { client, key } = useKubernetesCluster();
  const firstReadyAt = useKubernetesFirstReady();
  const [hours, setHours] = useState(6);
  const [now] = useState(Date.now);
  const rangeStart = hours === 0 ? 0 : now - hours * 3600000;
  const rangeDuration = Math.max(1, now - rangeStart);
  const [generation, setGeneration] = useState(0);
  const paging = useCursorPagination(JSON.stringify([resource.uid, resource.kind, resource.namespace, resource.name, hours, generation]));
  const timeline = useKubernetesQuery({
    queryKey: [
      "resource-detail-timeline",
      resource.uid,
      resource.kind,
      resource.namespace,
      resource.name,
      hours,
      generation,
      paging.cursor,
    ],
    queryFn: ({ signal }) =>
      client.kubernetesTimeline({
        namespace: resource.namespace,
        resource_uid: resource.uid,
        since: new Date(rangeStart).toISOString(),
        until: new Date(now).toISOString(),
        limit: 100,
        cursor: paging.cursor,
      }, signal),
    placeholderData: (previous, query) => JSON.stringify(query?.queryKey.slice(0, -1)) === JSON.stringify(key("resource-detail-timeline", resource.uid, resource.kind, resource.namespace, resource.name, hours, generation)) ? previous : undefined,
    enabled: Boolean(resource.uid),
    retry: false,
    refetchInterval: (query) => query.state.data?.sync.state === "syncing" ? 1000 : false,
  });
  const data = timeline.data;
  const history = (data?.items ?? []).map<{ id: string; at: string; type: string; fields?: KubernetesChange["fields"]; crash?: KubernetesCrashEvent }>((entry) =>
    entry.type === "change" ? { id: entry.id, at: entry.at, type: entry.change.type, fields: entry.change.fields }
      : entry.type === "crash" ? { id: entry.id, at: entry.at, type: "crash", crash: entry.crash }
        : { id: entry.id, at: entry.at, type: "break" },
  );
  const coverageReasons = [
    ...failureWarnings(data?.partial_failures).map((warning) => warning.content),
    ...(data?.gaps?.length ? ["History capture does not cover the whole range"] : []),
    ...(["stale", "error", "partial"].includes(data?.sync.state ?? "") ? ["Some history could not be read"] : []),
    ...(!resource.uid ? ["Resource identity could not be read"] : []),
    ...(timeline.isError ? ["History could not be read"] : []),
  ];
  const syncing = timeline.isPending && timeline.isFetching || (!firstReadyAt && (timeline.unpublished || data?.sync.state === "syncing"));
  return (
    <section aria-label="Resource change history" className="resource-history space-y-3">
      <div className="flex items-center gap-2">
        <div className="history-range flex-wrap" role="group" aria-label="History range">
        {[1, 6, 24, 168, 720, 0].map((value) => (
          <button
            type="button"
            className={`btn ${value === hours ? "bg-accent-subtle text-link" : ""}`}
            key={value}
            aria-pressed={value === hours}
            onClick={() => setHours(value)}
          >
            {value === 0 ? "All retained" : value > 24 ? `${value / 24}d` : `${value}h`}
          </button>
        ))}
        </div>
        <CoverageMarker reasons={coverageReasons} />
        <span className="sr-only">
          {resource.name}
        </span>
      </div>
      {history.length > 0 && <><div className="flex flex-wrap justify-end gap-3 text-2xs text-ink-400" aria-label="History legend"><span><span className="text-link">●</span> change</span><span><span className="text-sev-critical">│</span> break</span><span><span className="text-sev-critical">×</span> crash</span></div>
      <div
        className="relative h-[86px] rounded-control border border-ink-700 bg-surface-sunken px-4"
        aria-label="Resource timeline lane"
      >
        <span className="absolute left-4 top-3 text-2xs uppercase tracking-wide text-ink-400">This {resource.kind}</span>
        <div className="absolute inset-x-4 top-12 border-t border-ink-600" />
        {history.map((item) => (
          <button
            key={item.id}
            type="button"
            aria-label={`${item.type} at ${item.at}`}
            title={`${item.type} at ${item.at}`}
            className={`absolute top-10 flex size-4 items-center justify-center ${item.crash ? "text-sev-critical" : item.type === "break" ? "text-sev-critical" : "text-link"}`}
            style={{
              left: `${Math.max(2, Math.min(96, (100 * (Date.parse(item.at) - rangeStart)) / rangeDuration))}%`,
            }}
            onClick={() =>
              document
                .getElementById(`resource-history-${item.id}`)
                ?.scrollIntoView?.({ block: "center" })
            }
          >{item.crash ? "×" : item.type === "break" ? "│" : "●"}</button>
        ))}
        <div className="absolute inset-x-4 bottom-2 flex justify-between text-2xs text-ink-400">{[0, 1, 2, 3].map((tick) => <span key={tick}>{tick === 3 ? "now" : hours === 0 || hours > 24 ? new Date(rangeStart + rangeDuration * tick / 3).toLocaleDateString([], { month: "short", day: "numeric", year: hours === 0 ? "numeric" : undefined }) : new Date(rangeStart + rangeDuration * tick / 3).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })}</span>)}</div>
      </div></>}
      {syncing && (
        <p role="status" className="text-xs text-ink-400">
          Loading resource history.
        </p>
      )}
      <ol className="space-y-3">
        {history.map((item) => (
          <li
            id={`resource-history-${item.id}`}
            className="space-y-2 rounded-control border border-ink-700 bg-surface-sunken p-3"
            key={item.id}
          >
            <div className="flex flex-wrap items-center gap-2">
              <span
                className={`pill ${item.crash ? "text-sev-critical" : "pill-accent"}`}
              >
                {item.crash ? `${item.crash.type === "termination" ? "Terminated" : "Observed"}: ${item.crash.reason || item.crash.waiting_reason || "Container stopped"}` : item.fields?.some((field) => /(?:^|[./])image$/.test(field.path)) ? "Image changed" : item.type.replaceAll("_", " ")}
              </span>
              <time dateTime={item.at} title={new Date(item.at).toLocaleString()} className="ml-auto text-2xs text-ink-400">
                {clock(item.at)}
              </time>
            </div>
            {item.fields?.map((field) => (
              <p className="break-words font-mono text-2xs" key={field.path}>
                {field.path}{" "}
                <span className="history-diff-from">{field.from || "∅"}</span> →{" "}
                <span className="history-diff-to">{field.to || "∅"}</span>
              </p>
            ))}
            {item.crash && (
              <>
                <p className="text-xs text-ink-300">
                  Container {item.crash.container}, exit {item.crash.exit_code}
                </p>
                <button
                  type="button"
                  className="btn text-link"
                  onClick={() => onCrash(item.crash!)}
                >
                  View logs before crash
                </button>
              </>
            )}
          </li>
        ))}
      </ol>
      {data && !history.length && !data.next && !data.gaps?.length && !timeline.isFetching && !syncing && !timeline.isError && !incompleteKubernetesSnapshot(data) && (
        <p className="py-4 text-xs text-ink-400">
          No changes or crashes in this range.
        </p>
      )}
      {data && data.total > 0 && <p className="text-2xs text-ink-400">{counted(data.total, "event")} and {counted(data.crash_count, "crash")}</p>}
      {!data && !timeline.isPending && !syncing && <p className="py-4 text-xs text-ink-400">History unavailable.</p>}
      {timeline.isError && <button type="button" className="btn text-link" onClick={() => { paging.resetPage(); setGeneration((value) => value + 1); }}>{paging.cursor ? "Restart history" : "Retry history"}</button>}
      <CursorPagination state={paging} busy={timeline.isFetching} next={data?.next} />
    </section>
  );
}

