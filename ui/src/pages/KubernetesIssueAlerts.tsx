import { useId, useRef } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { Bell, ExternalLink, Settings } from "lucide-react";
import type { KubernetesIssueAlertsInput, KubernetesIssueAlertsSettings } from "@/lib/api";
import { useKubernetesCluster, useKubernetesQuery } from "@/lib/useKubernetesCluster";
import { useToast } from "@/components/toastContext";

export function KubernetesIssueAlerts() {
  const { client, key } = useKubernetesCluster();
  const queryClient = useQueryClient();
  const { push } = useToast();
  const descriptionId = useId();
  const saving = useRef(false);
  const queryKey = key("issue-alerts");
  const mutation = useMutation({
    mutationFn: (input: KubernetesIssueAlertsInput) => client.kubernetesSaveIssueAlerts(input),
    onMutate: async (input) => {
      await queryClient.cancelQueries({ queryKey, exact: true });
      const previous = queryClient.getQueryData<KubernetesIssueAlertsSettings>(queryKey);
      if (previous) queryClient.setQueryData(queryKey, { ...previous, ...input });
      return { previous };
    },
    onSuccess: (settings, input, context) => {
      queryClient.setQueryData(queryKey, settings);
      push({
        tone: "ok",
        title: input.enabled && !context?.previous?.enabled
          ? "Alerts on. Issues that break from now on will alert."
          : input.enabled ? "Alert level saved." : "Alerts off.",
      });
    },
    onError: (error, _input, context) => {
      if (context?.previous) queryClient.setQueryData(queryKey, context.previous);
      push({ tone: "error", title: "Couldn't save issue alerts.", description: error.message });
    },
    onSettled: () => {
      saving.current = false;
      void queryClient.invalidateQueries({ queryKey, exact: true });
    },
  });
  const query = useKubernetesQuery({
    queryKey: ["issue-alerts"],
    queryFn: ({ signal }) => client.kubernetesIssueAlerts(signal),
    enabled: !mutation.isPending,
    retry: false,
  });

  if (query.isError) return (
    <div role="status" className="flex flex-wrap items-center gap-2 border-b border-ink-700 p-3 text-xs text-ink-300">
      Issue alert settings are unavailable.
      <button type="button" className="btn" onClick={() => void query.refetch()}>Retry alert settings</button>
    </div>
  );
  if (!query.data) return <p role="status" className="border-b border-ink-700 p-3 text-xs text-ink-300">Loading issue alert settings…</p>;
  const settings = query.data;
  const disabled = !settings.can_write || !settings.available || mutation.isPending;
  const save = (input: KubernetesIssueAlertsInput) => {
    // Serialize writes so an older response cannot overwrite a newer choice.
    if (disabled || saving.current) return;
    saving.current = true;
    mutation.mutate(input);
  };

  return (
    <div className="min-w-0 space-y-2 border-b border-ink-700 p-3">
      <div className="flex min-w-0 flex-wrap items-center gap-x-4 gap-y-3">
        <div className="flex w-full items-center gap-2 sm:w-auto">
          <Bell size={15} aria-hidden className="shrink-0 text-ink-300" />
          <span className="text-xs font-medium text-ink-100">Alert on new issues</span>
          <button type="button" role="switch" aria-label="Alert on new issues" aria-checked={settings.enabled}
            aria-describedby={descriptionId} disabled={disabled}
            onClick={() => save({ enabled: !settings.enabled, level: settings.level })}
            className={`ml-auto inline-flex h-5 w-9 shrink-0 items-center rounded-full border p-0.5 focus-visible:outline focus-visible:outline-2 focus-visible:outline-accent disabled:cursor-not-allowed disabled:opacity-50 ${settings.enabled ? "border-accent bg-accent" : "border-ink-500 bg-ink-700"}`}>
            <span className={`h-3.5 w-3.5 rounded-full bg-ink-50 transition-transform motion-reduce:transition-none ${settings.enabled ? "translate-x-4" : "translate-x-0"}`} />
          </button>
        </div>
        {settings.enabled && <div role="group" aria-label="Issue alert level" className="flex w-full rounded border border-ink-500 p-0.5 sm:w-auto">
          {(["critical", "warning"] as const).map((level) => (
            <button key={level} type="button" disabled={disabled} aria-pressed={settings.level === level}
              onClick={() => save({ enabled: true, level })}
              className={`flex-1 rounded px-3 py-1 text-xs focus-visible:outline focus-visible:outline-2 focus-visible:outline-accent disabled:cursor-not-allowed disabled:opacity-50 ${settings.level === level ? "bg-ink-600 font-medium text-ink-50" : "text-ink-300 hover:bg-ink-700"}`}>
              {level === "critical" ? "Critical" : "Warning"}
            </button>
          ))}
        </div>}
        {mutation.isPending && <span role="status" className="text-2xs text-ink-300">Saving…</span>}
      </div>
      <div id={descriptionId} className="space-y-1 text-2xs text-ink-300">
        {settings.enabled && <p>{settings.level === "critical" ? "Critical sends critical issues only" : "Warning sends warning and critical issues"}</p>}
        {!settings.can_write && <p>Ask an admin to change alerts</p>}
        {settings.reason === "demand_capture" && <p>Alerts need continuous change capture</p>}
        {!settings.available && settings.reason !== "demand_capture" && <p>Issue alerts are unavailable for this cluster</p>}
        {!settings.channels_enabled && <p>No alert channel is on</p>}
        {!settings.links_ready && <p>Set public_host to add Timeline and agent links</p>}
      </div>
      <div className="flex flex-wrap items-center gap-2 text-2xs text-ink-300">
        <span>Sends through your alert channels</span>
        <a href="https://docs.versusincident.com/#/agent/channels" target="_blank" rel="noopener noreferrer" className="btn inline-flex items-center gap-1.5">
          <Settings size={12} aria-hidden />Channel settings<ExternalLink size={12} aria-hidden />
        </a>
      </div>
    </div>
  );
}
