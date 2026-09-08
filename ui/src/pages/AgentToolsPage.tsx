import { useState, type ElementType } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "react-router-dom";
import { AlertCircle, ArrowUpRight, BookOpenText, Check, CircleOff, ExternalLink, FileText, GitBranch, Loader2, Network, RefreshCw, Settings, ShipWheel, Wrench } from "lucide-react";
import { FaAws } from "react-icons/fa6";
import { SiGrafana, SiGraylog, SiPrometheus, SiSplunk } from "react-icons/si";
import { api, type AgentToolsetAvailability, type AgentToolKind, type AgentToolState } from "@/lib/api";
import { Modal } from "@/components/Modal";
import { TopBar } from "@/components/TopBar";
import { RetryableError } from "@/components/RetryableError";
import { SkCard } from "@/components/Skeleton";

const SECTIONS = ["connector", "datasource", "common"] as const;
const SECTION_LABELS = { connector: "Connectors", datasource: "Data Source Tools", common: "Common" };
type CatalogToolState = AgentToolState | "configured" | "shared_unknown";
type CatalogToolset = Omit<AgentToolsetAvailability, "state"> & {
  state: CatalogToolState;
  shared_state?: AgentToolState;
  policy_id?: string;
  provider_type?: string;
  provider_configured?: boolean;
  configured_source_names?: string[];
  config_known?: boolean;
  shared_capability?: string;
};

function ElasticsearchIcon({ size = 24, className }: { size?: number; className?: string }) {
  return <img src="/elasticsearch.svg" alt="" width={size} height={size} className={className} aria-hidden="true" />;
}

const ICONS: Record<string, ElementType> = {
  kubernetes: ShipWheel,
  git: GitBranch,
  file: FileText,
  elasticsearch: ElasticsearchIcon,
  loki: SiGrafana,
  cloudwatch: FaAws,
  graylog: SiGraylog,
  splunk: SiSplunk,
  signoz: Network,
  prometheus: SiPrometheus,
  tempo: SiGrafana,
  runbook: BookOpenText,
  dependencies: Network,
  common: Wrench,
};
const ICON_COLORS: Record<string, string> = {
  kubernetes: "tool-brand-kubernetes",
  git: "tool-brand-git",
  file: "tool-brand-common",
  elasticsearch: "tool-brand-common",
  loki: "tool-brand-common",
  cloudwatch: "tool-brand-common",
  graylog: "tool-brand-common",
  splunk: "tool-brand-common",
  signoz: "tool-brand-common",
  prometheus: "tool-brand-common",
  tempo: "tool-brand-common",
  runbook: "tool-brand-runbook",
  dependencies: "tool-brand-dependencies",
  common: "tool-brand-common",
};

const PROVIDERS = [
  { base_id: "logs", type: "file", name: "File", description: "Read logs from local files.", icon_key: "file", docs_url: "https://docs.versusincident.com/#/agent/data-sources/file" },
  { base_id: "logs", type: "loki", name: "Loki", description: "Query logs stored in Grafana Loki.", icon_key: "loki", docs_url: "https://docs.versusincident.com/#/agent/data-sources/loki" },
  { base_id: "logs", type: "cloudwatchlogs", name: "CloudWatch Logs", description: "Query logs stored in Amazon CloudWatch.", icon_key: "cloudwatch", docs_url: "https://docs.versusincident.com/#/agent/data-sources/cloudwatch-logs" },
  { base_id: "logs", type: "graylog", name: "Graylog", description: "Search logs stored in Graylog.", icon_key: "graylog", docs_url: "https://docs.versusincident.com/#/agent/data-sources/graylog" },
  { base_id: "logs", type: "splunk", name: "Splunk", description: "Search logs stored in Splunk.", icon_key: "splunk", docs_url: "https://docs.versusincident.com/#/agent/data-sources/splunk" },
  { base_id: "logs", type: "signoz", name: "SigNoz Logs", description: "Query logs stored in SigNoz.", icon_key: "signoz", docs_url: "https://docs.versusincident.com/#/agent/data-sources/signoz" },
  { base_id: "metrics", type: "prometheus", name: "Prometheus", description: "Query metrics from Prometheus.", icon_key: "prometheus", docs_url: "https://docs.versusincident.com/#/agent/data-sources/prometheus" },
  { base_id: "metrics", type: "cloudwatch_metrics", name: "CloudWatch Metrics", description: "Query metrics from Amazon CloudWatch.", icon_key: "cloudwatch", docs_url: "https://docs.versusincident.com/#/agent/data-sources/cloudwatch-metrics" },
  { base_id: "metrics", type: "signoz_metrics", name: "SigNoz Metrics", description: "Query metrics from SigNoz.", icon_key: "signoz", docs_url: "https://docs.versusincident.com/#/enterprise/metrics/signoz" },
  { base_id: "traces", type: "traces", name: "Grafana Tempo", description: "Inspect distributed traces in Grafana Tempo.", icon_key: "tempo", docs_url: "https://docs.versusincident.com/#/agent/data-sources/traces" },
  { base_id: "traces", type: "signoz_traces", name: "SigNoz Traces", description: "Inspect distributed traces in SigNoz.", icon_key: "signoz", docs_url: "https://docs.versusincident.com/#/agent/data-sources/traces?id=signoz-backend" },
] as const;

