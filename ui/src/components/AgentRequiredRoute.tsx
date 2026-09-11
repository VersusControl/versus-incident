import { useQuery } from "@tanstack/react-query";
import { AlertTriangle, RefreshCw } from "lucide-react";
import { api } from "@/lib/api";
import { TopBar } from "@/components/TopBar";
import { Spinner } from "@/components/feedback";

export function AgentRequiredRoute({ title, children }: { title: string; children: React.ReactNode }) {
  const config = useQuery({
    queryKey: ["agent-config"],
    queryFn: api.getAgentConfig,
    staleTime: 60_000,
    retry: false,
  });

  if (config.isPending) {
    return <>
      <TopBar title={title} />
      <main className="flex flex-1 items-center justify-center p-6 text-sm text-ink-300">
        <Spinner className="mr-2" /> Checking AI Agent status…
      </main>
    </>;
  }

  if (config.isSuccess && !config.data.enable) {
    return <>
      <TopBar title={title} />
      <main className="flex-1 overflow-auto p-6">
        <section className="mx-auto mt-8 max-w-2xl rounded-card border border-sev-warn/40 bg-sev-warn/10 p-5" role="alert">
          <div className="flex items-start gap-3">
            <AlertTriangle size={19} className="mt-0.5 shrink-0 text-sev-warn" aria-hidden />
            <div className="min-w-0">
              <h2 className="text-sm font-semibold text-ink-50">AI Agent is disabled</h2>
              <p className="mt-2 text-sm leading-6 text-ink-200">
                Enable the AI Agent with <code>AGENT_ENABLE=true</code>, restart Versus, then check again.
              </p>
              <button type="button" className="btn mt-4" onClick={() => config.refetch()} disabled={config.isFetching}>
                <RefreshCw size={13} className={config.isFetching ? "animate-spin" : undefined} aria-hidden />
                {config.isFetching ? "Checking…" : "Check again"}
              </button>
            </div>
          </div>
        </section>
      </main>
    </>;
  }

  return children;
}