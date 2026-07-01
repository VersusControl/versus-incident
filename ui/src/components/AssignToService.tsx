import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api, type ServiceOverrideSource } from "@/lib/api";
import { useToast } from "./toastContext";

// AssignToService — the per-row "assign this signal to a service" control that
// lives on the logs/metrics/traces signal pages. Selecting a target service
// creates a durable manual-attribution override (api.createServiceOverride)
// that re-points the signal after auto-detection. It reuses the known-service
// list (the ["services"] query) as the picker options.
//
// The control is a compact <select>: choosing a service fires the override
// immediately, shows a toast, and invalidates the overrides query. Clicks stop
// propagating so it never triggers the row's peek/navigation. Backend error
// messages (e.g. 400 "unknown target service; create it first") surface
// verbatim in the error toast.
export function AssignToService({
  sourceType,
  match,
  label = "Assign to service…",
}: {
  sourceType: ServiceOverrideSource;
  // match is the source-appropriate key: a log pattern id (source_type "log")
  // or a metric/trace signal name (source_type "metric"/"trace").
  match: string;
  label?: string;
}) {
  const qc = useQueryClient();
  const toast = useToast();
  const services = useQuery({
    queryKey: ["services"],
    queryFn: api.listServices,
  });

  const assign = useMutation({
    mutationFn: (service: string) =>
      api.createServiceOverride({ source_type: sourceType, match, service }),
    onSuccess: (_data, service) => {
      qc.invalidateQueries({ queryKey: ["service-overrides"] });
      toast.push({
        tone: "ok",
        title: `Assigned to ${service}`,
        description: match,
      });
    },
    onError: (err) =>
      toast.push({
        tone: "error",
        title: "Couldn't assign service",
        // Surface the verbatim backend message (e.g. 400 "unknown target
        // service; create it first").
        description: err.message,
      }),
  });

  // Every known service except the _unknown fallback is a valid target.
  const names = services.data
    ? Object.keys(services.data)
        .filter((n) => n !== "_unknown")
        .sort((a, b) => a.localeCompare(b))
    : [];

  return (
    <select
      className="input py-1 text-2xs"
      aria-label={`Assign ${match} to a service`}
      value=""
      disabled={assign.isPending || names.length === 0}
      title={
        names.length === 0
          ? "No services to assign to yet"
          : "Re-point this signal to a service"
      }
      onClick={(e) => e.stopPropagation()}
      onChange={(e) => {
        const svc = e.target.value;
        if (svc) assign.mutate(svc);
      }}
    >
      <option value="">{assign.isPending ? "Assigning…" : label}</option>
      {names.map((n) => (
        <option key={n} value={n}>
          {n}
        </option>
      ))}
    </select>
  );
}
