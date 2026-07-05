import { useId, useState } from "react";
import {
  useMutation,
  useQuery,
  useQueryClient,
  type QueryKey,
} from "@tanstack/react-query";
import { api, type ServiceOverrideSource } from "@/lib/api";
import {
  assignableServices,
  cellOverrideInput,
  resolveServiceCell,
} from "@/lib/serviceOverride";
import { displayService } from "@/lib/format";
import { Modal } from "./Modal";
import { Dropdown } from "./Dropdown";
import { Spinner } from "./feedback";
import { useToast } from "./toastContext";

// ServiceCell renders a signal's Service column: the effective service name plus
// a "pending" chip when a just-created reassignment is still settling.
// Attribution correction itself is not a control in this cell — the reassign
// flow lives in the checkbox selection + action bar ("Assign to service", which
// opens the exported ReassignModal for the selected rows), so a row has a
// single, consistent action affordance and the cell stays read-only.
//
// The match key is source-appropriate: a log pattern id (source_type "log") or
// a metric/trace signal name (source_type "metric"/"trace"). A matching override
// is reflected IMMEDIATELY — the cell shows the override target the instant the
// override exists (driven off the shared override query), so the reassign gives
// instant feedback on logs, metrics, and traces alike. The "(pending)" chip then
// rides purely on whether the signal's own attribution has caught up, so it means
// the same thing on all three surfaces (never "instant here, stuck there").
export function ServiceCell({
  service,
  sourceType,
  match,
}: {
  // service is the currently-attributed service (may be _unknown / blank).
  service?: string | null;
  sourceType: ServiceOverrideSource;
  // match is the source-appropriate key: a log pattern id (source_type "log")
  // or a metric/trace signal name (source_type "metric"/"trace"). It keys the
  // pending-override chip.
  match: string;
}) {
  // A durable override is stored the instant an operator reassigns, but the
  // backend read models catch up unevenly — the logs patterns reader re-points
  // on write while the metrics/traces baseline reader only re-points on the
  // NEXT re-observation, which with no live traffic reads as "reassign did
  // nothing". Resolve the effective attribution off the override list the UI
  // already fetches (shared query key, deduped across every cell) so the cell
  // shows the override target IMMEDIATELY on every surface, with a "(pending)"
  // chip while the signal's own attribution hasn't caught up. Degrades cleanly:
  // no data → the signal's own service, no chip.
  const overrides = useQuery({
    queryKey: ["service-overrides"],
    queryFn: api.listServiceOverrides,
  });
  const resolved = resolveServiceCell(
    overrides.data,
    cellOverrideInput(sourceType, match),
    service,
  );
  const pendingNoun = sourceType === "log" ? "pattern" : "signal";

  return (
    <span className="inline-flex items-center gap-1.5">
      <span
        className="truncate text-ink-100"
        title={resolved.service ?? undefined}
      >
        {displayService(resolved.service)}
      </span>
      {resolved.pending && (
        <span
          className="inline-flex shrink-0 items-center gap-0.5 whitespace-nowrap rounded-full
                     border border-accent/40 bg-accent/10 px-1.5 py-0.5 text-2xs font-medium text-accent"
          title={`Reassignment to ${resolved.service} is saved and takes effect the next time this ${pendingNoun} is seen.`}
        >
          pending
        </span>
      )}
    </span>
  );
}

// ReassignModal is the focus-trapped picker (built on the accessible Modal
// base). Its options are the assignable services (every known service minus the
// _unknown fallback). Confirming re-points EVERY selected match to the chosen
// service and closes on success; a backend rejection stays open with the
// verbatim error so the operator can pick a different target. It reassigns the
// SELECTION (1..N matches) so a single-row and a multi-row correction share one
// flow — opened from the "Assign to service" action in the checkbox action bar.
export function ReassignModal({
  sourceType,
  matches,
  invalidateKeys,
  onClose,
  onDone,
}: {
  sourceType: ServiceOverrideSource;
  // matches are the source-appropriate keys: log pattern ids (source_type
  // "log") or metric/trace signal names. One or many.
  matches: string[];
  invalidateKeys: QueryKey[];
  onClose: () => void;
  // onDone fires after a successful reassign (before onClose) so the caller can
  // clear its selection.
  onDone?: () => void;
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

  const noun = sourceType === "log" ? "pattern" : "signal";
  const count = matches.length;
  const countLabel = count === 1 ? `1 ${noun}` : `${count} ${noun}s`;

  const reassign = useMutation({
    mutationFn: async (service: string) => {
      // Re-point every selected match. createServiceOverride replaces the
      // same-key rule, so re-running is idempotent per match.
      for (const match of matches) {
        await api.createServiceOverride({ source_type: sourceType, match, service });
      }
      return service;
    },
    onSuccess: (service) => {
      qc.invalidateQueries({ queryKey: ["service-overrides"] });
      for (const key of invalidateKeys) qc.invalidateQueries({ queryKey: key });
      toast.push({
        tone: "ok",
        title: `Reassigned to ${service}`,
        description: `${countLabel} — takes effect on the next sighting`,
      });
      onDone?.();
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
          <span className="font-medium text-ink-100">{countLabel}</span> to the
          service you pick. This creates a durable attribution override.
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
