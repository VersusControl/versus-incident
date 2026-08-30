import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "react-router-dom";
import { AlertCircle, ExternalLink, Loader2, RefreshCw, Wrench } from "lucide-react";
import { api, type AgentToolAvailability, type AgentToolKind } from "@/lib/api";
import { TopBar } from "@/components/TopBar";
import { RetryableError } from "@/components/RetryableError";
import { SkCard } from "@/components/Skeleton";

const GROUPS = ["versus", "common", "k8s"] as const;
const GROUP_LABELS = { versus: "Versus", common: "Common", k8s: "Kubernetes" };

function isVisibleTool(tool: AgentToolAvailability) {
  return !(tool.group === "versus" && tool.state === "available" && tool.enabled);
}

export function AgentToolsPage() {
  const [agent, setAgent] = useState<AgentToolKind>("chat");
  const queryClient = useQueryClient();
  const tools = useQuery({
    queryKey: ["agent-tools", agent],
    queryFn: () => api.listAgentTools(agent),
    retry: false,
  });
  const toggle = useMutation({
    mutationFn: (input: { tool: AgentToolAvailability; enabled: boolean }) =>
      api.setAgentToolEnabled(agent, input.tool.name, input.enabled),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ["agent-tools", agent] }),
  });
  // Common and Kubernetes metadata are complete, so filtering healthy default
  // Versus tools alone cannot produce the empty state.
  const visibleTools = tools.data?.filter(isVisibleTool);

  return (
    <main className="min-w-0 flex-1 overflow-auto">
      <TopBar title="Tools" />
      <div className="mx-auto max-w-6xl space-y-6 p-4 sm:p-6">
        <div className="flex flex-col justify-between gap-3 sm:flex-row sm:items-end">
          <div>
            <h1 className="text-xl font-semibold text-ink-50">Agent tools</h1>
            <p className="mt-1 max-w-2xl text-sm text-ink-300">
              Control which satisfied tools are offered to each agent.
            </p>
          </div>
          <div className="inline-flex w-fit rounded border border-ink-600 p-1" role="group" aria-label="Agent">
            {(["chat", "analyze"] as const).map((value) => (
              <button
                key={value}
                aria-pressed={agent === value}
                className={`px-3 py-1.5 text-xs font-medium capitalize ${agent === value ? "bg-ink-600 text-ink-50" : "text-ink-300"}`}
                onClick={() => setAgent(value)}
              >
                {value}
              </button>
            ))}
          </div>
        </div>

        {tools.isPending && <div className="space-y-4" aria-label="Loading tools"><SkCard lines={3} /><SkCard lines={3} /></div>}
        {tools.isError && <RetryableError error={tools.error} onRetry={() => tools.refetch()} retrying={tools.isRefetching} context="Couldn't load agent tools" />}
        {visibleTools?.length === 0 && (
          <div className="card p-8 text-center text-sm text-ink-300">
            <Wrench className="mx-auto mb-3" aria-hidden="true" />
            No tools are known to this build.
          </div>
        )}
        {toggle.isError && (
          <div role="alert" className="flex flex-wrap items-center justify-between gap-3 rounded border border-sev-critical/40 bg-sev-critical/10 p-3 text-sm text-sev-critical">
            <span className="flex items-center gap-2"><AlertCircle size={16} />{toggle.error.message}</span>
            <button className="btn" onClick={() => toggle.reset()}><RefreshCw size={14} /> Dismiss</button>
          </div>
        )}

        {GROUPS.map((group) => {
          const rows = visibleTools?.filter((tool) => tool.group === group) ?? [];
          if (rows.length === 0) return null;
          return (
            <section key={group} aria-labelledby={`tools-${group}`}>
              <div className="mb-3 flex items-baseline justify-between border-b border-ink-700 pb-2">
                <h2 id={`tools-${group}`} className="text-sm font-semibold text-ink-100">{GROUP_LABELS[group]}</h2>
                <span className="text-xs text-ink-400">{rows.length} tools</span>
              </div>
              <div className="grid grid-cols-1 gap-3 md:grid-cols-2 xl:grid-cols-3">
                {rows.map((tool) => <ToolCard key={tool.name} tool={tool} agent={agent} pending={toggle.isPending && toggle.variables?.tool.name === tool.name} onToggle={(enabled) => toggle.mutate({ tool, enabled })} />)}
              </div>
            </section>
          );
        })}
      </div>
    </main>
  );
}

function ToolCard({ tool, agent, pending, onToggle }: { tool: AgentToolAvailability; agent: AgentToolKind; pending: boolean; onToggle: (enabled: boolean) => void }) {
  const satisfied = tool.state === "available" || tool.state === "disabled_by_operator";
  const toggleDisabled = !satisfied || pending;
  const unavailableExplanation = !satisfied ? `${tool.display_name} is unavailable: ${tool.reason}` : undefined;
  return (
    <article className="card flex min-h-52 flex-col p-4">
      <div className="flex items-start justify-between gap-3">
        <div className="min-w-0">
          <h3 className="text-sm font-semibold text-ink-50">{tool.display_name}</h3>
          <code className="text-2xs text-ink-400">{tool.name}</code>
        </div>
        <label className="relative inline-flex shrink-0 items-center gap-2 text-xs text-ink-300">
          <span className="sr-only">Enable {tool.display_name} for {agent}</span>
          {pending && <Loader2 size={14} className="animate-spin" aria-hidden="true" />}
          <input
            type="checkbox"
            className="h-4 w-4 accent-good"
            checked={tool.enabled && satisfied}
            disabled={toggleDisabled}
            aria-disabled={toggleDisabled}
            aria-describedby={!satisfied ? `${tool.name}-unavailable` : undefined}
            onChange={(event) => onToggle(event.target.checked)}
          />
        </label>
      </div>
      <p className="mt-3 text-xs leading-5 text-ink-300">{tool.description}</p>
      <div className="mt-auto pt-4">
        <div className="text-2xs font-semibold uppercase text-ink-400">{tool.state.replaceAll("_", " ")}</div>
        <p id={!satisfied ? `${tool.name}-unavailable` : undefined} className="mt-1 text-xs leading-5 text-ink-200">{unavailableExplanation ?? tool.reason}</p>
        {tool.health && <p className="mt-1 text-2xs text-ink-400">Health: {tool.health}</p>}
        <div className="mt-3 flex flex-wrap gap-x-4 gap-y-2 text-xs">
          {tool.ui_path && tool.state !== "needs_license" && <Link className="inline-flex items-center gap-1 text-accent-300 hover:underline" to={tool.ui_path}>Open tool</Link>}
          {tool.docs_url && <a className="inline-flex items-center gap-1 text-accent-300 hover:underline" href={tool.docs_url} target="_blank" rel="noopener noreferrer">Documentation <ExternalLink size={12} aria-hidden="true" /></a>}
          {tool.action && <a className="inline-flex items-center gap-1 text-accent-300 hover:underline" href={tool.action} target={tool.action.startsWith("http") ? "_blank" : undefined} rel={tool.action.startsWith("http") ? "noopener noreferrer" : undefined}>{tool.action_label} <ExternalLink size={12} aria-hidden="true" /></a>}
        </div>
      </div>
    </article>
  );
}