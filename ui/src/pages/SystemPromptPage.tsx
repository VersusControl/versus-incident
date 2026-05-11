import { Link } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { ArrowLeft } from "lucide-react";
import { api } from "@/lib/api";
import { TopBar } from "@/components/TopBar";
import { ErrorBox, Spinner } from "@/components/feedback";

export function SystemPromptPage() {
  const prompt = useQuery({
    queryKey: ["system-prompt"],
    queryFn: api.getSystemPrompt,
    staleTime: 60_000,
    refetchInterval: false,
  });

  return (
    <>
      <TopBar
        title="System prompt"
        subtitle="Constant header sent to the model on every detect-mode AI call."
        actions={
          <Link to="/detect" className="btn">
            <ArrowLeft size={12} /> Back
          </Link>
        }
      />
      <main className="flex-1 overflow-auto p-6">
        {prompt.isLoading && <Spinner />}
        {prompt.isError && <ErrorBox error={prompt.error} />}
        {prompt.data && (
          <div className="card">
            <div className="card-body">
              <pre className="max-h-[calc(100vh-200px)] overflow-auto whitespace-pre-wrap break-words rounded-md bg-ink-900/95 p-4 font-mono text-2xs text-ink-100">
                {prompt.data}
              </pre>
            </div>
          </div>
        )}
      </main>
    </>
  );
}
