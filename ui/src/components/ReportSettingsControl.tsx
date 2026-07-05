import { useEffect, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Loader2 } from "lucide-react";
import { api, type ReportSettings } from "@/lib/api";
import { REPORT_WINDOWS } from "@/lib/reportAnalytics";
import { ErrorBox } from "@/components/feedback";
import { InfoHint } from "@/components/InfoHint";
import { useToast } from "@/components/toastContext";

// INCIDENT_REPORT_DOCS is the docsify docs page the info icon points to. The
// docs site uses hash routing, so the route `/agent/incident-report` resolves
// to this URL (the Document Engineer owns the page content in src/).
const INCIDENT_REPORT_DOCS =
  "https://docs.versusincident.com/#/agent/incident-report";

// ReportSettingsControl — the OSS runtime settings panel for the incidents
// analytics report. It reads/writes the NON-SECRET report settings store via
// GET/PUT /api/admin/reports/settings (gateway secret, like the rest of the
// OSS admin surface). There are NO secret inputs — report settings carry
// none. Toggling enable here flips whether the Incidents-page "Report" action
// is shown (surfaced through the capabilities probe), so a save invalidates it.
export function ReportSettingsControl() {
  const qc = useQueryClient();
  const toast = useToast();

  const settings = useQuery({
    queryKey: ["report-settings"],
    queryFn: api.getReportSettings,
    staleTime: 30_000,
  });
  // The channel picker offers the enabled channels from the capabilities probe.
  const cap = useQuery({
    queryKey: ["capabilities"],
    queryFn: api.capabilities,
    staleTime: 60_000,
  });

  const [form, setForm] = useState<ReportSettings | null>(null);
  useEffect(() => {
    if (settings.data) setForm(settings.data);
  }, [settings.data]);

  const save = useMutation({
    mutationFn: (s: ReportSettings) => api.updateReportSettings(s),
    onSuccess: (saved) => {
      setForm(saved);
      qc.setQueryData(["report-settings"], saved);
      qc.invalidateQueries({ queryKey: ["capabilities"] });
      toast.push({ tone: "ok", title: "Report settings saved" });
    },
    onError: (err) => {
      toast.push({
        tone: "error",
        title: "Couldn't save report settings",
        description: err instanceof Error ? err.message : String(err),
      });
    },
  });

  if (settings.isLoading || !form) {
    return (
      <div className="card p-4 text-sm text-ink-300">
        <Loader2 size={14} className="mr-2 inline animate-spin" />
        Loading report settings…
      </div>
    );
  }
  if (settings.isError) {
    return (
      <div className="card p-4">
        <ErrorBox error={settings.error} />
      </div>
    );
  }

  const channels = cap.data?.report?.channels ?? [];
  const set = <K extends keyof ReportSettings>(key: K, value: ReportSettings[K]) =>
    setForm((f) => (f ? { ...f, [key]: value } : f));

  return (
    <div className="card space-y-4 p-4">
      <div>
        <h3 className="text-sm font-semibold text-ink-100">
          Incidents report
          <InfoHint
            label="About the Incidents report"
            text="A shareable analytics dashboard over a time window — incident volume, severity breakdown and trend."
            href={INCIDENT_REPORT_DOCS}
            linkLabel="Read the incident report docs"
          />
        </h3>
        <p className="text-2xs text-ink-400">
          An incident analytics dashboard.
        </p>
      </div>

      <label className="flex items-center gap-2 text-sm text-ink-200">
        <input
          type="checkbox"
          checked={form.enable}
          onChange={(e) => set("enable", e.target.checked)}
        />
        Enable the incidents report action
      </label>

      <div>
        <label className="field-label" htmlFor="rs-default-channel">
          Default channel
        </label>
        <select
          id="rs-default-channel"
          className="input"
          value={form.default_channel}
          onChange={(e) => set("default_channel", e.target.value)}
        >
          <option value="">(none)</option>
          {channels.map((c) => (
            <option key={c} value={c}>
              {c}
            </option>
          ))}
          {/* Preserve a stored channel that isn't currently enabled so the
              select doesn't silently drop it. */}
          {form.default_channel &&
            !channels.includes(form.default_channel) && (
              <option value={form.default_channel}>
                {form.default_channel} (not enabled)
              </option>
            )}
        </select>
      </div>

      <div>
        <label className="field-label" htmlFor="rs-default-window">
          Default window
        </label>
        <select
          id="rs-default-window"
          className="input"
          value={form.default_window}
          onChange={(e) => set("default_window", e.target.value)}
        >
          {REPORT_WINDOWS.map((w) => (
            <option key={w.value} value={w.value}>
              {w.label}
            </option>
          ))}
        </select>
      </div>

      <label className="flex items-center gap-2 text-sm text-ink-200">
        <input
          type="checkbox"
          checked={form.include_chart}
          onChange={(e) => set("include_chart", e.target.checked)}
        />
        Include charts
      </label>

      <div>
        <label className="field-label" htmlFor="rs-rate">
          Rate limit (renders/min, 0 = unlimited)
        </label>
        <input
          id="rs-rate"
          type="number"
          min={0}
          className="input w-32"
          value={form.rate_per_minute}
          onChange={(e) =>
            set("rate_per_minute", Math.max(0, Number(e.target.value) || 0))
          }
        />
      </div>

      <div className="flex justify-end">
        <button
          className="btn btn-primary"
          onClick={() => form && save.mutate(form)}
          disabled={save.isPending}
        >
          {save.isPending ? (
            <>
              <Loader2 size={12} className="animate-spin" /> Saving…
            </>
          ) : (
            "Save"
          )}
        </button>
      </div>
    </div>
  );
}
