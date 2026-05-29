import { useMemo, useState } from "react";
import { Link } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { Search } from "lucide-react";
import { api } from "@/lib/api";
import { fmtAbs, fmtRel } from "@/lib/format";
import { TopBar } from "@/components/TopBar";
import { Pill } from "@/components/Pill";
import { ErrorBox, Spinner, EmptyState } from "@/components/feedback";
import { formatDuration } from "@/components/AnalysisCard";

type StatusFilter = "all" | "ok" | "error";

const filters: { id: StatusFilter; label: string }[] = [
  { id: "all", label: "All" },
  { id: "ok", label: "OK" },
  { id: "error", label: "Error" },
];

// AnalysesListPage lists every analysis recorded across all incidents,
// newest first, in a table. The Incident column shows the parent
// incident title and links directly to the incident detail page.
export function AnalysesListPage() {
  const { data, isLoading, isError, error } = useQuery({
    queryKey: ["analyses-all"],
    queryFn: () => api.listAllAnalyses(),
  });

  const { data: incidents } = useQuery({
    queryKey: ["incidents"],
    queryFn: () => api.listIncidents(),
  });

  const titleByID = useMemo(
    () => new Map((incidents ?? []).map((inc) => [inc.id, inc.title])),
    [incidents],
  );

  const [q, setQ] = useState("");
  const [filter, setFilter] = useState<StatusFilter>("all");

  const filtered = useMemo(() => {
    if (!data) return [];
    const needle = q.trim().toLowerCase();
    return data.filter((rec) => {
      if (filter !== "all" && rec.status !== filter) return false;
      if (!needle) return true;
      const title = titleByID.get(rec.incident_id) ?? "";
      return (
        title.toLowerCase().includes(needle) ||
        rec.incident_id.toLowerCase().includes(needle) ||
        (rec.finding?.Title ?? "").toLowerCase().includes(needle) ||
        (rec.finding?.Summary ?? "").toLowerCase().includes(needle) ||
        (rec.model ?? "").toLowerCase().includes(needle)
      );
    });
  }, [data, q, filter, titleByID]);

  return (
    <>
      <TopBar
        title="Analyses"
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
              placeholder="Search by incident, finding or model…"
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
          <div className="max-h-[calc(100vh-180px)] overflow-auto">
            <table className="ddt">
              <thead>
                <tr>
                  <th className="w-32">When</th>
                  <th>Incident</th>
                  <th>Finding</th>
                  <th className="w-32">Model</th>
                  <th className="w-20">Duration</th>
                  <th className="w-20">Status</th>
                </tr>
              </thead>
              <tbody>
                {isLoading && (
                  <tr>
                    <td colSpan={6} className="py-8 text-center">
                      <Spinner />
                    </td>
                  </tr>
                )}
                {!isLoading && filtered.length === 0 && (
                  <tr>
                    <td colSpan={6}>
                      <EmptyState
                        title="No analyses"
                        hint={
                          q || filter !== "all"
                            ? "Try clearing filters."
                            : "Run an analysis from an incident to see it here."
                        }
                      />
                    </td>
                  </tr>
                )}
                {filtered.map((rec) => (
                  <tr key={rec.id}>
                    <td title={fmtAbs(rec.requested_at)}>
                      {fmtRel(rec.requested_at)}
                    </td>
                    <td>
                      <Link
                        to={`/incidents/${rec.incident_id}`}
                        className="font-medium text-accent hover:underline"
                        title={rec.incident_id}
                      >
                        {titleByID.get(rec.incident_id) ||
                          `incident ${rec.incident_id.slice(0, 8)}`}
                      </Link>
                    </td>
                    <td
                      className="text-ink-700"
                      title={rec.finding?.Title || rec.finding?.Summary}
                    >
                      {rec.finding?.Title || rec.finding?.Summary || "—"}
                    </td>
                    <td className="text-2xs text-ink-500">{rec.model || "—"}</td>
                    <td className="text-2xs text-ink-500">
                      {rec.duration_ms !== undefined
                        ? formatDuration(rec.duration_ms)
                        : "—"}
                    </td>
                    <td>
                      <Pill tone={rec.status === "ok" ? "good" : "bad"}>
                        {rec.status}
                      </Pill>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </div>
      </main>
    </>
  );
}
