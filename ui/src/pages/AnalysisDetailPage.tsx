import { useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ArrowLeft, Trash2 } from "lucide-react";
import { api } from "@/lib/api";
import { TopBar } from "@/components/TopBar";
import { SkCard } from "@/components/Skeleton";
import { RetryableError } from "@/components/RetryableError";
import { AnalysisCard } from "@/components/AnalysisCard";
import { ConfirmDialog } from "@/components/ConfirmDialog";
import { useToast } from "@/components/Toast";

// AnalysisDetailPage renders a single persisted analysis in full. The
// record (including its finding) goes straight into AnalysisCard, whose
// header now leads with the finding's Title + severity — the static
// title here is only the fallback for findingless records.
export function AnalysisDetailPage() {
  const { id = "", analysisId = "" } = useParams<{
    id: string;
    analysisId: string;
  }>();
  const navigate = useNavigate();
  const qc = useQueryClient();
  const toast = useToast();
  const [confirmDelete, setConfirmDelete] = useState(false);

  const { data, isLoading, isError, error, refetch, isFetching } = useQuery({
    queryKey: ["analysis", analysisId],
    queryFn: () => api.getAnalysis(analysisId),
    enabled: !!analysisId,
  });

  const del = useMutation({
    mutationFn: () => api.deleteAnalysis(analysisId),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["analyses"] });
      toast.push({ tone: "ok", title: "Analysis deleted" });
      navigate(`/analyses?incident=${id}`, { replace: true });
    },
    onError: (err) => {
      toast.push({
        tone: "error",
        title: "Couldn't delete the analysis",
        description: err instanceof Error ? err.message : String(err),
      });
    },
  });

  return (
    <>
      <TopBar
        title="Analysis"
        subtitle={analysisId.slice(0, 8)}
        actions={
          <div className="flex items-center gap-1.5">
            <Link to={`/analyses?incident=${id}`} className="btn">
              <ArrowLeft size={12} />
              All analyses
            </Link>
            <Link to={`/incidents/${id}`} className="btn">
              Incident
            </Link>
            <button
              className="btn btn-danger"
              aria-label="Delete this analysis"
              onClick={() => {
                del.reset();
                setConfirmDelete(true);
              }}
              disabled={del.isPending || !data}
            >
              <Trash2 size={12} aria-hidden />
              Delete
            </button>
          </div>
        }
      />

      <main className="flex-1 overflow-auto p-4 lg:p-6">
        {isLoading && <SkCard lines={6} />}
        {isError && (
          <RetryableError
            context="Couldn't load analysis"
            error={error}
            onRetry={() => refetch()}
            retrying={isFetching}
          />
        )}
        {data && <AnalysisCard rec={data} title="Analysis" />}
      </main>

      {confirmDelete && (
        <ConfirmDialog
          title="Delete analysis"
          message="This permanently removes this analysis record — the finding, hypotheses and tool-call audit trail are lost. The incident itself is unaffected."
          confirmLabel="Delete"
          tone="danger"
          busy={del.isPending}
          error={del.error instanceof Error ? del.error : null}
          onConfirm={() => del.mutate()}
          onClose={() => setConfirmDelete(false)}
        />
      )}
    </>
  );
}
