import { Link } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { ArrowLeft } from "lucide-react";
import { api } from "@/lib/api";
import { SYSTEM_PROMPT_PARENT } from "@/lib/systemPromptNav";
import { TopBar } from "@/components/TopBar";
import { SkCard } from "@/components/Skeleton";
import { RetryableError } from "@/components/RetryableError";
import { EmptyState } from "@/components/feedback";

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
          <Link to={SYSTEM_PROMPT_PARENT} className="btn">
            <ArrowLeft size={12} aria-hidden /> Back
          </Link>
        }
      />
      <main className="flex-1 overflow-auto p-6">
        {prompt.isLoading && <SkCard lines={10} />}
        {prompt.isError && (
          <RetryableError
            error={prompt.error}
            onRetry={() => prompt.refetch()}
            retrying={prompt.isRefetching}
            context="Couldn't load the system prompt"
          />
        )}
        {prompt.isSuccess &&
          (prompt.data ? (
            <div className="card">
              <div className="card-body">
                <pre className="max-h-[calc(100vh-200px)] overflow-auto whitespace-pre-wrap break-words rounded-md border border-ink-600 bg-surface-sunken p-4 font-mono text-2xs text-ink-100">
                  {prompt.data}
                </pre>
              </div>
            </div>
          ) : (
            <div className="card">
              <EmptyState
                title="No system prompt available"
                hint="The agent's AI bundle may be disabled (ai.enable=false)."
              />
            </div>
          ))}
      </main>
    </>
  );
}
