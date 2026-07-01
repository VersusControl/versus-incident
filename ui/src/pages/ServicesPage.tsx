import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "react-router-dom";
import { Check, Pencil, Play, Plus, Square, Trash2, X } from "lucide-react";
import { api } from "@/lib/api";
import { fmtAbs, fmtRel } from "@/lib/format";
import { TopBar } from "@/components/TopBar";
import { Pill } from "@/components/Pill";
import { EmptyState, Spinner } from "@/components/feedback";
import { RetryableError } from "@/components/RetryableError";
import { SkRows } from "@/components/Skeleton";
import { ConfirmDialog } from "@/components/ConfirmDialog";
import { TextInputModal } from "@/components/TextInputModal";
import { Pagination } from "@/components/Pagination";
import { usePagination } from "@/lib/pagination";
import { useToast } from "@/components/toastContext";

type GraceAction = "end" | "restart";

// Pending key for one {service, action} pair — per-row mutation tracking so
// only the clicked button disables/spins (audit S3: grace actions used to
// disable both buttons globally and fail silently).
const graceKey = (name: string, action: GraceAction) => `${name}:${action}`;

export function ServicesPage() {
  const qc = useQueryClient();
  const toast = useToast();
  const { data, isLoading, isError, error, refetch, isRefetching } = useQuery({
    queryKey: ["services"],
    queryFn: api.listServices,
  });

  const [pending, setPending] = useState<Set<string>>(new Set());

  const control = useMutation({
    mutationFn: ({ name, action }: { name: string; action: GraceAction }) =>
      api.controlGrace(name, action),
    onMutate: ({ name, action }) => {
      setPending((prev) => new Set(prev).add(graceKey(name, action)));
    },
    onSettled: (_data, _err, { name, action }) => {
      setPending((prev) => {
        const next = new Set(prev);
        next.delete(graceKey(name, action));
        return next;
      });
    },
    onSuccess: (_data, { name, action }) => {
      qc.invalidateQueries({ queryKey: ["services"] });
      toast.push({
        tone: "ok",
        title:
          action === "end"
            ? `Grace ended for ${name}`
            : `Grace restarted for ${name}`,
      });
    },
    onError: (err, vars) => {
      toast.push({
        tone: "error",
        title:
          vars.action === "end"
            ? `Couldn't end grace for ${vars.name}`
            : `Couldn't restart grace for ${vars.name}`,
        description: err.message,
        action: { label: "Retry", onClick: () => control.mutate(vars) },
      });
    },
  });

  // Manual service create / rename / delete.
  const [showAdd, setShowAdd] = useState(false);
  const [confirmClear, setConfirmClear] = useState(false);
  const [renaming, setRenaming] = useState<{ from: string; draft: string } | null>(
    null,
  );

  const createService = useMutation({
    mutationFn: (name: string) => api.createService(name),
    onSuccess: (_d, name) => {
      setShowAdd(false);
      qc.invalidateQueries({ queryKey: ["services"] });
      toast.push({ tone: "ok", title: `Service "${name}" created` });
    },
    onError: (err) =>
      toast.push({
        tone: "error",
        title: "Couldn't create service",
        description: err.message,
      }),
  });

  const renameService = useMutation({
    mutationFn: (v: { from: string; to: string }) =>
      api.renameService(v.from, v.to),
    onSuccess: (res, v) => {
      setRenaming(null);
      qc.invalidateQueries({ queryKey: ["services"] });
      qc.invalidateQueries({ queryKey: ["service-overrides"] });
      toast.push({
        tone: "ok",
        title: `Renamed to "${v.to}"`,
        description:
          res.overrides_repointed > 0
            ? `${res.overrides_repointed} override rule(s) repointed`
            : undefined,
      });
    },
    onError: (err) =>
      toast.push({
        tone: "error",
        title: "Couldn't rename service",
        description: err.message,
      }),
  });

  const deleteService = useMutation({
    mutationFn: (name: string) => api.deleteService(name),
    onSuccess: (_d, name) => {
      qc.invalidateQueries({ queryKey: ["services"] });
      toast.push({ tone: "ok", title: `Service "${name}" deleted` });
    },
    onError: (err) =>
      toast.push({
        tone: "error",
        title: "Couldn't delete service",
        // The 409 "override rules target it" message is surfaced verbatim so the
        // operator knows to clear the overrides first.
        description: err.message,
      }),
  });

  // Clear all services — destructive reset of every discovered/manual service.
  // Learned log patterns are left intact (that is a separate action on the Logs
  // page).
  const clearServices = useMutation({
    mutationFn: api.clearServices,
    onSuccess: (res) => {
      qc.invalidateQueries({ queryKey: ["services"] });
      setConfirmClear(false);
      toast.push({
        tone: "ok",
        title: "Discovered services cleared",
        description: `${res.services} services removed — the agent re-discovers services from scratch`,
      });
    },
    onError: (err) =>
      toast.push({
        tone: "error",
        title: "Clear all services failed",
        description: err.message,
        action: { label: "Retry", onClick: () => clearServices.mutate() },
      }),
  });

  const entries = data ? Object.entries(data) : [];
  // Sort A→Z, then paginate at 100/page so a multi-thousand-service estate
  // never renders every row at once (the freeze the founder hit).
  const sorted = entries.sort(([a], [b]) => a.localeCompare(b));
  const pg = usePagination(sorted);

  return (
    <>
      <TopBar
        title="Services"
        subtitle={data ? `${entries.length} discovered` : undefined}
        actions={
          <div className="flex items-center gap-2">
            <button
              className="btn btn-primary"
              onClick={() => setShowAdd(true)}
            >
              <Plus size={12} />
              Add service
            </button>
            <button
              className="btn btn-danger"
              disabled={clearServices.isPending || entries.length === 0}
              onClick={() => setConfirmClear(true)}
            >
              <Trash2 size={12} />
              Clear all services
            </button>
          </div>
        }
      />

      <main className="flex-1 overflow-auto p-6">
        {isError && (
          <div className="mb-3">
            <RetryableError
              error={error}
              onRetry={() => refetch()}
              retrying={isRefetching}
              context="Couldn't load services"
            />
          </div>
        )}

        {(!isError || data) && (
          <div className="card overflow-hidden">
            <div className="max-h-[calc(100vh-240px)] overflow-auto">
              <table className="ddt">
                <thead>
                  <tr>
                    <th>Service</th>
                    <th className="w-48">First seen</th>
                    <th className="w-24">Origin</th>
                    <th className="w-32">Status</th>
                    <th className="w-72">Grace control</th>
                  </tr>
                </thead>
                <tbody>
                  {isLoading && <SkRows rows={6} cols={5} />}
                  {!isLoading && !isError && entries.length === 0 && (
                    <tr>
                      <td colSpan={5}>
                        <EmptyState
                          title="No services discovered yet."
                          hint="Service detection runs on every signal that matches `agent.service_patterns`."
                        />
                      </td>
                    </tr>
                  )}
                  {pg.pageItems.map(([name, info]) => {
                      const isUnknown = name === "_unknown";
                      const endPending = pending.has(graceKey(name, "end"));
                      const restartPending = pending.has(
                        graceKey(name, "restart"),
                      );
                      const isRenaming = renaming?.from === name;
                      return (
                        <tr key={name}>
                          <td className="font-mono">
                            {isRenaming ? (
                              <form
                                className="flex items-center gap-1"
                                onSubmit={(e) => {
                                  e.preventDefault();
                                  const to = renaming.draft.trim();
                                  if (to && to !== name)
                                    renameService.mutate({ from: name, to });
                                  else setRenaming(null);
                                }}
                              >
                                <input
                                  className="input py-1"
                                  autoFocus
                                  value={renaming.draft}
                                  maxLength={256}
                                  aria-label={`New name for ${name}`}
                                  onChange={(e) =>
                                    setRenaming({
                                      from: name,
                                      draft: e.target.value,
                                    })
                                  }
                                />
                                <button
                                  type="submit"
                                  className="btn"
                                  disabled={renameService.isPending}
                                  aria-label="Save rename"
                                  title="Save"
                                >
                                  {renameService.isPending ? (
                                    <Spinner />
                                  ) : (
                                    <Check size={11} />
                                  )}
                                </button>
                                <button
                                  type="button"
                                  className="btn"
                                  aria-label="Cancel rename"
                                  title="Cancel"
                                  onClick={() => setRenaming(null)}
                                >
                                  <X size={11} />
                                </button>
                              </form>
                            ) : isUnknown ? (
                              name
                            ) : (
                              <Link
                                className="link"
                                to={`/agent/services/${encodeURIComponent(name)}`}
                              >
                                {name}
                              </Link>
                            )}
                            {isUnknown && (
                              <Pill tone="warn" className="ml-2">
                                fallback
                              </Pill>
                            )}
                          </td>
                          <td
                            className="text-2xs text-ink-300"
                            title={fmtAbs(info.first_seen)}
                          >
                            {fmtAbs(info.first_seen)}{" "}
                            <span className="text-ink-400">
                              ({fmtRel(info.first_seen)})
                            </span>
                          </td>
                          <td>
                            {/* Origin: whether this service was auto-discovered
                                from a signal or created by hand (X-service-crud).
                                info.manual carries this for every row. */}
                            {info.manual ? (
                              <Pill tone="accent">Manual</Pill>
                            ) : (
                              <Pill>Auto</Pill>
                            )}
                          </td>
                          <td>
                            <Pill tone="good">tracked</Pill>
                          </td>
                          <td>
                            <div className="flex gap-1">
                              <button
                                className="btn"
                                disabled={endPending}
                                aria-label={`End grace period for ${name}`}
                                title="End grace period now"
                                onClick={() =>
                                  control.mutate({ name, action: "end" })
                                }
                              >
                                {endPending ? (
                                  <Spinner />
                                ) : (
                                  <Square size={11} />
                                )}{" "}
                                End
                              </button>
                              <button
                                className="btn"
                                disabled={restartPending}
                                aria-label={`Restart grace timer for ${name}`}
                                title="Restart grace timer (treat as new)"
                                onClick={() =>
                                  control.mutate({ name, action: "restart" })
                                }
                              >
                                {restartPending ? (
                                  <Spinner />
                                ) : (
                                  <Play size={11} />
                                )}{" "}
                                Restart
                              </button>
                              {info.manual && (
                                <>
                                  <button
                                    className="btn"
                                    disabled={isRenaming}
                                    aria-label={`Rename ${name}`}
                                    title="Rename this manual service"
                                    onClick={() =>
                                      setRenaming({ from: name, draft: name })
                                    }
                                  >
                                    <Pencil size={11} /> Rename
                                  </button>
                                  <button
                                    className="btn"
                                    disabled={
                                      deleteService.isPending &&
                                      deleteService.variables === name
                                    }
                                    aria-label={`Delete ${name}`}
                                    title="Delete this manual service"
                                    onClick={() => deleteService.mutate(name)}
                                  >
                                    {deleteService.isPending &&
                                    deleteService.variables === name ? (
                                      <Spinner />
                                    ) : (
                                      <Trash2 size={11} />
                                    )}{" "}
                                    Delete
                                  </button>
                                </>
                              )}
                            </div>
                          </td>
                        </tr>
                      );
                    })}
                </tbody>
              </table>
            </div>
            <Pagination state={pg} />
          </div>
        )}

        <p className="mt-3 text-2xs text-ink-400">
          Grace is controlled by{" "}
          <code className="rounded bg-ink-700 px-1 text-ink-200">
            agent.new_service_grace
          </code>
          . During the grace window, signals from a service are learned but not
          surfaced as shadow / detect events. To re-point a mis-attributed
          signal to a service, use the “Assign to service” action on the Logs,
          Metrics or Traces page.
        </p>
      </main>

      {showAdd && (
        <TextInputModal
          title="Add service"
          label="Service name"
          placeholder="New service name (e.g. checkout-api)"
          confirmLabel="Add service"
          maxLength={256}
          busy={createService.isPending}
          error={createService.error}
          onSubmit={(name) => createService.mutate(name)}
          onClose={() => setShowAdd(false)}
        />
      )}

      {confirmClear && (
        <ConfirmDialog
          title="Clear all discovered services"
          message="This removes ALL discovered and manually-created services, so the agent re-discovers services from scratch on the next tick. Learned log patterns are left untouched. This cannot be undone."
          confirmLabel="Clear all services"
          tone="danger"
          busy={clearServices.isPending}
          error={clearServices.error}
          onConfirm={() => clearServices.mutate()}
          onClose={() => setConfirmClear(false)}
        />
      )}
    </>
  );
}