function isCanonicalToolProvider(baseID: string, providerType: string): boolean {
  return (baseID === "metrics" && providerType === "prometheus") || (baseID === "traces" && providerType === "traces");
}

function buildCatalog(toolsets: AgentToolsetAvailability[], sources: Awaited<ReturnType<typeof api.getAgentConfig>>["sources"] | undefined, configKnown: boolean): CatalogToolset[] {
  const baseByID = new Map(toolsets.map((toolset) => [toolset.id, toolset]));
  const enabledSourceTypes = new Set(sources?.filter((source) => source.enable).map((source) => source.type) ?? []);
  const providers = PROVIDERS.flatMap((provider) => {
    const base = baseByID.get(provider.base_id);
    if (!base) return [];
    const configuredSourceNames = sources
      ?.filter((source) => source.enable && source.type === provider.type)
      .map((source) => source.name) ?? [];
    const configured = configKnown && configuredSourceNames.length > 0;
    const siblingConfigured = PROVIDERS.some((candidate) => candidate.base_id === base.id && enabledSourceTypes.has(candidate.type));
    const configuredOutsideSources = configKnown && !configured && !siblingConfigured && isCanonicalToolProvider(base.id, provider.type) && ["available", "disabled_by_operator"].includes(base.state);
    const providerAvailable = configured || configuredOutsideSources;
    const authoritative = base.state === "needs_license" || base.state === "needs_permission";
    const state: CatalogToolState = authoritative
      ? base.state
      : !configKnown
        ? "shared_unknown"
        : providerAvailable
          ? base.state === "unhealthy" ? "unhealthy" : "configured"
          : "needs_datasource";
    const reason = authoritative
      ? base.reason
      : !configKnown
        ? `Provider configuration is unavailable. This provider uses the shared ${base.display_name} agent capability.`
        : providerAvailable
          ? base.state === "unhealthy" ? `The shared ${base.display_name} agent capability is unhealthy.` : configuredOutsideSources ? `${provider.name} tools are configured outside agent sources.` : `${provider.name} is configured.`
          : `${provider.name} is not configured.`;
    const baseAllowsUI = base.ui_path && !["needs_license", "needs_permission"].includes(base.state);
    return [{
      ...base,
      id: `provider:${provider.type}`,
      policy_id: base.id,
      shared_state: base.state,
      provider_type: provider.type,
      provider_configured: providerAvailable,
      configured_source_names: configuredSourceNames,
      config_known: configKnown,
      shared_capability: base.display_name,
      display_name: provider.name,
      description: provider.description,
      icon_key: provider.icon_key,
      docs_url: provider.docs_url,
      ui_path: providerAvailable && baseAllowsUI ? base.ui_path : undefined,
      state,
      reason,
    }];
  });
  return [
    ...toolsets
      .filter((toolset) => toolset.section !== "datasource" || toolset.id === "elasticsearch-logs")
      .map((toolset) => toolset.id === "elasticsearch-logs" ? { ...toolset, display_name: "Elasticsearch" } : toolset),
    ...providers,
  ];
}

