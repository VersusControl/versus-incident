import { useEffect, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Loader2 } from "lucide-react";
import { api, type SpikeSettings } from "@/lib/api";
import { ErrorBox } from "@/components/feedback";
import { InfoHint } from "@/components/InfoHint";
import { useToast } from "@/components/toastContext";

// SPIKE_DOCS is the docsify docs page the info icon points to (hash routing).
const SPIKE_DOCS = "https://docs.versusincident.com/#/agent/spike";

// SPIKE_BASELINE_MODES are the three baseline modes the detector can score a
// volume spike against, with a one-line description of each. The GLOBAL default
// picked here applies to every learned pattern that does not carry its own
// per-pattern override.
const SPIKE_BASELINE_MODES: {
  value: string;
  label: string;
  description: string;
}[] = [
  {
    value: "default",
    label: "Default (global baseline)",
    description: "Global EWMA rate baseline — the smoothed normal rate.",
  },
  {
    value: "average",
    label: "Average (cumulative mean)",
    description: "Cumulative mean of the rate as the center; never decays.",
  },
  {
    value: "time_of_day",
    label: "Time of day (hour-of-day)",
    description:
      "Per-hour baseline.",
  },
];

// SpikeSettingsControl — the runtime settings panel for the spike detector's
// GLOBAL default baseline mode. It reads/writes the non-secret setting via
// GET/PUT /api/admin/agent/spike-settings (gateway secret, like the rest of the
// admin surface). This is the global default; a pattern's own baseline-mode
// override still wins over it.
export function SpikeSettingsControl() {
  const qc = useQueryClient();
  const toast = useToast();

  const settings = useQuery({
    queryKey: ["spike-settings"],
    queryFn: api.getSpikeSettings,
    staleTime: 30_000,
  });

  const [form, setForm] = useState<SpikeSettings | null>(null);
  useEffect(() => {
    if (settings.data) setForm(settings.data);
  }, [settings.data]);

  const save = useMutation({
    mutationFn: (s: SpikeSettings) => api.updateSpikeSettings(s),
    onSuccess: (saved) => {
      setForm(saved);
      qc.setQueryData(["spike-settings"], saved);
      toast.push({ tone: "ok", title: "Spike settings saved" });
    },
    onError: (err) => {
      toast.push({
        tone: "error",
        title: "Couldn't save spike settings",
        description: err instanceof Error ? err.message : String(err),
      });
    },
  });

  if (settings.isLoading || !form) {
    return (
      <div className="card p-4 text-sm text-ink-300">
        <Loader2 size={14} className="mr-2 inline animate-spin" />
        Loading spike settings…
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

  const active = SPIKE_BASELINE_MODES.find((m) => m.value === form.baseline_mode);

  return (
    <div className="card space-y-4 p-4">
      <div>
        <h3 className="text-sm font-semibold text-ink-100">
          Spike baseline
          <InfoHint
            label="About the spike baseline mode"
            text="Which learned baseline a volume spike is scored against. This is the global default; a pattern's own override wins over it."
            href={SPIKE_DOCS}
            linkLabel="Read the spike detection docs"
          />
        </h3>
        <p className="text-2xs text-ink-400">
          The global baseline the spike z-score is measured against.
        </p>
      </div>

      <div>
        <label className="field-label" htmlFor="spike-baseline-mode">
          Default baseline mode
        </label>
        <select
          id="spike-baseline-mode"
          className="input"
          value={form.baseline_mode}
          onChange={(e) =>
            setForm((f) => (f ? { ...f, baseline_mode: e.target.value } : f))
          }
        >
          {SPIKE_BASELINE_MODES.map((m) => (
            <option key={m.value} value={m.value}>
              {m.label}
            </option>
          ))}
        </select>
        {active && (
          <p className="mt-1 text-2xs text-ink-400">{active.description}</p>
        )}
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
