import { useRef } from "react";
import { useQuery } from "@tanstack/react-query";
import { AlertTriangle, RefreshCw } from "lucide-react";
import { api } from "@/lib/api";
import { TopBar } from "@/components/TopBar";
import { Spinner } from "@/components/feedback";
import { useAccess } from "@/lib/access";

export function AgentRequiredRoute({ title, children }: { title: string; children: React.ReactNode }) {
  // Every role is asked, finance included (it gets only `enable` and
  // `mode`). Waits for the session so the shell's role is known first.
  const access = useAccess();
  const config = useQuery({
    queryKey: ["agent-config"],
    queryFn: api.getAgentConfig,
    staleTime: 60_000,
    retry: false,
    // A refused probe is a final answer: a page mounting below must not ask again.
    retryOnMount: false,
    enabled: access.ready,
  });
  // An older server refuses finance the probe with a 403: a final answer.
  // Once the probe has answered (even with a refusal or an error) the page
  // stays mounted. Without this, each new reader of the same probe re-asks it,
  // the probe drops back to pending, the page unmounts and mounts again, and a
  // refused probe loops about once a second.
  const answered = useRef(false);
  if (config.isSuccess || config.isError) answered.current = true;

  if (!answered.current && (!access.ready || config.isPending)) {
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