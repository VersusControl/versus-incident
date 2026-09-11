import { useEffect, useState } from "react";
import { AlertTriangle, Loader2 } from "lucide-react";
import { ApiError, type ServiceHealthSettings } from "@/lib/api";
import { ErrorBox } from "@/components/feedback";
import { AdminAccessNotice } from "@/components/AdminAccessNotice";
import { useEffectiveRole } from "@/lib/useEffectiveRole";
import { useToast } from "@/components/toastContext";
import { useServiceHealthSettingsQuery, useUpdateServiceHealthSettings } from "@/lib/useServiceHealth";

function validate(settings: ServiceHealthSettings): Record<string, string> {
  const errors: Record<string, string> = {};
  if (settings.interval_seconds < 30 || settings.interval_seconds > 900) {
    errors.interval_seconds = "Use 30 to 900 seconds.";
  }
  if (settings.window_seconds < 60 || settings.window_seconds > 3600) {
    errors.window_seconds = "Use 60 to 3,600 seconds.";
  } else if (settings.window_seconds < settings.interval_seconds) {
    errors.window_seconds = "Window must be at least as long as the interval.";
  }
  return errors;
}

export function ServiceHealthSettingsControl() {
  const toast = useToast();
  const access = useEffectiveRole();
  const settings = useServiceHealthSettingsQuery();
  const [form, setForm] = useState<ServiceHealthSettings | null>(null);
  const [saveError, setSaveError] = useState<unknown>(null);

  useEffect(() => {
    if (settings.data) setForm(settings.data);
  }, [settings.data]);

  const save = useUpdateServiceHealthSettings({
    onSuccess: (saved) => {
      setForm(saved);
      setSaveError(null);
      toast.push({ tone: "ok", title: "Service Health Timing saved" });
    },
    onError: (error) => setSaveError(error),
  });

  if (settings.isError) return <div className="card p-4"><ErrorBox error={settings.error} /></div>;
  if (settings.isPending || !form || access.loading) {
    return <div className="card p-4 text-sm text-ink-300"><Loader2 size={14} className="mr-2 inline animate-spin" aria-hidden /> Loading Service Health settings…</div>;
  }

  const readOnly = access.enterprise && !access.isAdmin;
  const errors = validate(form);
  const invalid = Object.keys(errors).length > 0;
  const dirty = settings.data != null && (
    form.interval_seconds !== settings.data.interval_seconds ||
    form.window_seconds !== settings.data.window_seconds
  );
  const conflict = saveError instanceof ApiError && saveError.status === 409;

  return (
    <section className="card space-y-4 p-4" aria-labelledby="service-health-settings-title">
      <div>
        <h3 id="service-health-settings-title" className="text-sm font-semibold text-ink-100">Service Health Timing</h3>
        <p className="text-2xs text-ink-400">Service Health is always enabled. Timing changes apply without a restart.</p>
      </div>

      {settings.data.diagnostic && (
        <div className="flex gap-2 text-xs text-sev-warn" role="status">
          <AlertTriangle size={14} className="shrink-0" aria-hidden /> Stored timing was invalid, so effective defaults are shown.
        </div>
      )}

      {readOnly ? (
        <>
          <dl className="grid grid-cols-1 gap-3 text-xs sm:grid-cols-2">
            <div><dt className="text-ink-400">Interval</dt><dd className="mt-1 font-medium text-ink-100">{form.interval_seconds} seconds</dd></div>
            <div><dt className="text-ink-400">Window</dt><dd className="mt-1 font-medium text-ink-100">{form.window_seconds} seconds</dd></div>
          </dl>
          <AdminAccessNotice reason={access.hasSession ? "role" : "sign-in"} />
        </>
      ) : (
        <>
          <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
            <label>
              <span className="field-label">Service Health Interval</span>
              <input
                className="input"
                type="number"
                min={30}
                max={900}
                step={1}
                value={form.interval_seconds}
                aria-invalid={Boolean(errors.interval_seconds)}
                aria-describedby={errors.interval_seconds ? "service-health-interval-error" : undefined}
                onChange={(event) => setForm({ ...form, interval_seconds: Number(event.target.value) })}
              />
              <p id="service-health-interval-error" className={`mt-1 text-2xs ${errors.interval_seconds ? "text-sev-critical" : "text-ink-400"}`}>
                {errors.interval_seconds ?? "How often the assessment is refreshed, from 30 to 900 seconds."}
              </p>
            </label>
            <label>
              <span className="field-label">Service Health Window</span>
              <input
                className="input"
                type="number"
                min={60}
                max={3600}
                step={1}
                value={form.window_seconds}
                aria-invalid={Boolean(errors.window_seconds)}
                aria-describedby={errors.window_seconds ? "service-health-window-error" : undefined}
                onChange={(event) => setForm({ ...form, window_seconds: Number(event.target.value) })}
              />
              <p id="service-health-window-error" className={`mt-1 text-2xs ${errors.window_seconds ? "text-sev-critical" : "text-ink-400"}`}>
                {errors.window_seconds ?? "How much recent evidence is assessed, from 60 to 3,600 seconds."}
              </p>
            </label>
          </div>

          {saveError && (
            <div className="flex flex-col items-start gap-2 text-xs text-sev-critical" role="alert">
              <span>{conflict ? "Settings changed in another session. Reload the latest values before saving again." : saveError instanceof Error ? saveError.message : String(saveError)}</span>
              {conflict && <button type="button" className="btn" onClick={() => { setSaveError(null); settings.refetch(); }}>Reload values</button>}
            </div>
          )}

          <div className="flex justify-end">
            <button type="button" className="btn btn-primary" disabled={!dirty || invalid || save.isPending} onClick={() => save.mutate(form)}>
              {save.isPending ? <><Loader2 size={12} className="animate-spin" aria-hidden /> Saving…</> : "Save"}
            </button>
          </div>
        </>
      )}
    </section>
  );
}