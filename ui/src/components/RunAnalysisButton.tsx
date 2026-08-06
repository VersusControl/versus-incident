import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Sparkles } from "lucide-react";
import clsx from "clsx";
import { api } from "@/lib/api";
import { Spinner } from "@/components/feedback";
import { useToast } from "@/components/toastContext";

// RunAnalysisButton fires the analyze mutation (wired to the analyze
// agent). Disabled with an explanation while AI is off (agent.ai.enable);
// outcomes always surface via toast — never silent. Shared by the incident
// detail page and the incidents list so both trigger analysis identically.
export function RunAnalysisButton({
  incidentID,
  onRan,
  className,
}: {
  incidentID: string;
  onRan?: () => void;
  className?: string;
}) {
  const cfg = useQuery({
    queryKey: ["agent-config"],
    queryFn: () => api.getAgentConfig(),
    staleTime: 60_000,
  });
  const qc = useQueryClient();
  const toast = useToast();
  const m = useMutation({
    mutationFn: () => api.runAnalysis(incidentID),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["analyses", incidentID] });
      toast.push({ tone: "ok", title: "Analysis complete" });
      onRan?.();
    },
    onError: (err) => {
      toast.push({
        tone: "error",
        title: "Analysis failed",
        description: err instanceof Error ? err.message : String(err),
        action: { label: "Retry", onClick: () => m.mutate() },
      });
    },
  });

  const aiOff = cfg.isSuccess && !cfg.data.ai?.enable;
  return (
    <span className="inline-flex items-center gap-2">
      <button
        className={clsx("btn", className)}
        disabled={m.isPending || cfg.isLoading || aiOff}
        onClick={() => m.mutate()}
        aria-label={
          aiOff
            ? "Run AI analysis — unavailable: AI is not enabled"
            : "Run AI analysis"
        }
        title={
          aiOff
            ? "AI is not enabled — configure it to run analyses."
            : "Run a fresh analysis. Past analyses stay available below."
        }
      >
        {m.isPending ? (
          <>
            <Spinner /> Analysing…
          </>
        ) : (
          <>
            <Sparkles size={11} /> Run analysis
          </>
        )}
      </button>
      {aiOff && (
        <span className="hidden text-2xs text-ink-400 sm:inline">
          AI not enabled
        </span>
      )}
    </span>
  );
}
