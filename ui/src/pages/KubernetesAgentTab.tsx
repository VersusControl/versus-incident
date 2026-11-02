import { useState } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { Activity, FileText, Save, Waypoints } from "lucide-react";
import { type KubernetesAgentSettings, type KubernetesAgentSettingsInput } from "@/lib/api";
import { useKubernetesCluster, useKubernetesQuery } from "@/lib/useKubernetesCluster";
import { KubernetesLinkedSource } from "./KubernetesLinkedSource";

export function KubernetesAgentTab({ clusterName }: { clusterName?: string }) {
  const { client, cluster } = useKubernetesCluster();
  const settings = useKubernetesQuery({ queryKey: ["kubernetes-agent-settings"], queryFn: () => client.kubernetesAgentSettings(), retry: false, refetchInterval: false });
  return <section aria-label="Kubernetes Agent" className="min-w-0 p-1 text-xs sm:p-4">
    <header className="mb-5"><div className="flex flex-wrap items-center gap-3"><h2 className="text-lg font-semibold text-ink-50">Agent</h2><span className="pill">cluster {cluster ?? clusterName ?? "Unavailable"}</span></div><p className="mt-2 text-ink-300">Log capture and linked sources for this cluster.</p></header>
    {settings.isPending && <p role="status">Loading Agent settings...</p>}
    {settings.isError && <p role="alert" className="text-sev-warning">Agent settings unavailable.<button type="button" className="btn ml-2" onClick={() => void settings.refetch()}>Retry</button></p>}
    {!settings.isError && settings.data && <AgentSettings key={cluster ?? "default"} settings={settings.data} />}
  </section>;
}

function AgentSettings({ settings }: { settings: KubernetesAgentSettings }) {
  const { client, key } = useKubernetesCluster();
  const queryClient = useQueryClient();
  const [draft, setDraft] = useState<KubernetesAgentSettingsInput | null>(null);
  const [saved, setSaved] = useState(false);
  const [delayInvalid, setDelayInvalid] = useState(false);
  const [delayDraft, setDelayDraft] = useState<string>();
  if (!settings.can_write && draft) setDraft(null);
  const value = draft ?? settings;
  const edit = (next: KubernetesAgentSettingsInput) => { setSaved(false); setDraft(next); };
  const save = useMutation({ mutationFn: () => client.kubernetesSaveAgentSettings({ learning: settings.learning, break_logs: value.break_logs }), onSuccess: (result) => { queryClient.setQueryData(key("kubernetes-agent-settings"), result); setDraft(null); setDelayDraft(undefined); setDelayInvalid(false); setSaved(true); } });
  const rows = [
    { field: "logs" as const, title: "Logs", icon: FileText, description: "Kubernetes container logs, grouped by Deployment and StatefulSet.", status: `${settings.detected.workloads} workloads in ${settings.detected.namespaces} namespaces` },
    { field: "metrics" as const, title: "Metrics", icon: Activity, description: "CPU and memory from metrics-server.", status: settings.detected.metrics_server ? "metrics-server found" : "metrics-server not found" },
    { field: "traces" as const, title: "Traces", icon: Waypoints, description: "Auto-detect a tracing backend running in this cluster.", status: settings.detected.traces ? `${settings.detected.traces.kind} found in ${settings.detected.traces.namespace}` : "No tracing backend found" },
  ];
  return <>
    {!settings.can_write && <p role="status" className="mb-4 text-ink-300">Read only. An administrator can change these settings.</p>}
    <KubernetesLinkedSource settings={settings} fields={value.break_logs.fields} selected={value.break_logs.linked_source} savedSource={settings.break_logs.linked_source} onSelect={(name) => edit({ ...value, break_logs: { ...value.break_logs, linked_source: name } })} onFields={(type, fields) => edit({ ...value, break_logs: { ...value.break_logs, fields: { ...value.break_logs.fields, [type]: fields } } })} onSave={() => { if (!delayInvalid) save.mutate(); }} saving={save.isPending} />
    {value.break_logs.linked_source && <div className="mb-4 text-xs">
      {settings.can_write ? <label className="flex flex-wrap items-center gap-2">Ingestion delay override (seconds)<input aria-label="Ingestion delay override (seconds)" type="number" min={0} max={3600} step={1} value={delayDraft ?? value.break_logs.ingest_delay ?? ""} placeholder="Source default" disabled={save.isPending} aria-invalid={delayInvalid} className="input w-36" onChange={event => {
        const raw = event.target.value, number = Number(raw);
        setDelayDraft(raw);
        const invalid = event.target.validity.badInput || raw !== "" && (!Number.isInteger(number) || number < 0 || number > 3600);
        setDelayInvalid(invalid);
        if (invalid) return;
        const breakLogs = { ...value.break_logs };
        if (raw === "") delete breakLogs.ingest_delay; else breakLogs.ingest_delay = number;
        edit({ ...value, break_logs: breakLogs });
      }} /></label> : <p>Ingestion delay: {value.break_logs.ingest_delay === undefined ? "source default" : `${value.break_logs.ingest_delay} seconds`}</p>}
      <p className="mt-2 text-ink-300">{delayInvalid ? "Enter a whole number from 0 to 3600, or leave empty for the source default." : "Optional. Leave empty to use the built-in source delay; no setup is required."}</p>
    </div>}
    {save.isError && <p role="alert" className="my-3 text-sev-critical">Settings could not be saved. Check your permission and mapping, then try again.</p>}
    {saved && <p role="status" className="my-3 text-sev-ok">Agent settings saved.</p>}
    {settings.can_write && <div className="flex justify-end gap-2 border-t border-ink-600 pt-4"><button type="button" className="btn" disabled={!draft && !delayInvalid || save.isPending} onClick={() => { setDraft(null); setDelayDraft(undefined); setSaved(false); setDelayInvalid(false); }}>Discard changes</button><button type="button" className="btn btn-primary" disabled={!draft || delayInvalid || save.isPending} onClick={() => save.mutate()}><Save size={14} />{save.isPending ? "Saving..." : "Save settings"}</button></div>}
    <section aria-label="Agent learning" className="mt-6 border-t border-ink-600 pt-5">
      <h3 className="text-sm font-semibold text-ink-100">Agent learning</h3>
      <p className="mt-2 text-ink-300">Not available yet.</p>
      <div className="mt-3 divide-y divide-ink-600">{rows.map((row) => <div key={row.field} className="flex min-w-0 items-center gap-3 py-3">
        <row.icon size={18} className="shrink-0 text-ink-400" aria-hidden="true" />
        <div className="min-w-0 flex-1"><h4 className="font-medium text-ink-200">{row.title}</h4><p className="mt-1 text-ink-400">{row.status}</p>
          {row.field === "logs" && <div className="mt-2 flex flex-wrap gap-3"><span className="break-all text-ink-400">{settings.learning.scope.include?.length ? `Include: ${settings.learning.scope.include.join(", ")}` : "All namespaces"}{settings.learning.scope.exclude?.length ? `; exclude: ${settings.learning.scope.exclude.join(", ")}` : ""}</span><button type="button" className="text-ink-400 disabled:cursor-not-allowed" disabled>Edit scope</button></div>}
        </div>
        <input type="checkbox" role="switch" aria-label={`${row.title} learning`} className="size-4 shrink-0 accent-accent" checked={settings.learning[row.field]} disabled />
      </div>)}</div>
    </section>
  </>;
}