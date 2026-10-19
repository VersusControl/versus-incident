import { useState } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { Activity, FileText, Save, Waypoints } from "lucide-react";
import { Modal } from "@/components/Modal";
import { type KubernetesAgentSettings, type KubernetesAgentSettingsInput } from "@/lib/api";
import { useKubernetesCluster, useKubernetesQuery } from "@/lib/useKubernetesCluster";
import { KubernetesLinkedSource } from "./KubernetesLinkedSource";

export function KubernetesAgentTab({ clusterName }: { clusterName?: string }) {
  const { client, cluster } = useKubernetesCluster();
  const settings = useKubernetesQuery({ queryKey: ["kubernetes-agent-settings"], queryFn: () => client.kubernetesAgentSettings(), retry: false, refetchInterval: false });
  return <section aria-label="Kubernetes Agent" className="min-w-0 p-1 text-xs sm:p-4">
    <header className="mb-5"><div className="flex flex-wrap items-center gap-3"><h2 className="text-lg font-semibold text-ink-50">Agent</h2><span className="pill">cluster {cluster ?? clusterName ?? "Unavailable"}</span></div><p className="mt-2 text-ink-300">Learning preferences and log capture for this cluster.</p></header>
    {settings.isPending && <p role="status">Loading Agent settings...</p>}
    {settings.isError && <p role="alert" className="text-sev-warning">Agent settings unavailable.<button type="button" className="btn ml-2" onClick={() => void settings.refetch()}>Retry</button></p>}
    {!settings.isError && settings.data && <AgentSettings key={cluster ?? "default"} settings={settings.data} />}
  </section>;
}

