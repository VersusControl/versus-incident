import { useEffect, useId, useRef, useState } from "react";
import { Check, ChevronDown, ExternalLink, FlaskConical, Link2, Lock, Save, Settings } from "lucide-react";
import { Link } from "react-router-dom";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { ApiError, type KubernetesAgentSettings, type KubernetesLogFields, type KubernetesLogSourceType } from "@/lib/api";
import { useKubernetesCluster } from "@/lib/useKubernetesCluster";

const sourceTypes: Array<{ type: KubernetesLogSourceType; name: string; tile: string; hint: string; scope: string; noun: string; defaults: KubernetesLogFields }> = [
  { type: "loki", name: "Loki", tile: "Lk", hint: "labels", scope: "Selector from source", noun: "label", defaults: { namespace: "namespace", pod: "pod", container: "container" } },
  { type: "elasticsearch", name: "Elasticsearch", tile: "Es", hint: "fields", scope: "Index pattern from source", noun: "field", defaults: { namespace: "kubernetes.namespace_name", pod: "kubernetes.pod_name", container: "kubernetes.container_name", time: "@timestamp", message: "message" } },
  { type: "cloudwatchlogs", name: "CloudWatch Logs", tile: "Cw", hint: "log group", scope: "Log group from source", noun: "field", defaults: { namespace: "kubernetes.namespace_name", pod: "kubernetes.pod_name", container: "kubernetes.container_name", message: "log" } },
  { type: "graylog", name: "Graylog", tile: "Gl", hint: "stream", scope: "Stream from source", noun: "field", defaults: { namespace: "kubernetes_namespace_name", pod: "kubernetes_pod_name", container: "kubernetes_container_name" } },
  { type: "splunk", name: "Splunk", tile: "Sp", hint: "index and sourcetype", scope: "Index and sourcetype from source", noun: "field", defaults: { namespace: "namespace", pod: "pod", container: "container_name" } },
  { type: "signoz", name: "SigNoz", tile: "Sz", hint: "resource attributes", scope: "Resource attributes from OpenTelemetry", noun: "attribute", defaults: { namespace: "k8s.namespace.name", pod: "k8s.pod.name", container: "k8s.container.name" } },
];

function Health({ source }: { source: KubernetesAgentSettings["sources"][number] }) {
  const label = { healthy: "healthy", slow: "slow", failing: "failing", not_checked: "not checked yet" }[source.health] ?? "not checked yet";
  return <span className="inline-flex items-center gap-1.5 text-2xs text-ink-300" title={source.checked_at ? `Last checked ${new Date(source.checked_at).toLocaleString()}` : "Not checked yet"}><i aria-hidden="true" className={`size-1.5 shrink-0 rounded-full ${source.health === "healthy" ? "bg-sev-ok" : source.health === "slow" ? "bg-sev-warn" : source.health === "failing" ? "bg-sev-critical" : "bg-ink-400"}`} />{label}</span>;
}

function testError(error: Error) {
  if (!(error instanceof ApiError)) return "Link test unavailable. Try again.";
  const body = error.body;
  const code = body && typeof body === "object" && "error" in body ? body.error : error.message;
  if (code === "unauthorized") return "Source refused the read. Check the token permissions.";
  if (code === "not_found") return "Source or workload not found. Check the source and cluster.";
  if (code === "timeout") return "Source timed out. Try again.";
  if (code === "invalid_mapping") return "Invalid mapping. Check the field names.";
  if (error.status === 403) return "Settings write permission is required to test this link.";
  return "Link test unavailable. Try again.";
}