export function AgentToolsPage() {
  const [agent, setAgent] = useState<AgentToolKind>("chat");
  const [selectedID, setSelectedID] = useState<string | null>(null);
  const queryClient = useQueryClient();
  const toolsets = useQuery({
    queryKey: ["agent-toolsets", agent],
    queryFn: () => api.listAgentToolsets(agent),
    retry: false,
  });
  const config = useQuery({
    queryKey: ["agent-config"],
    queryFn: () => api.getAgentConfig(),
    retry: false,
  });
  const catalog = buildCatalog(toolsets.data ?? [], config.data?.sources, config.isSuccess);
  const toggle = useMutation({
    mutationFn: (input: { toolset: CatalogToolset; enabled: boolean }) =>
      api.setAgentToolsetEnabled(agent, input.toolset.policy_id ?? input.toolset.id, input.enabled),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ["agent-toolsets", agent] }),
  });
  const selected = catalog.find((toolset) => toolset.id === selectedID) ?? null;

  return (
    <main className="min-w-0 flex-1 overflow-auto">
      <TopBar title="Tools" />
      <div className="mx-auto max-w-6xl space-y-5 p-4 sm:p-6">
        <div className="flex flex-col justify-between gap-4 border-ink-700 pb-5 sm:flex-row sm:items-end">
          <div>
            <h1 className="text-xl font-semibold text-ink-50">Tool Catalog</h1>
            <p className="mt-1 max-w-2xl text-sm text-ink-300">
              Set up and activate tools
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
        {config.isError && <RetryableError error={config.error} onRetry={() => config.refetch()} retrying={config.isRefetching} context="Couldn't verify configured data sources" />}
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
          const rows = catalog.filter((toolset) => toolset.section === section);
          if (rows.length === 0) return null;
          return <section key={section} aria-labelledby={`tools-${section}`}>
            <h2 id={`tools-${section}`} className="mb-3 text-sm font-semibold text-ink-100">{SECTION_LABELS[section]}</h2>
            <div className="grid grid-cols-1 gap-3 md:grid-cols-2 xl:grid-cols-3">
              {rows.map((toolset) => <ToolsetCard key={toolset.id} toolset={toolset} onDetails={() => setSelectedID(toolset.id)} />)}
            </div>
          </section>;
        })}
      </div>
      {selected && <ToolsetDetails toolset={selected} agent={agent} pending={toggle.isPending && toggle.variables?.toolset.id === selected.id} onClose={() => setSelectedID(null)} onToggle={(enabled) => toggle.mutate({ toolset: selected, enabled })} />}
    </main>
  );
}

function ToolsetCard({ toolset, onDetails }: { toolset: CatalogToolset; onDetails: () => void }) {
  const Icon = ICONS[toolset.icon_key] ?? Wrench;
  const iconColor = ICON_COLORS[toolset.icon_key] ?? ICON_COLORS.common;
  return (
    <article className="flex min-h-22 items-center gap-4 rounded-card border border-transparent bg-transparent p-4 transition-colors hover:border-ink-500/60 hover:bg-surface hover:shadow-card">
      <div className="flex size-12 shrink-0 items-center justify-center rounded-xl border border-ink-600 bg-ink-800"><Icon size={24} className={`tool-brand-icon ${iconColor}`} strokeWidth={2.2} aria-hidden="true" /></div>
      <div className="min-w-0 flex-1">
        <h3 className="text-base font-medium text-ink-50">{toolset.display_name}</h3>
        <StateBadge state={toolset.state} />
      </div>
      <div className="flex shrink-0 items-center gap-1">
        {toolset.ui_path && !["needs_license", "needs_permission"].includes(toolset.state) && <Link to={toolset.ui_path} aria-label={`Open ${toolset.display_name}`} title={`Open ${toolset.display_name}`} className="inline-flex size-11 items-center justify-center rounded-full text-ink-200 hover:bg-ink-700 hover:text-ink-50"><ArrowUpRight size={20} /></Link>}
        <button type="button" onClick={onDetails} aria-label={`${toolset.display_name} settings`} title={`${toolset.display_name} settings`} className="inline-flex size-11 items-center justify-center rounded-full text-ink-300 hover:bg-ink-700 hover:text-ink-50"><Settings size={19} /></button>
      </div>
    </article>
  );
}

function StateBadge({ state }: { state: CatalogToolState }) {
  const labels: Record<CatalogToolState, string> = {
    available: "Ready",
    disabled_by_operator: "Off",
    needs_license: "Enterprise",
    needs_datasource: "Data source needed",
    needs_integration: "Connection needed",
    needs_capability: "Setup needed",
    needs_permission: "No access",
    unhealthy: "Unhealthy",
    shared_unknown: "Shared status unknown",
    configured: "Configured",
  };
  const ready = state === "available" || state === "configured";
  const off = state === "disabled_by_operator";
  return <span className={`mt-1 inline-flex items-center gap-1 text-2xs ${ready ? "text-sev-ok" : off ? "text-ink-400" : state === "unhealthy" ? "text-sev-critical" : ""}`}>{ready ? <Check size={11} /> : off ? <CircleOff size={11} /> : <AlertCircle size={11} />}{labels[state]}</span>;
}

