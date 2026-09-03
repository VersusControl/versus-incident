import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "react-router-dom";
import { AlertCircle, BarChart3, BookOpenText, ExternalLink, GitBranch, Loader2, Network, RefreshCw, Route, ScrollText, ShipWheel, Wrench, type LucideIcon } from "lucide-react";
import { api, type AgentToolsetAvailability, type AgentToolKind } from "@/lib/api";
import { TopBar } from "@/components/TopBar";
import { RetryableError } from "@/components/RetryableError";
import { SkCard } from "@/components/Skeleton";

const SECTIONS = ["connector", "datasource", "common"] as const;
const SECTION_LABELS = { connector: "Connectors", datasource: "Data Source Tools", common: "Common" };
const ICONS: Record<string, LucideIcon> = {
  kubernetes: ShipWheel,
  git: GitBranch,
  logs: ScrollText,
  metrics: BarChart3,
  traces: Route,
  runbook: BookOpenText,
  dependencies: Network,
  common: Wrench,
};

export function AgentToolsPage() {
  const [agent, setAgent] = useState<AgentToolKind>("chat");
  const queryClient = useQueryClient();
  const toolsets = useQuery({
    queryKey: ["agent-toolsets", agent],
    queryFn: () => api.listAgentToolsets(agent),
    retry: false,
  });
  const toggle = useMutation({
    mutationFn: (input: { toolset: AgentToolsetAvailability; enabled: boolean }) =>
      api.setAgentToolsetEnabled(agent, input.toolset.id, input.enabled),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ["agent-toolsets", agent] }),
  });

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

        {toolsets.isPending && <div className="space-y-4" aria-label="Loading tools"><SkCard lines={3} /><SkCard lines={3} /></div>}
        {toolsets.isError && <RetryableError error={toolsets.error} onRetry={() => toolsets.refetch()} retrying={toolsets.isRefetching} context="Couldn't load agent tools" />}
        {toolsets.data?.length === 0 && (
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

        {SECTIONS.map((section) => {
          const rows = toolsets.data?.filter((toolset) => toolset.section === section) ?? [];
          if (rows.length === 0) return null;
          return (
            <section key={section} aria-labelledby={`tools-${section}`}>
              <div className="mb-3 flex items-baseline justify-between border-b border-ink-700 pb-2">
                <h2 id={`tools-${section}`} className="text-sm font-semibold text-ink-100">{SECTION_LABELS[section]}</h2>
                <span className="text-xs text-ink-400">{rows.length} {rows.length === 1 ? "toolset" : "toolsets"}</span>
              </div>
              <div className="grid grid-cols-1 gap-3 md:grid-cols-2 xl:grid-cols-3">
                {rows.map((toolset) => <ToolsetCard key={toolset.id} toolset={toolset} agent={agent} pending={toggle.isPending && toggle.variables?.toolset.id === toolset.id} onToggle={(enabled) => toggle.mutate({ toolset, enabled })} />)}
              </div>
            </section>
          );
        })}
      </div>
    </main>
  );
}

function ToolsetCard({ toolset, agent, pending, onToggle }: { toolset: AgentToolsetAvailability; agent: AgentToolKind; pending: boolean; onToggle: (enabled: boolean) => void }) {
  const satisfied = toolset.state === "available" || toolset.state === "disabled_by_operator";
  const toggleDisabled = !satisfied || pending;
  const unavailableExplanation = !satisfied ? `${toolset.display_name} is unavailable: ${toolset.reason}` : undefined;
  const showAvailabilityAction = toolset.action && !(toolset.section === "common" && toolset.action.startsWith("/"));
  const Icon = ICONS[toolset.icon_key] ?? Wrench;
  return (
    <article className="card flex min-h-52 flex-col p-4">
      <div className="flex items-start justify-between gap-3">
        <div className="flex min-w-0 gap-3">
          <Icon size={18} className="mt-0.5 shrink-0 text-accent-300" aria-hidden="true" />
          <div>
            <h3 className="text-sm font-semibold text-ink-50">{toolset.display_name}</h3>
            <span className="text-2xs text-ink-400">{toolset.child_count} {toolset.child_count === 1 ? "tool" : "tools"}</span>
          </div>
        </div>
        <label className="relative inline-flex shrink-0 items-center gap-2 text-xs text-ink-300">
          <span className="sr-only">Enable {toolset.display_name} for {agent}</span>
          {pending && <Loader2 size={14} className="animate-spin" aria-hidden="true" />}
          <input
            type="checkbox"
            className="h-4 w-4 accent-good"
            checked={toolset.enabled && satisfied}
            disabled={toggleDisabled}
            aria-disabled={toggleDisabled}
            aria-describedby={!satisfied ? `${toolset.id}-unavailable` : undefined}
            onChange={(event) => onToggle(event.target.checked)}
          />
        </label>
      </div>
      <p className="mt-3 text-xs leading-5 text-ink-300">{toolset.description}</p>
      <div className="mt-auto pt-4">
        <div className="text-2xs font-semibold uppercase text-ink-400">{toolset.state.replaceAll("_", " ")}</div>
        <p id={!satisfied ? `${toolset.id}-unavailable` : undefined} className="mt-1 text-xs leading-5 text-ink-200">{unavailableExplanation ?? toolset.reason}</p>
        {toolset.health && <p className="mt-1 text-2xs text-ink-400">Health: {toolset.health}</p>}
        <div className="mt-3 flex flex-wrap gap-x-4 gap-y-2 text-xs">
          {toolset.ui_path && !["needs_license", "needs_permission"].includes(toolset.state) && <Link className="inline-flex items-center gap-1 text-accent-300 hover:underline" to={toolset.ui_path}>Open tool</Link>}
          {toolset.docs_url && <a className="inline-flex items-center gap-1 text-accent-300 hover:underline" href={toolset.docs_url} target="_blank" rel="noopener noreferrer">Documentation <ExternalLink size={12} aria-hidden="true" /></a>}
          {showAvailabilityAction && <a className="inline-flex items-center gap-1 text-accent-300 hover:underline" href={toolset.action} target={toolset.action.startsWith("http") ? "_blank" : undefined} rel={toolset.action.startsWith("http") ? "noopener noreferrer" : undefined}>{toolset.action_label} <ExternalLink size={12} aria-hidden="true" /></a>}
        </div>
      </div>
    </article>
  );
}