export function KubernetesLinkedSource({ settings, fields, selected, savedSource, onSelect, onFields, onSave, saving }: {
  settings: KubernetesAgentSettings; fields: KubernetesAgentSettings["break_logs"]["fields"]; selected: string; savedSource: string;
  onSelect: (name: string) => void; onFields: (type: KubernetesLogSourceType, fields: KubernetesLogFields) => void; onSave: () => void; saving: boolean;
}) {
  const [open, setOpen] = useState(false);
  const [openUp, setOpenUp] = useState(false);
  const trigger = useRef<HTMLButtonElement>(null);
  const pickerID = useId();
  const source = (settings.sources ?? []).find((item) => item.name === selected);
  const config = sourceTypes.find((item) => item.type === source?.type);
  return <section aria-label="Log capture at break" className="min-w-0 border-t border-ink-600 py-5 text-xs">
    <h3 className="text-sm font-semibold text-ink-100">Log capture at break</h3>
    <p className="mt-2 text-ink-300">When a workload breaks, Versus captures the logs around it. Kubernetes container logs are always read first.</p>
    <div className="flex flex-wrap items-center justify-between gap-3 border-b border-ink-600 py-4"><div><h4 className="font-medium text-ink-100">Kubernetes container logs</h4><p className="mt-1 text-ink-300">Previous and current containers, 500 lines and 64 KiB per container, up to 5 pods and 3 containers per pod. Kept 24 hours, up to 8 MiB per cluster.</p></div><span className="pill pill-good">Always on</span></div>
    <div className="relative py-4" onKeyDown={(event) => { if (event.key === "Escape" && open) { setOpen(false); trigger.current?.focus(); } }} onBlur={(event) => { if (!event.currentTarget.contains(event.relatedTarget)) setOpen(false); }}>
      <div className="flex flex-wrap items-center justify-between gap-3"><div><h4 className="font-medium text-ink-100">Linked log data source</h4><p className="mt-1 text-ink-300">Optional, one per cluster. Fills gaps, including logs of pods that no longer exist.</p></div>
        {settings.can_write ? <button ref={trigger} type="button" className="btn min-w-48 max-w-full justify-between" disabled={!settings.sources?.length || saving} aria-expanded={open} aria-controls={pickerID} onClick={() => { setOpenUp(window.innerHeight - (trigger.current?.getBoundingClientRect().bottom ?? 0) < 330); setOpen(!open); }}>{selected || (settings.sources?.length ? "None" : "None available")}<ChevronDown size={14} aria-hidden="true" /></button> : <span className="pill max-w-full break-all whitespace-normal">{selected || "None"}</span>}
      </div>
      {open && <div id={pickerID} aria-label="Configured log sources" className={`fixed inset-x-3 bottom-16 top-16 z-20 overflow-y-auto rounded-control border border-ink-500 bg-surface-raised p-3 shadow-overlay sm:absolute sm:inset-x-0 sm:max-h-[min(28rem,70vh)] ${openUp ? "sm:bottom-full sm:top-auto" : "sm:top-full sm:bottom-auto"} lg:left-auto lg:w-[min(50rem,100%)]`}>
        <button type="button" className="flex min-h-9 w-full flex-wrap items-center gap-2 text-left text-ink-100" onClick={() => { onSelect(""); setOpen(false); trigger.current?.focus(); }}>None <span className="text-ink-300">Kubernetes container logs only</span>{!selected && <Check size={14} />}</button>
        <div className="grid gap-4 border-y border-ink-600 py-3 sm:grid-cols-3">{sourceTypes.map((type) => {
          const sources = (settings.sources ?? []).filter((item) => item.type === type.type);
          return sources.length ? <section aria-label={`${type.name} sources`} key={type.type}><h5 className="mb-2 flex items-center gap-2 font-medium text-ink-200"><span className="rounded-control border border-ink-500 px-1 text-2xs">{type.tile}</span>{type.name}</h5>{sources.map((item) => <button type="button" key={item.name} className={`mb-1 block min-h-12 w-full rounded-control p-2 text-left ${selected === item.name ? "bg-accent-subtle" : "hover:bg-ink-700"}`} onClick={() => { onSelect(item.name); setOpen(false); trigger.current?.focus(); }}><span className="flex items-center justify-between gap-2 break-all text-ink-100">{item.name}{selected === item.name && <Check size={12} className="shrink-0" />}</span><span className="flex flex-wrap items-center justify-between gap-1"><span className="text-2xs text-ink-300">{type.hint}</span><Health source={item} /></span></button>)}</section> : null;
        })}</div>
        <Link to="/settings?tab=agent-sources" className="mt-3 flex min-h-8 items-center gap-2 text-link"><Settings size={13} />Add a log data source in Settings, Data sources</Link>
      </div>}
    </div>
    {settings.linked_source_missing && <p role="status" className="mb-3 text-sev-warning">The saved source {savedSource} is no longer available. It is not being read.</p>}
    {!settings.sources?.length && <div className="space-y-3 rounded-control border border-dashed border-ink-500 p-5"><Settings size={20} className="text-ink-300" /><h4 className="font-medium text-ink-100">No log data source is configured</h4><p className="text-ink-300">Loki, Elasticsearch, CloudWatch Logs, Graylog, Splunk and SigNoz are supported.</p><div className="flex flex-wrap gap-4"><a href="https://docs.versusincident.com/#/agent/data-sources" target="_blank" rel="noopener noreferrer" className="inline-flex items-center gap-1 text-link">Read the data sources guide<ExternalLink size={12} /></a><Link className="text-link" to="/settings?tab=agent-sources">Data sources settings</Link></div><p className="text-ink-300">Kubernetes container logs are still captured at every break.</p></div>}
    <div aria-label="Read order at break" className="my-4 max-w-lg space-y-2 border-l-2 border-accent pl-4"><h4 className="font-semibold text-ink-100">Read order at break</h4><p className="flex justify-between gap-3"><span>1 Kubernetes container logs</span><span className="text-ink-300">Always</span></p><p className="flex justify-between gap-3"><span className="break-all">2 {source?.name ?? "None"}</span><span className="text-ink-300">{source && selected === savedSource ? "Linked" : source ? "Not saved" : "Not linked"}</span></p><p className="text-ink-300">Only one linked source per cluster</p></div>
    {source && config && <Mapping key={source.name} source={source} config={config} fields={fields[source.type]} canWrite={settings.can_write} linked={savedSource === source.name} onFields={(value) => onFields(source.type, value)} onSave={onSave} saving={saving} />}
  </section>;
}

