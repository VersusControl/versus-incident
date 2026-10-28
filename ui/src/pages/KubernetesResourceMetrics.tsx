import { useId, useState, type ReactNode } from "react";
import { useQueries } from "@tanstack/react-query";
import type {
  KubernetesCrashEvent,
  KubernetesResource,
  KubernetesWorkload,
} from "@/lib/api";
import { useKubernetesCluster } from "@/lib/useKubernetesCluster";
import { CursorPagination } from "@/components/Pagination";
import {
  detailText,
  quantity,
  resourceStart,
  scopedCrashes,
  useResourceCrashes,
  metricThreshold,
  metricAllocation,
  mergeUsageHistory,
  type ResourceMetricSample,
  resourceRestarts,
} from "@/lib/kubernetesResourceDetail";
import { counted } from "@/lib/kubernetesEvidence";
function metricValue(value: number | null, field: "cpu" | "memory") {
  if (value === null) return "Unavailable";
  if (field === "cpu") return `${Math.round(value * 1000)} mCPU`;
  return value >= 1024 ** 3
    ? `${(value / 1024 ** 3).toFixed(2)} GiB`
    : `${(value / 1024 ** 2).toFixed(1)} MiB`;
}
export function ResourceMetrics({
  resource,
  workload,
  liveSamples,
  onCrash,
}: {
  resource: KubernetesResource;
  workload?: KubernetesWorkload;
  liveSamples: ResourceMetricSample[];
  onCrash: (crash: KubernetesCrashEvent) => void;
}) {
  const { client, key, cluster } = useKubernetesCluster();
  const [range, setRange] = useState(60);
  const [chartVisible, setChartVisible] = useState(false);
  const now = Date.now();
  const podNames =
    resource.kind === "Pod"
      ? [resource.name]
      : (workload?.pods?.map((pod) => pod.name) ?? []);
  const scope = JSON.stringify([
    cluster,
    resource.uid,
    resource.kind,
    resource.namespace,
    resource.name,
  ]);
  const [tracked, setTracked] = useState({
    scope,
    names: podNames.slice(0, 2000),
  });
  // Keep departed series for the open resource so rollout membership cannot erase capture history.
  const historyNames = [
    ...new Set([
      ...podNames,
      ...(tracked.scope === scope ? tracked.names : []),
    ]),
  ].slice(0, 2000);
  if (
    tracked.scope !== scope ||
    historyNames.join("\0") !== tracked.names.join("\0")
  ) {
    setTracked({ scope, names: historyNames });
  }
  const history = useQueries({
    queries: historyNames.map((pod) => ({
      queryKey: key("resource-usage-history", scope, pod),
      queryFn: ({ signal }: { signal: AbortSignal }) =>
        client.kubernetesUsageHistory(resource.namespace ?? "", pod, signal),
      retry: false,
      refetchInterval: 60000,
      enabled: podNames.includes(pod),
    })),
  });
  const merged = mergeUsageHistory(
    history.map((query) =>
      (query.isError ? [] : (query.data?.samples ?? []))
        .map((sample) => ({
          observedAt: Date.parse(sample.timestamp),
          cpu: sample.cpu_milli / 1000,
          memory: sample.memory_bytes,
        }))
        .filter((sample) => Number.isFinite(sample.observedAt)),
    ),
  );
  const samples = (merged.length ? merged : liveSamples).filter(
    (sample) =>
      sample.observedAt >= now - range * 60000 && sample.observedAt <= now,
  );
  const last = samples.at(-1);
  const currentValue = (field: "cpu" | "memory") => {
    if (last?.[field] !== undefined && last[field] !== null) return last[field];
    const usage = workload?.usage ?? [];
    if (podNames.some((name) => !usage.some((pod) => pod.name === name)))
      return null;
    const amounts = usage.map((pod) => quantity(pod[field]));
    return amounts.length && amounts.every((amount) => amount !== null)
      ? amounts.reduce<number>((sum, amount) => sum + (amount ?? 0), 0)
      : null;
  };
  const crashesQuery = useResourceCrashes(resource);
  const crashes = scopedCrashes(
    crashesQuery.isError ? undefined : crashesQuery.data,
    resource,
  ).filter((crash) => crash.type === "termination");
  const lastCrash = [...crashes].sort(
    (a, b) => Date.parse(b.at) - Date.parse(a.at),
  )[0];
  const start = resourceStart(resource, workload);
  const elapsedMinutes =
    typeof start === "string"
      ? Math.max(0, Math.floor((now - Date.parse(start)) / 60000))
      : null;
  const restarts = resourceRestarts(resource, workload);
  const mainContainer = typeof resource.summary?.main_container === "string" ? resource.summary.main_container : workload?.containers?.length === 1 ? workload.containers[0].name : null;
  return (
    <section aria-label="Live workload metrics" className="resource-metrics space-y-4">
      <div className="metric-tiles grid grid-cols-2 gap-3 sm:grid-cols-4">
        {(["cpu", "memory"] as const).map((field) => {
          const limit = metricAllocation(workload, field, "limits", resource);
          const value = currentValue(field);
          const coverage =
            last?.[field] !== null && last?.[field] !== undefined
              ? last.coverage
              : undefined;
          const partial =
            coverage !== undefined &&
            (coverage[field] < coverage.total ||
              coverage.total !== podNames.length);
          const percent =
            !partial && value !== null && limit !== null && limit > 0
              ? (value / limit) * 100
              : null;
          return (
            <div
              key={field}
              className={`metric-tile relative min-w-0 rounded-control border bg-surface-sunken p-3 ${percent !== null && percent > 90 ? "border-sev-critical/50" : percent !== null && percent > 75 ? "border-sev-warning/50" : "border-ink-700"}`}
              aria-label={`${field === "cpu" ? "CPU" : "Memory"} metric tile`}
            >
              <h3 className="text-2xs text-ink-300"><button type="button" className="metric-history-toggle" aria-expanded={chartVisible} aria-controls="resource-usage-chart" onClick={() => setChartVisible(!chartVisible)}>{field === "cpu" ? "CPU" : "Memory"}<span className="sr-only"> usage history</span></button></h3>
              <p
                aria-label={metricValue(value, field)}
                className={`mt-2 text-lg font-semibold ${metricThreshold(value === null ? null : (percent ?? 0))}`}
              >
                {value === null ? "Unavailable" : field === "cpu" ? `${Math.round(value * 1000)}m` : metricValue(value, field).replace(/\.0(?= MiB)/, "")}
                <span className="metric-allocation">of {field === "cpu" && limit !== null && limit >= 1 ? `${limit} ${limit === 1 ? "core" : "cores"}` : metricValue(limit, field)}</span>
              </p>
              {partial && (
                <p className="text-2xs text-ink-400">
                  Available samples {coverage[field]} of {coverage.total} tracked
                  Pods
                </p>
              )}
                {percent !== null && (
                  <span
                    className={`metric-corner ${percent <= 75 ? "text-link" : metricThreshold(percent)}`}
                  >
                    {Math.round(percent)}%
                  </span>
                )}
              <Sparkline samples={samples} field={field} warning={percent !== null && percent > 75} gapMs={merged.length ? 150000 : 37500} limit={limit} />
            </div>
          );
        })}
        <div aria-label="Restarts metric tile" className="metric-tile rounded-control border border-ink-700 bg-surface-sunken p-3">
          <h3 className="text-2xs text-ink-300">Restarts</h3>
          <span className="metric-corner text-ink-400">24 h</span>
          <p
            className={`mt-2 text-lg font-semibold ${restarts === null ? "text-ink-400" : "text-ink-50"}`}
          >
            {restarts ?? "Unavailable"}
            <span className="metric-restart-description">{crashesQuery.isError ? "Crash evidence unavailable" : lastCrash ? `shown ${lastCrash.reason || lastCrash.waiting_reason || "Termination"} ${new Date(lastCrash.at).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", hour12: false })}` : crashesQuery.isFetching && !crashesQuery.data ? "Loading captured crashes" : "No captured crash on this page"}</span>
          </p>
          <div
            className="relative mt-3 h-6 border-b border-ink-700"
            aria-label="24 hour crash ticks"
          >
            {crashes
              .filter((crash) => Date.parse(crash.at) >= now - 86400000)
              .map((crash) => (
                <button
                  type="button"
                  aria-label={`Crash at ${crash.at}`}
                  onClick={() => onCrash(crash)}
                  key={crash.id}
                  className="absolute bottom-0 h-4 w-1 bg-sev-critical"
                  style={{
                    left: `${Math.max(0, Math.min(99, (Date.parse(crash.at) - now + 86400000) / 864000))}%`,
                  }}
                />
              ))}
          </div>
          <p className="mt-1 text-2xs text-ink-500">24h crash journal</p>
        </div>
        <div aria-label="Uptime metric tile" className="metric-tile rounded-control border border-ink-700 bg-surface-sunken p-3">
          <h3 className="text-2xs text-ink-300">Uptime</h3>
          {resource.kind === "Pod" && mainContainer && <span title={mainContainer} className="metric-container-label block max-w-full truncate text-2xs text-ink-400">{mainContainer}</span>}
          <p className="mt-2 text-lg font-semibold">
            {elapsedMinutes !== null && Number.isFinite(elapsedMinutes)
              ? `${Math.floor(elapsedMinutes / 60)}h ${elapsedMinutes % 60}m`
              : "Unavailable"}
          </p>
          <p className="mt-1 text-2xs text-ink-400">
            since restart at{" "}
            {typeof start === "string"
              ? <time dateTime={start} title={new Date(start).toLocaleString()}>{new Date(start).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", hour12: false })}</time>
              : "Unavailable"}
          </p>
        </div>
      </div>
      {!crashesQuery.isError && crashesQuery.data && (crashesQuery.data.crash_count ?? 0) > 0 && <nav aria-label="Captured crash pages" className="text-2xs text-ink-400">
        <p>{`${counted(crashesQuery.data.crash_count ?? 0, "captured crash")} in the last 24h`}</p>
        {(crashesQuery.data.next || crashesQuery.paging.canPrevious) && <CursorPagination state={crashesQuery.paging} busy={crashesQuery.isFetching} next={crashesQuery.data.next} />}
      </nav>}
      <section
        id="resource-usage-chart"
        data-mobile-open={chartVisible}
        className="resource-usage-chart rounded-control border border-ink-700 bg-surface-sunken p-3"
        aria-label="CPU and memory history"
      >
        <div className="mt-2">
            <UsageChart
              rangeControl={<div role="group" aria-label="Usage history range" className="mt-2 flex min-w-0 flex-wrap items-center justify-end gap-2">
          {[15, 60, 360].map((minutes) => (
            <button
              className="btn"
              type="button"
              key={minutes}
              aria-pressed={range === minutes}
              onClick={() => setRange(minutes)}
            >
              {minutes === 15 ? "15m" : `${minutes / 60}h`}
            </button>
          ))}
          {!merged.length && (
            <span className="text-2xs text-ink-400">since opened</span>
          )}
          </div>}
              samples={samples}
              start={
                merged.length
                  ? now - range * 60000
                  : Math.max(now - range * 60000, samples[0]?.observedAt ?? now)
              }
              now={now}
              allocations={{
                cpu: { request: metricAllocation(workload, "cpu", "requests", resource), limit: metricAllocation(workload, "cpu", "limits", resource) },
                memory: { request: metricAllocation(workload, "memory", "requests", resource), limit: metricAllocation(workload, "memory", "limits", resource) },
              }}
              crashes={crashes}
              onCrash={onCrash}
              gapMs={merged.length ? 150000 : 37500}
              containers={workload?.containers}
            />
        </div>
        {!samples.length && (
          <p role="status" className="py-3 text-xs text-ink-400">
            {history.some((query) => query.isPending)
              ? "Loading usage history."
              : "Metrics unavailable. Waiting for a live sample."}
          </p>
        )}
      </section>
    </section>
  );
}
function Sparkline({
  samples,
  field,
  warning,
  gapMs,
  limit,
}: {
  samples: ResourceMetricSample[];
  field: "cpu" | "memory";
  warning: boolean;
  gapMs: number;
  limit: number | null;
}) {
  const gradient = useId();
  const values = samples.filter((sample) => sample[field] !== null);
  const max = Math.max(0.001, limit ?? 0, ...values.map((sample) => sample[field]!));
  const from = values[0]?.observedAt ?? 0;
  const span = Math.max(1, (values.at(-1)?.observedAt ?? from) - from);
  const segments: Array<Array<{ x: number; y: number }>> = [];
  let previous: number | null = null;
  for (const sample of samples) {
    if (sample[field] === null) { previous = null; continue; }
    if (previous === null || sample.observedAt - previous > gapMs) segments.push([]);
    segments.at(-1)!.push({ x: (sample.observedAt - from) / span * 100, y: 22 - sample[field]! / max * 20 });
    previous = sample.observedAt;
  }
  return (
    <svg
      viewBox="0 0 100 24"
      className={`mt-2 h-6 w-full ${warning ? "text-sev-warning" : "text-link"}`}
      role="img"
      aria-label={`${field} sparkline`}
      preserveAspectRatio="none"
    >
      <defs><linearGradient id={gradient} x1="0" y1="0" x2="0" y2="1"><stop stopColor="currentColor" stopOpacity=".3" /><stop offset="1" stopColor="currentColor" stopOpacity="0" /></linearGradient></defs>
      {warning && limit !== null && limit > 0 && <line x1="0" x2="100" y1={22 - limit / max * 20} y2={22 - limit / max * 20} className="text-sev-critical" stroke="currentColor" strokeDasharray="3 3" strokeOpacity=".7" />}
      {segments.map((points, index) => {
        const path = points.map((point, i) => `${i ? "L" : "M"}${point.x},${point.y}`).join(" ");
        return <g key={index}>
          {points.length > 1 ? <><path d={`${path} L${points.at(-1)!.x},24 L${points[0].x},24 Z`} fill={`url(#${gradient})`} /><path data-sparkline-series={field} d={path} fill="none" stroke="currentColor" strokeWidth="1.5" /></> : <circle data-sparkline-series={field} cx={points[0].x} cy={points[0].y} r="1.5" fill="currentColor" />}
        </g>;
      })}
    </svg>
  );
}
function UsageChart({
  samples,
  start,
  now,
  allocations,
  crashes,
  onCrash,
  gapMs,
  containers,
  rangeControl,
}: {
  samples: ResourceMetricSample[];
  start: number;
  now: number;
  allocations: Record<"cpu" | "memory", { request: number | null; limit: number | null }>;
  crashes: KubernetesCrashEvent[];
  onCrash: (crash: KubernetesCrashEvent) => void;
  gapMs: number;
  containers?: KubernetesWorkload["containers"];
  rangeControl: ReactNode;
}) {
  const [field, setField] = useState<"cpu" | "memory">("memory");
  const gradient = useId();
  const { request, limit } = allocations[field];
  const [activeTime, setActiveTime] = useState<number | null>(null);
  const validSamples = samples.filter((sample) => sample[field] !== null);
  const hasScale = validSamples.length > 0 || request !== null || limit !== null;
  const showCoverage = samples.some((sample) => sample.coverage);
  const active = validSamples.find(
    (sample) => sample.observedAt === activeTime,
  );
  const maxima = {
    cpu: Math.max(0.001, allocations.cpu.limit ?? 0, allocations.cpu.request ?? 0, ...samples.map((sample) => sample.cpu ?? 0)),
    memory: Math.max(1, allocations.memory.limit ?? 0, allocations.memory.request ?? 0, ...samples.map((sample) => sample.memory ?? 0)),
  };
  const maximum = (metric: "cpu" | "memory") => maxima[metric];
  const x = (at: number) =>
    48 +
    Math.max(0, Math.min(1, (at - start) / Math.max(1, now - start))) * 720;
  const yFor = (amount: number, metric: "cpu" | "memory") => 112 - (amount / maximum(metric)) * 90;
  const seriesPath = (metric: "cpu" | "memory") => {
    let previous = 0;
    return samples
    .map((sample) => {
      if (sample[metric] === null) {
        previous = 0;
        return "";
      }
      const prefix =
        previous && sample.observedAt - previous <= gapMs ? "L" : "M";
      previous = sample.observedAt;
      return `${prefix} ${x(sample.observedAt)},${yFor(sample[metric]!, metric)}`;
    })
    .join(" ");
  };
  const memorySegments: ResourceMetricSample[][] = [];
  let previousMemory: number | null = null;
  for (const sample of samples) {
    if (sample.memory === null) { previousMemory = null; continue; }
    if (previousMemory === null || sample.observedAt - previousMemory > gapMs) memorySegments.push([]);
    memorySegments.at(-1)!.push(sample);
    previousMemory = sample.observedAt;
  }
  const title = field === "cpu" ? "CPU" : "Memory";
  const activeDescription = active
    ? `${new Date(active.observedAt).toLocaleTimeString()}: ${metricValue(active[field], field)}${active.coverage ? ` with ${active.coverage[field]} of ${active.coverage.total} tracked Pods sampled` : ""}`
    : "Use arrow keys or hover to inspect usage samples.";
  return (
    <figure aria-label="CPU and memory usage chart">
      <figcaption className="flex flex-wrap items-center gap-x-3 text-2xs text-ink-400">
        <h3 className="min-w-0 text-xs font-semibold text-ink-100">CPU and memory</h3>
        {(["cpu", "memory"] as const).map((metric) => <button key={metric} type="button" aria-label={`Inspect ${metric === "cpu" ? "CPU" : "Memory"} history`} aria-pressed={metric === field} onClick={() => setField(metric)} className={metric === "cpu" ? "text-link" : "text-sev-warning"}>━ {metric === "cpu" ? "CPU" : "Memory"}</button>)}
        <span className="ml-auto">┄ Request</span><span className="text-sev-critical">┄ Limit</span><span className="text-sev-critical">× crash</span>
        <span className="sr-only">Request {metricValue(request, field)}; Limit {metricValue(limit, field)}. Each series uses its own scale.</span>
      </figcaption>
      {rangeControl}
      <div
        tabIndex={0}
        aria-label={`Inspect ${title} samples`}
        aria-describedby={`live-${field}-metric-value`}
        onFocus={() => setActiveTime(validSamples.at(-1)?.observedAt ?? null)}
        onBlur={() => setActiveTime(null)}
        onKeyDown={(event) => {
          if (
            !["ArrowLeft", "ArrowRight", "Home", "End"].includes(event.key) ||
            !validSamples.length
          )
            return;
          event.preventDefault();
          const index = Math.max(
            0,
            validSamples.findIndex(
              (sample) => sample.observedAt === activeTime,
            ),
          );
          const next =
            event.key === "Home"
              ? 0
              : event.key === "End"
                ? validSamples.length - 1
                : Math.max(
                    0,
                    Math.min(
                      validSamples.length - 1,
                      index + (event.key === "ArrowRight" ? 1 : -1),
                    ),
                  );
          setActiveTime(validSamples[next].observedAt);
        }}
      >
        <span
          id={`live-${field}-metric-value`}
          className="sr-only"
          aria-live="polite"
        >
          {activeDescription}
        </span>
        <svg
          className="h-auto w-full text-ink-400"
          preserveAspectRatio="none"
          viewBox="0 0 792 142"
          role="img"
          aria-label={`Workload CPU and memory collected since ${new Date(start).toLocaleTimeString()}, independently scaled with request, limit and crash markers`}
          onPointerMove={(event) => {
            if (!validSamples.length) return;
            const bounds = event.currentTarget.getBoundingClientRect();
            const at =
              start +
              Math.max(
                0,
                Math.min(
                  1,
                  (((event.clientX - bounds.left) / Math.max(1, bounds.width)) *
                    792 -
                    48) /
                    720,
                ),
              ) *
                (now - start);
            setActiveTime(
              validSamples.reduce((nearest, sample) =>
                Math.abs(sample.observedAt - at) <
                Math.abs(nearest.observedAt - at)
                  ? sample
                  : nearest,
              ).observedAt,
            );
          }}
          onPointerLeave={() => setActiveTime(null)}
        >
          <defs><linearGradient id={gradient} className="text-sev-warning" x1="0" y1="0" x2="0" y2="1"><stop stopColor="currentColor" stopOpacity=".28" /><stop offset="1" stopColor="currentColor" stopOpacity="0" /></linearGradient></defs>
          <g stroke="currentColor" strokeOpacity=".2">
            {[22, 67, 112].map((line) => (
              <line key={line} x1="48" x2="768" y1={line} y2={line} />
            ))}
          </g>
          {memorySegments.filter((segment) => segment.length > 1).map((segment, index) => <path data-memory-area key={index} className="text-sev-warning" fill={`url(#${gradient})`} d={`${segment.map((sample, i) => `${i ? "L" : "M"}${x(sample.observedAt)},${yFor(sample.memory!, "memory")}`).join(" ")} L${x(segment.at(-1)!.observedAt)},112 L${x(segment[0].observedAt)},112 Z`} />)}
          {(["cpu", "memory"] as const).map((metric) => <g key={metric} className={metric === "cpu" ? "text-link" : "text-sev-warning"}>
          {allocations[metric].request !== null && (
            <line
              data-reference="request"
              x1="48"
              x2="768"
              y1={yFor(allocations[metric].request!, metric)}
              y2={yFor(allocations[metric].request!, metric)}
              stroke="currentColor"
              strokeDasharray="4 4"
              strokeOpacity={metric === field ? .8 : 0}
            >
              <title>{metric} request {metricValue(allocations[metric].request, metric)}</title>
            </line>
          )}
          {allocations[metric].limit !== null && (
            <line
              data-reference="limit"
              className="text-sev-critical"
              x1="48"
              x2="768"
              y1={yFor(allocations[metric].limit!, metric)}
              y2={yFor(allocations[metric].limit!, metric)}
              stroke="currentColor"
              strokeDasharray="2 3"
              strokeOpacity={metric === field ? .8 : 0}
            >
              <title>{metric} limit {metricValue(allocations[metric].limit, metric)}</title>
            </line>
          )}
          <path
            data-series={metric}
            d={seriesPath(metric)}
            fill="none"
            stroke="currentColor"
            strokeWidth="2"
            className={metric === "cpu" ? "text-link" : "text-sev-warning"}
          />
          {samples.filter((sample) => sample[metric] !== null).map((sample) => (
            <circle
              key={sample.observedAt}
              cx={x(sample.observedAt)}
              cy={yFor(sample[metric]!, metric)}
              r="2"
              fill="currentColor"
            >
              <title>
                {new Date(sample.observedAt).toLocaleTimeString()}{" "}
                {metricValue(sample[metric], metric)}
              </title>
            </circle>
          ))}
          </g>)}
          <line data-now-marker x1={x(now)} x2={x(now)} y1="18" y2="114" stroke="currentColor" strokeDasharray="3 3" strokeOpacity=".6" />
          {crashes
            .filter(
              (crash) =>
                Date.parse(crash.at) >= start && Date.parse(crash.at) <= now,
            )
            .map((crash) => (
              <g
                key={crash.id}
                role="button"
                tabIndex={0}
                aria-label={`View crash at ${crash.at}`}
                className="cursor-pointer text-sev-critical"
                onClick={() => onCrash(crash)}
                onKeyDown={(event) => {
                  if (event.key === "Enter" || event.key === " ") {
                    event.preventDefault();
                    event.stopPropagation();
                    onCrash(crash);
                  }
                }}
              >
                <title>
                  {crash.reason || "Crash"} at {crash.at}
                </title>
                <line
                  data-crash-marker={crash.id}
                  x1={x(Date.parse(crash.at))}
                  x2={x(Date.parse(crash.at))}
                  y1="18"
                  y2="114"
                  stroke="currentColor"
                  strokeDasharray="2 3"
                />
                <path d={`M${x(Date.parse(crash.at)) - 4} 14l8 8m0 -8l-8 8`} stroke="currentColor" strokeWidth="2" />
              </g>
            ))}
          {active && (
            <g data-testid={`${field}-tooltip`}>
              <line
                x1={x(active.observedAt)}
                x2={x(active.observedAt)}
                y1="20"
                y2="112"
                stroke="currentColor"
                strokeDasharray="3 3"
              />
              <rect
                x={Math.max(48, Math.min(590, x(active.observedAt) - 60))}
                y="2"
                width="175"
                height="20"
                className="fill-surface-raised"
              />
              <text
                x={Math.max(53, Math.min(595, x(active.observedAt) - 55))}
                y="16"
                fontSize="10"
                fill="currentColor"
              >
                {new Date(active.observedAt).toLocaleTimeString()}:{" "}
                {metricValue(active[field], field)}
                <title>{activeDescription}</title>
              </text>
            </g>
          )}
          <g fill="currentColor" fontSize="10">
            {hasScale && <><text x="0" y="24">{field === "cpu" ? `${Math.round(maximum(field) * 1000)}m` : metricValue(maximum(field), field).replace(/\.0 MiB/, " Mi").replace(" MiB", " Mi").replace(" GiB", " Gi")}</text>
            <text x="0" y="69">{field === "cpu" ? `${Math.round(maximum(field) / 2 * 1000)}m` : metricValue(maximum(field) / 2, field).replace(/\.0 MiB/, " Mi").replace(" MiB", " Mi").replace(" GiB", " Gi")}</text>
            <text x="0" y="112">0</text></>}
            {[0, .25, .5, .75, 1].map((fraction) => <text key={fraction} x={48 + fraction * 720} y="135" textAnchor={fraction === 0 ? "start" : fraction === 1 ? "end" : "middle"}>{fraction === 1 ? "now" : new Date(start + fraction * (now - start)).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", hour12: false })}</text>)}
          </g>
        </svg>
      </div>
      <details className="metric-inspector"><summary>Data<span className="sr-only"> and allocations</span></summary><div>
      {(["cpu", "memory"] as const).map((metric) => <details key={metric} className="usage-samples text-2xs text-ink-400">
        <summary className="cursor-pointer">
          Usage samples ({samples.length})
        </summary>
        <table className="mt-2 w-full">
          <thead>
            <tr>
              <th className="text-left">Time</th>
              <th className="text-right">{metric === "cpu" ? "CPU" : "Memory"}</th>
              {showCoverage && <th className="text-right">Pods sampled</th>}
            </tr>
          </thead>
          <tbody>
            {samples.map((sample) => (
              <tr key={sample.observedAt}>
                <td>{new Date(sample.observedAt).toLocaleString()}</td>
                <td className="text-right">
                  {metricValue(sample[metric], metric)}
                </td>
                {showCoverage && (
                  <td className="text-right">
                    {sample.coverage
                      ? `${sample.coverage[metric]}/${sample.coverage.total}`
                      : "Unavailable"}
                  </td>
                )}
              </tr>
            ))}
          </tbody>
        </table>
      </details>)}
      {containers?.length ? <details className="resource-container-resources"><summary>Container resources</summary>{containers.map((container) => <p key={container.name} className="break-all">{container.name} {detailText(container.image)} CPU {container.requests?.cpu ?? "no request"} / {container.limits?.cpu ?? "no limit"} Memory {container.requests?.memory ?? "no request"} / {container.limits?.memory ?? "no limit"}</p>)}</details> : null}
      </div></details>
    </figure>
  );
}
