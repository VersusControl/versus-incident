import { useId, useState } from "react";
import {
  useMutation,
  useQuery,
  useQueryClient,
  type QueryKey,
} from "@tanstack/react-query";
import { Pencil } from "lucide-react";
import { api, type ServiceOverrideSource } from "@/lib/api";
import { assignableServices } from "@/lib/serviceOverride";
import { displayService } from "@/lib/format";
import { Modal } from "./Modal";
import { Dropdown } from "./Dropdown";
import { Spinner } from "./feedback";
import { useToast } from "./toastContext";

// ServiceCell renders a signal's Service column: the current service name plus
// a small pencil that opens the reassign picker. Attribution correction lives
// here — where the service is shown — instead of a separate column. Choosing a
// target service creates a durable manual-attribution override
// (api.createServiceOverride) that re-points the signal after auto-detection.
//
// The match key is source-appropriate: a log pattern id (source_type "log") or
// a metric/trace signal name (source_type "metric"/"trace"). On a successful
// reassign it invalidates ["service-overrides"] plus every `invalidateKeys`
// list query passed in (e.g. ["patterns"] or ["baselines","metric"]) so the
// corrected service shows without a manual refresh. Backend error messages
// (e.g. 400 "unknown target service; create it first") surface verbatim.
export function ServiceCell({
  service,
  sourceType,
  match,
  label,
  invalidateKeys = [],
}: {
  // service is the currently-attributed service (may be _unknown / blank).
  service?: string | null;
  sourceType: ServiceOverrideSource;
  // match is the source-appropriate key: a log pattern id (source_type "log")
  // or a metric/trace signal name (source_type "metric"/"trace").
  match: string;
  // label names the signal/pattern for the pencil's aria-label + modal title
  // (e.g. the human signal name or the pattern id).
  label: string;
  invalidateKeys?: QueryKey[];
}) {
  const [open, setOpen] = useState(false);

  return (
    <>
      <span className="inline-flex items-center gap-1.5">
        <span className="truncate text-ink-100" title={service ?? undefined}>
          {displayService(service)}
        </span>
        <button
          type="button"
          className="btn shrink-0 p-1"
          aria-label={`Reassign service for ${label}`}
          title="Reassign to another service"
          onClick={(e) => {
            e.stopPropagation();
            setOpen(true);
          }}
        >
          <Pencil size={11} aria-hidden />
        </button>
      </span>
      {open && (
        <ReassignModal
          sourceType={sourceType}
          match={match}
          label={label}
          currentService={service}
          invalidateKeys={invalidateKeys}
          onClose={() => setOpen(false)}
        />
      )}
    </>
  );
}

// ReassignModal is the focus-trapped picker (built on the accessible Modal
// base). Its options are the assignable services (every known service minus the
// _unknown fallback). Confirming fires the override and closes on success; a
// backend rejection stays open with the verbatim error so the operator can pick
// a different target.
function ReassignModal({
  sourceType,
  match,
  label,
  currentService,
  invalidateKeys,
  onClose,
}: {
  sourceType: ServiceOverrideSource;
  match: string;
  label: string;
  currentService?: string | null;
  invalidateKeys: QueryKey[];
  onClose: () => void;
}) {
  const qc = useQueryClient();
  const toast = useToast();
  const selectId = useId();
  const [choice, setChoice] = useState("");

  const services = useQuery({
    queryKey: ["services"],
    queryFn: api.listServices,
  });
  const names = assignableServices(services.data);

  const reassign = useMutation({
    mutationFn: (service: string) =>
      api.createServiceOverride({ source_type: sourceType, match, service }),
    onSuccess: (_data, service) => {
      qc.invalidateQueries({ queryKey: ["service-overrides"] });
      for (const key of invalidateKeys) qc.invalidateQueries({ queryKey: key });
      toast.push({
        tone: "ok",
        title: `Reassigned to ${service}`,
        description: label,
      });
      onClose();
    },
    onError: (err) =>
      toast.push({
        tone: "error",
        title: "Couldn't reassign service",
        // Surface the verbatim backend message (e.g. 400 "unknown target
        // service; create it first").
        description: err.message,
      }),
  });

  const currentLabel = displayService(currentService);

  return (
    <Modal
      title="Reassign to another service"
      size="sm"
      onClose={onClose}
      closeDisabled={reassign.isPending}
      footer={
        <>
          <button
            type="button"
            className="btn"
            disabled={reassign.isPending}
            onClick={onClose}
          >
            Cancel
          </button>
          <button
            type="button"
            className="btn btn-primary"
            disabled={reassign.isPending || !choice}
            onClick={() => choice && reassign.mutate(choice)}
          >
            {reassign.isPending ? <Spinner /> : "Reassign"}
          </button>
        </>
      }
    >
      <div className="space-y-3 text-xs text-ink-200">
        <p>
          Re-point{" "}
          <span className="font-mono text-ink-100">{label}</span> from{" "}
          <span className="font-medium text-ink-100">{currentLabel}</span> to
          the service you pick. This creates a durable attribution override.
        </p>
        <div>
          <label
            htmlFor={selectId}
            className="mb-1 block text-2xs uppercase tracking-wide text-ink-400"
          >
            Service
          </label>
          <Dropdown
            id={selectId}
            aria-label="Service"
            value={choice}
            onChange={setChoice}
            disabled={reassign.isPending || names.length === 0}
            placeholder={
              names.length === 0
                ? "No services to assign to yet"
                : "Choose a service…"
            }
            options={names.map((n) => ({ value: n, label: n }))}
          />
        </div>
      </div>
    </Modal>
  );
}
