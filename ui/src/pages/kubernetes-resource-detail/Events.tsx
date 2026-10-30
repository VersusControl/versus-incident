import { useState } from "react";
import { Search } from "lucide-react";
import type { KubernetesResource } from "@/lib/api";
import { detailText } from "@/lib/kubernetesResourceDetail";
import { isKnownKubernetesTime } from "@/lib/kubernetesTime";
import { clock } from "@/lib/kubernetesResourcePresentation";

function eventTime(event: KubernetesResource, first = false): string {
  const summary = event.summary ?? {};
  const time = detailText(
    first
      ? (summary.first_timestamp ?? summary.firstTimestamp)
      : (summary.last_timestamp ?? summary.lastTimestamp),
    "",
  );
  return isKnownKubernetesTime(time) ? time : "";
}
function eventBoundary(events: KubernetesResource[], first = false) {
  const times = events.map((event) => eventTime(event, first)).filter(Boolean).sort((a, b) => Date.parse(a) - Date.parse(b));
  return (first ? times[0] : times.at(-1)) ?? "";
}
function eventOccurrences(event: KubernetesResource): number {
  const count = Number(event.summary?.count ?? 1);
  return Number.isSafeInteger(count) && count > 0 ? count : 1;
}
export function ResourceEvents({ events }: { events: KubernetesResource[] }) {
  const [kind, setKind] = useState("All");
  const [search, setSearch] = useState("");
  const now = Date.now();
  const warning = (event: KubernetesResource) =>
    event.summary?.type === "Warning";
  const counts = {
    All: events.reduce((sum, event) => sum + eventOccurrences(event), 0),
    Warnings: events
      .filter(warning)
      .reduce((sum, event) => sum + eventOccurrences(event), 0),
    Normal: events
      .filter((event) => !warning(event))
      .reduce((sum, event) => sum + eventOccurrences(event), 0),
  };
  const visible = events.filter(
    (event) =>
      (kind === "All" || (kind === "Warnings") === warning(event)) &&
      `${detailText(event.summary?.reason, event.name)} ${detailText(event.summary?.message, "")}`
        .toLowerCase()
        .includes(search.toLowerCase()),
  );
  const groups = new Map<string, KubernetesResource[]>();
  visible.forEach((event) => {
    const key = `${warning(event) ? "Warning" : "Normal"}:${detailText(event.summary?.reason, event.name)}`;
    groups.set(key, [...(groups.get(key) ?? []), event]);
  });
  const bins = Array.from({ length: 12 }, (_, index) => {
    const members = visible.filter((event) => {
      const at = Date.parse(eventTime(event));
      return (
        at >= now - (12 - index) * 300000 && at < now - (11 - index) * 300000
      );
    });
    return {
      normal: members
        .filter((event) => !warning(event))
        .reduce((sum, event) => sum + eventOccurrences(event), 0),
      warnings: members
        .filter(warning)
        .reduce((sum, event) => sum + eventOccurrences(event), 0),
    };
  });
  const maximum = Math.max(1, ...bins.map((bin) => bin.normal + bin.warnings));
  return (
    <section aria-label="Object-scoped events" className="resource-events space-y-3">
      <p className="sr-only">
        Counts are occurrences; histogram buckets use each Event’s last observed
        time.
      </p>
      <div className="flex flex-wrap items-center gap-2">
        {Object.entries(counts).map(([label, count]) => (
          <button
            type="button"
            key={label}
            className={`btn ${label === "Warnings" ? "event-warning-filter" : ""} ${label === kind ? "bg-accent/10 text-link" : ""}`}
            aria-pressed={label === kind}
            onClick={() => setKind(label)}
          >
            {label} <span className="text-ink-400">{count}</span>
          </button>
        ))}
        <label className="flex min-w-0 flex-1 items-center gap-2">
          <Search size={14} aria-hidden="true" />
          <input
            aria-label="Search events"
            placeholder="Search events"
            className="input w-full"
            value={search}
            onChange={(event) => setSearch(event.target.value)}
          />
        </label>
      </div>
      <figure className="event-histogram rounded-control border border-ink-700 bg-surface-sunken p-3">
        <figcaption className="mb-2 flex items-center justify-between text-xs text-ink-300">
          <span>Events per 5 minutes, last hour</span><span className="flex gap-3 text-2xs"><span><span className="text-sev-critical">■</span> Warnings</span><span><span className="text-ink-500">■</span> Normal</span></span>
        </figcaption>
        <div
          className="flex h-20 items-end gap-2"
          role="img"
          aria-label="Event histogram: warnings red, normal gray"
        >
          {bins.map((bin, index) => (
            <div
              key={index}
              className="flex h-full flex-1 flex-col justify-end"
              title={`${bin.warnings} warnings, ${bin.normal} normal`}
            >
              <div
                className="bg-sev-critical"
                style={{ height: `${(bin.warnings / maximum) * 100}%` }}
              />
              <div
                className="bg-ink-500"
                style={{ height: `${(bin.normal / maximum) * 100}%` }}
              />
            </div>
          ))}
        </div>
        <div className="mt-2 flex justify-between text-2xs text-ink-400">
          {[60, 45, 30, 15, 0].map((minutes) => <span key={minutes}>{minutes === 0 ? "now" : clock(new Date(now - minutes * 60000).toISOString()).slice(0, 5)}</span>)}
        </div>
      </figure>
      {!visible.length && (
        <p role="status" className="py-8 text-center text-xs text-ink-400">
          {events.length
            ? "No events match this filter."
            : "No object-scoped events were returned."}
        </p>
      )}
      {[...groups].map(([key, items]) => (
        <article
          key={key}
          className={`rounded-control border border-ink-700 border-l-[3px] bg-surface-sunken p-3 ${warning(items[0]) ? "border-l-sev-warning" : "border-l-ink-500"}`}
        >
          <div className="flex items-center gap-2">
            <h3 className="text-xs font-semibold">
              {detailText(items[0].summary?.reason, items[0].name)}
            </h3>
            <span className="pill">
              ×{items.reduce((sum, item) => sum + eventOccurrences(item), 0)}
            </span>
            <span className="ml-auto flex flex-wrap justify-end gap-x-4 text-2xs text-ink-400">
              <span>first <time dateTime={eventBoundary(items, true) || undefined} title={eventBoundary(items, true)}>{clock(eventBoundary(items, true))}</time></span>
              <span>last <time dateTime={eventBoundary(items) || undefined} title={eventBoundary(items)}>{clock(eventBoundary(items))}</time></span>
            </span>
          </div>
          {items.map((event) => (
            <div className="mt-2 space-y-1" key={event.uid ?? event.name}>
              <p
                className="truncate text-xs text-ink-200"
                title={detailText(event.summary?.message)}
              >
                {detailText(
                  event.summary?.message,
                  "Event message unavailable",
                )}
              </p>
              <div className="flex flex-wrap gap-x-4 gap-y-1 text-2xs text-ink-400">
                <span>
                  {detailText(event.summary?.source_component, "unknown")}{" "}
                  {detailText(event.summary?.source_host, "")}
                </span>
              </div>
            </div>
          ))}
        </article>
      ))}
    </section>
  );
}

