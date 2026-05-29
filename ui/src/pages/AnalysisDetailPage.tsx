import { Link, useParams } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { ArrowLeft } from "lucide-react";
import { api } from "@/lib/api";
import { TopBar } from "@/components/TopBar";
import { ErrorBox, Spinner } from "@/components/feedback";
import { AnalysisCard } from "@/components/AnalysisCard";

// AnalysisDetailPage renders a single persisted analysis in full.
export function AnalysisDetailPage() {
  const { id = "", analysisId = "" } = useParams<{
    id: string;
    analysisId: string;
  }>();
  const { data, isLoading, isError, error } = useQuery({
    queryKey: ["analysis", analysisId],
    queryFn: () => api.getAnalysis(analysisId),
    enabled: !!analysisId,
  });

  return (
    <>
      <TopBar
        title="Analysis"
        subtitle={analysisId.slice(0, 8)}
        actions={
          <div className="flex items-center gap-1.5">
            <Link to={`/incidents/${id}/analyses`} className="btn">
              <ArrowLeft size={12} />
              All analyses
            </Link>
            <Link to={`/incidents/${id}`} className="btn">
              Incident
            </Link>
          </div>
        }
      />

      <main className="flex-1 overflow-auto p-6">
        {isLoading && <Spinner />}
        {isError && <ErrorBox error={error} />}
        {data && <AnalysisCard rec={data} title="Analysis" />}
      </main>
    </>
  );
}