function AgentSettings({ settings }: { settings: KubernetesAgentSettings }) {
  const { client, key } = useKubernetesCluster();
  const queryClient = useQueryClient();
  const [draft, setDraft] = useState<KubernetesAgentSettingsInput | null>(null);
  const [scopeOpen, setScopeOpen] = useState(false);
  const [scope, setScope] = useState({ include: "", exclude: "" });
  const [saved, setSaved] = useState(false);
  if (!settings.can_write && draft) setDraft(null);
  const value = draft ?? settings;
  const edit = (next: KubernetesAgentSettingsInput) => { setSaved(false); setDraft(next); };
  const save = useMutation({ mutationFn: () => client.kubernetesSaveAgentSettings(value), onSuccess: (result) => { queryClient.setQueryData(key("kubernetes-agent-settings"), result); setDraft(null); setSaved(true); } });
  const rows = [
    { field: "logs" as const, title: "Logs", icon: FileText, description: "Kubernetes container logs, grouped by Deployment and StatefulSet.", status: `${settings.detected.workloads} workloads in ${settings.detected.namespaces} namespaces` },
    { field: "metrics" as const, title: "Metrics", icon: Activity, description: "CPU and memory from metrics-server.", status: settings.detected.metrics_server ? "metrics-server found" : "metrics-server not found" },
    { field: "traces" as const, title: "Traces", icon: Waypoints, description: "Auto-detect a tracing backend running in this cluster.", status: settings.detected.traces ? `${settings.detected.traces.kind} found in ${settings.detected.traces.namespace}` : "No tracing backend found" },
  ];
  const parseScope = (input: string) => [...new Set(input.split(/[\s,]+/).map((item) => item.trim()).filter(Boolean))];
  const validScope = [scope.include, scope.exclude].every((input) => parseScope(input).every((name) => name.length <= 63 && /^[a-z0-9](?:[-a-z0-9]*[a-z0-9])?$/.test(name)));
  return <>
    {!settings.can_write && <p role="status" className="mb-4 text-ink-300">Read only. An administrator can change these settings.</p>}
    <section aria-label="Agent learning" className="pb-5"><header className="flex items-center gap-2"><h3 className="text-sm font-semibold text-ink-100">Agent learning</h3><span className="pill pill-accent">Preview</span></header><p className="mt-2 text-ink-300">Preferences are saved for this cluster. Learning is not active in this preview.</p>
      <div className="mt-3 divide-y divide-ink-600">{rows.map((row) => <div key={row.field} className="flex min-w-0 flex-wrap items-center gap-3 py-4"><row.icon size={18} className="shrink-0 text-link" aria-hidden="true" /><div className="min-w-0 flex-1"><h4 className="font-medium text-ink-100">{row.title}</h4><p className="mt-1 text-ink-300">{row.description}</p><div className="mt-2 flex flex-wrap items-center gap-3"><span className="pill max-w-full whitespace-normal">{row.status}</span>{row.field === "logs" && <><span className="break-all text-ink-300">{value.learning.scope.include?.length ? `Include: ${value.learning.scope.include.join(", ")}` : "All namespaces"}{value.learning.scope.exclude?.length ? `; exclude: ${value.learning.scope.exclude.join(", ")}` : ""}</span>{settings.can_write && <button type="button" className="text-link" disabled={save.isPending} onClick={() => { setScope({ include: (value.learning.scope.include ?? []).join(", "), exclude: (value.learning.scope.exclude ?? []).join(", ") }); setScopeOpen(true); }}>Edit scope</button>}</>}</div></div>{settings.can_write ? <input type="checkbox" role="switch" aria-label={`${row.title} learning`} className="size-5 shrink-0 accent-accent" checked={value.learning[row.field]} disabled={save.isPending} onChange={(event) => edit({ ...value, learning: { ...value.learning, [row.field]: event.target.checked } })} /> : <span className="pill">{value.learning[row.field] ? "On" : "Off"}</span>}</div>)}</div>
    </section>
    <KubernetesLinkedSource settings={settings} fields={value.break_logs.fields} selected={value.break_logs.linked_source} savedSource={settings.break_logs.linked_source} onSelect={(name) => edit({ ...value, break_logs: { ...value.break_logs, linked_source: name } })} onFields={(type, fields) => edit({ ...value, break_logs: { ...value.break_logs, fields: { ...value.break_logs.fields, [type]: fields } } })} onSave={() => save.mutate()} saving={save.isPending} />
    {save.isError && <p role="alert" className="my-3 text-sev-critical">Settings could not be saved. Check your permission and mapping, then try again.</p>}
    {saved && <p role="status" className="my-3 text-sev-ok">Agent settings saved.</p>}
    {settings.can_write && <div className="flex justify-end gap-2 border-t border-ink-600 pt-4"><button type="button" className="btn" disabled={!draft || save.isPending} onClick={() => { setDraft(null); setSaved(false); }}>Discard changes</button><button type="button" className="btn btn-primary" disabled={!draft || save.isPending} onClick={() => save.mutate()}><Save size={14} />{save.isPending ? "Saving..." : "Save settings"}</button></div>}
    {scopeOpen && <Modal title="Log learning scope" onClose={() => setScopeOpen(false)} footer={<><button type="button" className="btn" onClick={() => setScopeOpen(false)}>Cancel</button><button type="button" className="btn btn-primary" disabled={!validScope} onClick={() => { edit({ ...value, learning: { ...value.learning, scope: { include: parseScope(scope.include), exclude: parseScope(scope.exclude) } } }); setScopeOpen(false); }}>Apply scope</button></>}><div className="space-y-4 text-xs"><label className="block text-ink-200">Include namespaces<input className="input mt-1 w-full" value={scope.include} onChange={(event) => setScope({ ...scope, include: event.target.value })} placeholder="All namespaces" /></label><label className="block text-ink-200">Exclude namespaces<input className="input mt-1 w-full" value={scope.exclude} onChange={(event) => setScope({ ...scope, exclude: event.target.value })} /></label>{!validScope && <p role="alert" className="text-sev-warning">Enter valid namespace names separated by commas.</p>}</div></Modal>}
  </>;
}