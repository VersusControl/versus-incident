import { useEffect, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Loader2 } from "lucide-react";
import { api, type CountSettings, type CountWindow } from "@/lib/api";
import { COUNT_WINDOW_LABELS } from "@/lib/countWindow";
import { ErrorBox } from "@/components/feedback";
import { InfoHint } from "@/components/InfoHint";
import { useToast } from "@/components/toastContext";

// COUNT_WINDOWS are the lookbacks every incident-count surface can be bounded
// to, in the order they are offered. "All time" keeps the historical behaviour
// for anyone who wants it.
const COUNT_WINDOWS: {
  value: CountWindow;
  description: string;
}[] = [
  {
    value: "24h",
    description: "Counts cover the last day only — tightest view of right now.",
  },
  {
    value: "7d",
    description: "A week of load. The default, and the usual on-call horizon.",
  },
  { value: "30d", description: "A month of load." },
  { value: "90d", description: "A quarter of load." },
  {
    value: "all",
    description:
      "Every incident ever recorded. The total only grows, so it stops describing current load.",
  },
];

// CountSettingsControl — the runtime panel for how far back incident COUNTS
// look. One setting drives every count surface (the header badge, the Now
// tiles, the Incidents origin/status tabs), so they can never disagree.
export function CountSettingsControl() {
  const qc = useQueryClient();
  const toast = useToast();

  const settings = useQuery({
    queryKey: ["count-settings"],
    queryFn: api.getCountSettings,
    staleTime: 30_000,
  });

  const [form, setForm] = useState<CountSettings | null>(null);
  useEffect(() => {
    if (settings.data) setForm(settings.data);
  }, [settings.data]);

  const save = useMutation({
    mutationFn: (s: CountSettings) => api.updateCountSettings(s),
    onSuccess: (saved) => {
      setForm(saved);
      qc.setQueryData(["count-settings"], saved);
      // Every count surface is now stale — they all read this window.
      qc.invalidateQueries({ queryKey: ["incidents"] });
      qc.invalidateQueries({ queryKey: ["incident-index"] });
      toast.push({ tone: "ok", title: "Count window saved" });
    },
    onError: (err) => {
      toast.push({
        tone: "error",
        title: "Couldn't save the count window",
        description: err instanceof Error ? err.message : String(err),
      });
    },
  });

  // Error first: on a failed load `form` is still null, so a `!form` check
  // ahead of this one would spin on "Loading…" forever.
  if (settings.isError) {
    return (
      <div className="card p-4">
        <ErrorBox error={settings.error} />
      </div>
    );
  }
  if (settings.isLoading || !form) {
    return (
      <div className="card p-4 text-sm text-ink-300">
        <Loader2 size={14} className="mr-2 inline animate-spin" />
        Loading count settings…
      </div>
    );
  }

  const active = COUNT_WINDOWS.find((o) => o.value === form.window);

  return (
    <div className="card space-y-4 p-4">
      <div>
        <h3 className="text-sm font-semibold text-ink-100">
          Incident count window
          <InfoHint
            label="About the incident count window"
            text="How far back the incident counts look. Applies to the header badge, the Now tiles and the Incidents tabs together, so no two surfaces disagree."
          />
        </h3>
        <p className="text-2xs text-ink-400">
          Counts describe recent load rather than an all-time total.
        </p>
      </div>

      <div>
        <label className="field-label" htmlFor="count-window">
          Count incidents from
        </label>
        <select
          id="count-window"
          className="input"
          value={form.window}
          onChange={(e) =>
            setForm((f) =>
              f ? { ...f, window: e.target.value as CountWindow } : f,
            )
          }
        >
          {COUNT_WINDOWS.map((o) => (
            <option key={o.value} value={o.value}>
              {COUNT_WINDOW_LABELS[o.value].long}
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