function ToolsetDetails({ toolset, agent, pending, onClose, onToggle }: { toolset: CatalogToolset; agent: AgentToolKind; pending: boolean; onClose: () => void; onToggle: (enabled: boolean) => void }) {
  const baseSatisfied = toolset.provider_type ? toolset.config_known && toolset.state === "configured" && (toolset.shared_state === "available" || toolset.shared_state === "disabled_by_operator") : toolset.state === "available" || toolset.state === "disabled_by_operator";
  const providerConfigured = !toolset.provider_type || Boolean(toolset.provider_configured);
  const satisfied = baseSatisfied && providerConfigured;
  const hiddenInternalAction = toolset.action.startsWith("/") && (toolset.id === "source-control" || toolset.section === "datasource" || toolset.section === "common");
  const showAvailabilityAction = toolset.action && !hiddenInternalAction;
  const Icon = ICONS[toolset.icon_key] ?? Wrench;
  const iconColor = ICON_COLORS[toolset.icon_key] ?? ICON_COLORS.common;
  return <Modal title={toolset.display_name} onClose={onClose} size="lg" footer={<>{toolset.docs_url && <a className="btn" href={toolset.docs_url} target="_blank" rel="noopener noreferrer">Documentation <ExternalLink size={13} /></a>}{toolset.ui_path && !["needs_license", "needs_permission"].includes(toolset.state) && <Link className="btn btn-primary" to={toolset.ui_path} onClick={onClose}>Open tool <ArrowUpRight size={13} /></Link>}{showAvailabilityAction && (toolset.action.startsWith("/") ? <Link className="btn btn-primary" to={toolset.action}>{toolset.action_label}</Link> : <a className="btn btn-primary" href={toolset.action} target="_blank" rel="noopener noreferrer">{toolset.action_label} <ExternalLink size={13} /></a>)}</>}>
    <div className="flex items-start gap-3"><div className="flex size-12 shrink-0 items-center justify-center rounded-control border border-ink-600 bg-ink-800"><Icon size={24} className={`tool-brand-icon ${iconColor}`} strokeWidth={2.2} aria-hidden="true" /></div><div className="min-w-0"><p className="text-sm leading-6 text-ink-200">{toolset.description}</p><StateBadge state={toolset.state} /></div></div>
    <dl className="mt-5 divide-y divide-ink-700 border-y border-ink-700 text-xs"><div className="grid grid-cols-[7rem_minmax(0,1fr)] gap-3 py-3"><dt className="text-ink-400">Availability</dt><dd className="text-ink-100">{toolset.reason}</dd></div>{toolset.shared_capability && <div className="grid grid-cols-[7rem_minmax(0,1fr)] gap-3 py-3"><dt className="text-ink-400">Capability</dt><dd className="text-ink-100">Uses the shared {toolset.shared_capability} agent capability.</dd></div>}{toolset.configured_source_names && toolset.configured_source_names.length > 0 && <div className="grid grid-cols-[7rem_minmax(0,1fr)] gap-3 py-3"><dt className="text-ink-400">Configured sources</dt><dd className="text-ink-100">{toolset.configured_source_names.join(", ")}</dd></div>}{toolset.health && <div className="grid grid-cols-[7rem_minmax(0,1fr)] gap-3 py-3"><dt className="text-ink-400">Shared health</dt><dd className="text-ink-100">{toolset.health}</dd></div>}<div className="grid grid-cols-[7rem_minmax(0,1fr)] items-center gap-3 py-3"><dt className="text-ink-400">{toolset.shared_capability ? `${toolset.shared_capability} tools` : agent === "chat" ? "Chat agent" : "Analyze agent"}</dt><dd><label className="inline-flex items-center gap-2 text-ink-100"><input type="checkbox" aria-label={`Enable ${toolset.shared_capability ?? toolset.display_name} for ${agent}`} aria-describedby={!satisfied ? `${toolset.id}-setup-required` : undefined} className="h-4 w-4 accent-good" checked={toolset.enabled && satisfied} disabled={!satisfied || pending} onChange={(event) => onToggle(event.target.checked)} /><span>{toolset.enabled && satisfied ? "Enabled" : "Disabled"}</span>{pending && <Loader2 size={13} className="animate-spin" />}</label>{toolset.shared_capability && <p className="mt-1 text-2xs text-sev-warning">This setting affects every configured {toolset.shared_capability.toLowerCase()} provider.</p>}</dd></div></dl>
    {!satisfied && <div id={`${toolset.id}-setup-required`} className="mt-4 rounded-control border border-sev-warning/30 bg-sev-warning/10 p-3 text-xs leading-5 text-ink-200"><span className="font-medium">Setup required.</span> {toolset.reason}</div>}
  </Modal>;
}