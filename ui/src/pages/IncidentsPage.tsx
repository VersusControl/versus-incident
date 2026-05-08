import { useMemo, useState } from "react";
import { Link } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { Search } from "lucide-react";
import { api, type IncidentSummary } from "@/lib/api";
import { fmtAbs, fmtRel, truncate } from "@/lib/format";
import { TopBar } from "@/components/TopBar";
import { Pill } from "@/components/Pill";
import { EmptyState, ErrorBox, Spinner } from "@/components/feedback";

type StatusFilter = "all" | "open" | "acked" | "resolved";

const filters: { id: StatusFilter; label: string }[] = [
  { id: "all", label: "All" },
  { id: "open", label: "Open" },
  { id: "acked", label: "Acknowledged" },
  { id: "resolved", label: "Resolved" },
];

// IncidentsPage shows the persisted incident history pulled from the
// storage backend. Newest first, with a free-text filter and a status
// segmented control.
export function IncidentsPage() {
  const { data, isLoading, isError, error } = useQuery({
    queryKey: ["incidents"],
    queryFn: () => api.listIncidents(),
  });
  const [q, setQ] = useState("");
  const [filter, setFilter] = useState<StatusFilter>("all");

  const filtered = useMemo(() => {
    if (!data) return [];
    const needle = q.trim().toLowerCase();
    return data.filter((i) => {
      if (filter === "open" && (i.resolved || i.acked_at)) return false;
      if (filter === "acked" && !i.acked_at) return false;
      if (filter === "resolved" && !i.resolved) return false;
      if (!needle) return true;
      return (
        (i.title ?? "").toLowerCase().includes(needle) ||
        (i.service ?? "").toLowerCase().includes(needle) ||
        i.id.toLowerCase().includes(needle)
      );
    });
  }, [data, q, filter]);

  return (
    <>
      <TopBar
        title="Incidents"
        subtitle={data ? `${data.length} stored` : undefined}
      />

      <main className="flex-1 overflow-auto p-6">
        <div className="mb-3 flex flex-wrap items-center gap-2">
          <div className="relative max-w-md flex-1">
            <Search
              size={12}
              className="absolute left-2.5 top-1/2 -translate-y-1/2 text-ink-300"
            />
            <input
              className="input pl-7"
              placeholder="Search by id, title or service…"
              value={q}
              onChange={(e) => setQ(e.target.value)}
            />
          </div>
          <div className="flex overflow-hidden rounded-md border border-ink-200 bg-white">
            {filters.map((f) => (
              <button
                key={f.id}
                className={
                  "px-3 py-1.5 text-xs " +
                  (filter === f.id
                    ? "bg-accent text-white"
                    : "text-ink-700 hover:bg-ink-50")
                }
                onClick={() => setFilter(f.id)}
              >
                {f.label}
              </button>
            ))}
          </div>
        </div>

        {isError && <ErrorBox error={error} />}

        <div className="card overflow-hidden">
          <div className="max-h-[calc(100vh-220px)] overflow-auto">
            <table className="ddt">
              <thead>
                <tr>
                  <th className="w-44">When</th>
                  <th className="w-28">Service</th>
                  <th>Title</th>
                  <th className="w-32">Channels</th>
                  <th className="w-24">Notify</th>
                  <th className="w-24">Status</th>
                  <th className="w-32">ID</th>
                </tr>
              </thead>
              <tbody>
                {isLoading && (
                  <tr>
                    <td colSpan={7} className="py-8 text-center">
                      <Spinner />
                    </td>
                  </tr>
                )}
                {!isLoading && filtered.length === 0 && (
                  <tr>
                    <td colSpan={7}>
                      <EmptyState
                        title="No incidents"
                        hint={
                          q || filter !== "all"
                            ? "Try clearing filters."
                            : "Once an alert fires, it'll show up here."
                        }
                      />
                    </td>
                  </tr>
                )}
                {filtered.map((i) => (
                  <IncidentRow key={i.id} i={i} />
                ))}
              </tbody>
            </table>
          </div>
        </div>
      </main>
    </>
  );
}

function IncidentRow({ i }: { i: IncidentSummary }) {
  const status = i.resolved
    ? { label: "resolved", tone: "good" as const }
    : i.acked_at
      ? { label: "acked", tone: "accent" as const }
      : { label: "open", tone: "bad" as const };
  return (
    <tr>
      <td title={fmtAbs(i.created_at)}>{fmtRel(i.created_at)}</td>
      <td className="text-ink-700">{i.service || "—"}</td>
      <td>
        <Link
          to={`/incidents/${i.id}`}
          className="font-medium text-accent hover:underline"
        >
          {truncate(i.title || "(untitled)", 80)}
        </Link>
      </td>
      <td>
        <div className="flex flex-wrap gap-1">
          {(i.channels_notified ?? []).map((c) => (
            <Pill key={c}>{c}</Pill>
          ))}
          {!i.channels_notified?.length && (
            <span className="text-ink-300">—</span>
          )}
        </div>
      </td>
      <td>
        <NotifyPill status={i.notify_status} error={i.notify_error} />
      </td>
      <td>
        <Pill tone={status.tone}>{status.label}</Pill>
      </td>
      <td className="font-mono text-2xs text-ink-400">{i.id.slice(0, 8)}</td>
    </tr>
  );
}

function NotifyPill({ status, error }: { status?: string; error?: string }) {
  if (!status) return <span className="text-ink-300">—</span>;
  if (status === "sent") return <Pill tone="good">sent</Pill>;
  if (status === "failed")
    return (
      <span title={error}>
        <Pill tone="bad">failed</Pill>
      </span>
    );
  return <Pill tone="accent">{status}</Pill>;
}
