import { Link, useParams } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { ArrowLeft, Brain, ChevronRight } from "lucide-react";
import { api } from "@/lib/api";
import { fmtAbs, fmtRel } from "@/lib/format";
import { TopBar } from "@/components/TopBar";
import { Pill } from "@/components/Pill";
import { ErrorBox, Spinner } from "@/components/feedback";
import { formatDuration } from "@/components/AnalysisCard";

// AnalysesPage lists every analysis recorded for one incident, newest
// first. Each row links to the dedicated analysis detail page.
export function AnalysesPage() {
  const { id = "" } = useParams<{ id: string }>();
  const { data, isLoading, isError, error } = useQuery({
    queryKey: ["analyses", id],
    queryFn: () => api.listAnalyses(id),
    enabled: !!id,
  });

  const list = data ?? [];

  return (
    <>
      <TopBar
        title="Analyses"
        subtitle={id.slice(0, 8)}
        actions={
          <Link to={`/incidents/${id}`} className="btn">
            <ArrowLeft size={12} />
            Back to incident
          </Link>
        }
      />

      <main className="flex-1 overflow-auto p-6">
        {isLoading && <Spinner />}
        {isError && <ErrorBox error={error} />}

        {data && list.length === 0 && (
          <p className="text-sm text-ink-400">
            No analyses have been run for this incident yet.
          </p>
        )}

        {list.length > 0 && (
          <div className="space-y-2">
            {list.map((rec) => (
              <Link
                key={rec.id}
                to={`/incidents/${id}/analyses/${rec.id}`}
                className="flex items-center justify-between gap-3 rounded-md border border-ink-100 bg-white p-3 text-xs hover:border-accent/40"
              >
                <span className="flex min-w-0 items-center gap-2">
                  <Brain size={13} className="flex-none text-accent" />
                  <span className="min-w-0">
                    <span
                      className="block truncate text-ink-900"
                      title={rec.finding?.Title || rec.finding?.Summary}
                    >
                      {rec.finding?.Title ||
                        rec.finding?.Summary ||
                        "Analysis"}
                    </span>
                    <span className="flex items-center gap-2 text-2xs text-ink-500">
                      <span title={fmtAbs(rec.requested_at)}>
                        {fmtRel(rec.requested_at)}
                      </span>
                      {rec.requested_by && <span>by {rec.requested_by}</span>}
                      <span className="font-mono">{rec.id.slice(0, 8)}</span>
                    </span>
                  </span>
                </span>
                <span className="flex flex-none items-center gap-1.5">
                  {rec.model && (
                    <span className="text-2xs text-ink-500">{rec.model}</span>
                  )}
                  {rec.duration_ms !== undefined && (
                    <Pill>{formatDuration(rec.duration_ms)}</Pill>
                  )}
                  <Pill tone={rec.status === "ok" ? "good" : "bad"}>
                    {rec.status}
                  </Pill>
                  <ChevronRight size={13} className="text-ink-400" />
                </span>
              </Link>
            ))}
          </div>
        )}
      </main>
    </>
  );
}