function Mapping({ source, config, fields, canWrite, linked, onFields, onSave, saving }: {
  source: KubernetesAgentSettings["sources"][number]; config: typeof sourceTypes[number]; fields: KubernetesLogFields; canWrite: boolean; linked: boolean; onFields: (fields: KubernetesLogFields) => void; onSave: () => void; saving: boolean;
}) {
  const { client, key } = useKubernetesCluster();
  const queryClient = useQueryClient();
  const test = useMutation({ mutationFn: () => client.kubernetesTestLink(source.name, fields), onSettled: () => queryClient.invalidateQueries({ queryKey: key("kubernetes-agent-settings") }) });
  const fieldSignature = JSON.stringify(fields);
  const { reset } = test;
  useEffect(() => { reset(); }, [fieldSignature, reset]);
  const valid = Object.values(fields).every((value) => !value || /^[A-Za-z_@][A-Za-z0-9_.\-/@]{0,127}$/.test(value)) && new Set([fields.namespace, fields.pod, fields.container]).size === 3;
  let plans: Array<{ from?: string; to?: string }> = [];
  try { const parsed: unknown = JSON.parse(test.data?.preview ?? "[]"); if (Array.isArray(parsed)) plans = parsed; } catch { plans = []; }
  return <section aria-label={`${config.name} mapping`} className="min-w-0 border-t border-ink-600 pt-4">
    <header className="mb-3 flex flex-wrap items-center gap-2"><span className="rounded-control border border-ink-500 px-1.5 py-0.5 text-2xs">{config.tile}</span><h4 className="text-sm font-semibold text-ink-100">{config.name} mapping</h4><span className="pill break-all whitespace-normal">{source.name}</span><Health source={source} />{linked && <span className="pill pill-good ml-auto">Linked</span>}</header>
    <p className="mb-4 flex flex-wrap items-center gap-2 bg-surface px-3 py-2 text-ink-300"><Lock size={12} aria-hidden="true" />{config.scope}: <span className="min-w-0 break-all font-mono text-ink-100">{source.scope || "Not provided by source"}</span><span className="ml-auto">Read only</span></p>
    <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">{(Object.keys(config.defaults) as Array<keyof KubernetesLogFields>).map((field) => <label className="min-w-0 text-ink-300" key={field}>{field[0].toUpperCase() + field.slice(1)} {config.noun}{canWrite ? <><input className="input mt-1 w-full font-mono" aria-label={`${field[0].toUpperCase() + field.slice(1)} ${config.noun}`} value={fields[field] ?? ""} maxLength={128} placeholder={config.defaults[field]} onChange={(event) => onFields({ ...fields, [field]: event.target.value })} /><span className="mt-1 block break-all text-2xs">{fields[field] === config.defaults[field] ? "default" : `default: ${config.defaults[field]}`}</span></> : <span className="mt-1 block break-all font-mono text-ink-100">{fields[field] || config.defaults[field]}</span>}</label>)}</div>
    {!valid && <p role="alert" className="mt-2 text-sev-warning">Use distinct namespace, pod and container names with letters, digits, underscores, dots, slashes, hyphens or @.</p>}
    <div className="my-4"><div className="mb-2 flex flex-wrap justify-between gap-2"><h5 className="font-medium text-ink-100">Query preview</h5>{plans[0]?.from && plans[0]?.to && <span className="pill font-mono">{new Date(plans[0].from).toLocaleTimeString()} - {new Date(plans[0].to).toLocaleTimeString()}</span>}</div><pre className="max-h-64 overflow-auto whitespace-pre-wrap break-all rounded-control bg-surface p-3 text-2xs text-ink-200">{test.data?.preview ?? "Not tested"}</pre>{config.type === "cloudwatchlogs" && <p className="mt-2 text-ink-300">FilterLogEvents preserves the configured log group, stream prefix and filter pattern, then matches Kubernetes JSON fields.</p>}</div>
    <div className="flex flex-wrap items-center gap-3" aria-live="polite">
      {canWrite && <button type="button" className="btn" disabled={test.isPending || saving || !valid} onClick={() => test.mutate()}><FlaskConical size={14} />{test.isPending ? "Testing..." : test.isError ? "Retry" : "Test link"}</button>}
      {test.data && <div className="min-w-0 space-y-1"><p className={`pill max-w-full whitespace-normal ${test.data.lines ? "pill-good" : "pill-accent"}`}>{test.data.lines ? `Found ${test.data.lines.toLocaleString()} lines` : "Found 0 lines. Check the field names."}</p><p className="break-all text-ink-300">{test.data.namespace}/{test.data.workload} in the last 15 minutes</p><p className="text-ink-300">Source responded in {(test.data.duration_ms / 1000).toFixed(1)} s{test.data.truncated ? "; results truncated, coverage remains incomplete" : ""}</p></div>}
      {test.isError && <p role="alert" className="min-w-0 text-sev-critical">{testError(test.error)}</p>}
      {canWrite && <button type="button" className="btn btn-primary ml-auto" disabled={saving || test.isPending || !valid} onClick={onSave}>{linked ? <Save size={14} /> : <Link2 size={14} />}{saving ? "Saving..." : linked ? "Save" : "Use this source"}</button>}
    </div>
  </section>;
}