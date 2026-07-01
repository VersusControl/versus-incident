import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "react-router-dom";
import { Check, Pencil, Play, Plus, Square, Trash2, X } from "lucide-react";
import { api, type ServiceOverrideSource } from "@/lib/api";
import { fmtAbs, fmtRel } from "@/lib/format";
import { TopBar } from "@/components/TopBar";
import { Pill } from "@/components/Pill";
import { EmptyState, Spinner } from "@/components/feedback";
import { RetryableError } from "@/components/RetryableError";
import { SkRows } from "@/components/Skeleton";
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

  // Override rules (manual attribution corrections) + the enterprise probe that
  // decides whether metric/trace source types are offered. listBaselines 403s /
  // 404s on a community binary — treat any failure as "not licensed", so the
  // metric/trace override options stay hidden exactly like the ServiceDetail
  // Metrics & Traces card degrades.
  const overrides = useQuery({
    queryKey: ["service-overrides"],
    queryFn: api.listServiceOverrides,
  });
  const intelProbe = useQuery({
    queryKey: ["service-overrides-intel-probe"],
    queryFn: () => api.listBaselines(),
    retry: false,
  });
  const metricsLicensed = intelProbe.isSuccess;

  // Manual service create / rename / delete.
  const [newName, setNewName] = useState("");
  const [renaming, setRenaming] = useState<{ from: string; draft: string } | null>(
    null,
  );

  // Manual-attribution override create form.
  const [ovSource, setOvSource] = useState<ServiceOverrideSource>("log");
  const [ovMatch, setOvMatch] = useState("");
  const [ovService, setOvService] = useState("");

  const createService = useMutation({
    mutationFn: (name: string) => api.createService(name),
    onSuccess: (_d, name) => {
      setNewName("");
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

  const deleteOverride = useMutation({
    mutationFn: (id: string) => api.deleteServiceOverride(id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["service-overrides"] });
      toast.push({ tone: "ok", title: "Override removed" });
    },
    onError: (err) =>
      toast.push({
        tone: "error",
        title: "Couldn't remove override",
        description: err.message,
      }),
  });

  const createOverride = useMutation({
    mutationFn: (input: {
      source_type: ServiceOverrideSource;
      match: string;
      service: string;
    }) => api.createServiceOverride(input),
    onSuccess: () => {
      setOvMatch("");
      qc.invalidateQueries({ queryKey: ["service-overrides"] });
      toast.push({ tone: "ok", title: "Override created" });
    },
    onError: (err) =>
      toast.push({
        tone: "error",
        // The 400 "unknown target service; create it first" message is surfaced
        // verbatim so the operator knows to add the service above first.
        title: "Couldn't create override",
        description: err.message,
      }),
  });

  const entries = data ? Object.entries(data) : [];

  // Override targets: every known service except the _unknown fallback.
  const serviceNames = entries
    .map(([name]) => name)
    .filter((n) => n !== "_unknown")
    .sort((a, b) => a.localeCompare(b));

  return (
    <>
      <TopBar
        title="Services"
        subtitle={data ? `${entries.length} discovered` : undefined}
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
            {/* Create a manual service so it is selectable as an override
                target before any signal is attributed to it (X-service-crud). */}
            <form
              className="flex flex-wrap items-center gap-2 border-b border-ink-700 p-3"
              onSubmit={(e) => {
                e.preventDefault();
                const name = newName.trim();
                if (name) createService.mutate(name);
              }}
            >
              <input
                className="input min-w-[16rem] flex-1"
                placeholder="New service name (e.g. checkout-api)"
                value={newName}
                maxLength={256}
                onChange={(e) => setNewName(e.target.value)}
                aria-label="New service name"
              />
              <button
                type="submit"
                className="btn btn-primary"
                disabled={!newName.trim() || createService.isPending}
              >
                {createService.isPending ? <Spinner /> : <Plus size={12} />} Add
                service
              </button>
            </form>
            <div className="max-h-[calc(100vh-240px)] overflow-auto">
              <table className="ddt">
                <thead>
                  <tr>
                    <th>Service</th>
                    <th className="w-48">First seen</th>
                    <th className="w-32">Status</th>
                    <th className="w-72">Grace control</th>
                  </tr>
                </thead>
                <tbody>
                  {isLoading && <SkRows rows={6} cols={4} />}
                  {!isLoading && !isError && entries.length === 0 && (
                    <tr>
                      <td colSpan={4}>
                        <EmptyState
                          title="No services discovered yet."
                          hint="Service detection runs on every signal that matches `agent.service_patterns`."
                        />
                      </td>
                    </tr>
                  )}
                  {entries
                    .sort(([a], [b]) => a.localeCompare(b))
                    .map(([name, info]) => {
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
                            {info.manual && !isRenaming && (
                              <Pill className="ml-2">manual</Pill>
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
          </div>
        )}

        {/* Manual-attribution overrides. When the regex service_patterns
            mis-attributes a signal (wrong service, a log level, _unknown), an
            override re-points it after detection. Log overrides work on any
            binary; metric/trace overrides only apply where the Enterprise
            metric/trace brains run, so those source types are offered only when
            the /intel probe is licensed. */}
        <div className="card mt-4 overflow-hidden">
          <div className="border-b border-ink-700 p-3">
            <h2 className="text-sm font-semibold text-ink-50">
              Attribution overrides
            </h2>
            <p className="mt-0.5 text-2xs text-ink-400">
              Re-point a mis-detected signal to the correct service. Matches are
              applied after auto-detection, per signal source.
            </p>
          </div>

          <form
            className="flex flex-wrap items-end gap-2 border-b border-ink-700 p-3"
            onSubmit={(e) => {
              e.preventDefault();
              const match = ovMatch.trim();
              if (match && ovService)
                createOverride.mutate({
                  source_type: ovSource,
                  match,
                  service: ovService,
                });
            }}
          >
            <label className="flex flex-col gap-1">
              <span className="text-2xs uppercase tracking-wide text-ink-400">
                Source
              </span>
              <select
                className="input py-1"
                value={ovSource}
                aria-label="Override source type"
                onChange={(e) =>
                  setOvSource(e.target.value as ServiceOverrideSource)
                }
              >
                <option value="log">Log</option>
                {metricsLicensed && <option value="metric">Metric</option>}
                {metricsLicensed && <option value="trace">Trace</option>}
              </select>
            </label>
            <label className="flex min-w-[16rem] flex-1 flex-col gap-1">
              <span className="text-2xs uppercase tracking-wide text-ink-400">
                Match
              </span>
              <input
                className="input py-1"
                value={ovMatch}
                aria-label="Override match"
                placeholder={
                  ovSource === "log"
                    ? "pattern id or message substring"
                    : "signal name (supports * and ? globs)"
                }
                onChange={(e) => setOvMatch(e.target.value)}
              />
            </label>
            <label className="flex flex-col gap-1">
              <span className="text-2xs uppercase tracking-wide text-ink-400">
                Assign to
              </span>
              <select
                className="input py-1 min-w-[12rem]"
                value={ovService}
                aria-label="Override target service"
                onChange={(e) => setOvService(e.target.value)}
              >
                <option value="">Select a service…</option>
                {serviceNames.map((n) => (
                  <option key={n} value={n}>
                    {n}
                  </option>
                ))}
              </select>
            </label>
            <button
              type="submit"
              className="btn btn-primary"
              disabled={
                !ovMatch.trim() || !ovService || createOverride.isPending
              }
            >
              {createOverride.isPending ? <Spinner /> : <Plus size={12} />} Add
              override
            </button>
          </form>

          <table className="ddt">
            <thead>
              <tr>
                <th className="w-24">Source</th>
                <th>Match</th>
                <th className="w-48">Assigned to</th>
                <th className="w-44">Created</th>
                <th className="w-16" />
              </tr>
            </thead>
            <tbody>
              {overrides.isLoading && <SkRows rows={3} cols={5} />}
              {overrides.isError && (
                <tr>
                  <td colSpan={5}>
                    <RetryableError
                      error={overrides.error}
                      onRetry={() => overrides.refetch()}
                      retrying={overrides.isRefetching}
                      context="Couldn't load overrides"
                    />
                  </td>
                </tr>
              )}
              {overrides.isSuccess && overrides.data.length === 0 && (
                <tr>
                  <td colSpan={5}>
                    <EmptyState
                      title="No overrides yet."
                      hint="Add one above to correct a mis-attributed signal."
                    />
                  </td>
                </tr>
              )}
              {overrides.data?.map((o) => (
                <tr key={o.id}>
                  <td>
                    <Pill
                      tone={o.source_type === "log" ? "default" : "accent"}
                    >
                      {o.source_type}
                    </Pill>
                  </td>
                  <td
                    className="font-mono text-2xs text-ink-100"
                    title={o.match}
                  >
                    {o.match}
                  </td>
                  <td className="font-mono text-2xs text-ink-100">
                    {o.service}
                  </td>
                  <td
                    className="text-2xs text-ink-300"
                    title={fmtAbs(o.created_at)}
                  >
                    {fmtRel(o.created_at)}
                  </td>
                  <td>
                    <button
                      className="btn"
                      disabled={
                        deleteOverride.isPending &&
                        deleteOverride.variables === o.id
                      }
                      aria-label={`Delete override ${o.match}`}
                      title="Delete override"
                      onClick={() => deleteOverride.mutate(o.id)}
                    >
                      {deleteOverride.isPending &&
                      deleteOverride.variables === o.id ? (
                        <Spinner />
                      ) : (
                        <Trash2 size={11} />
                      )}
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>

        <p className="mt-3 text-2xs text-ink-400">
          Grace is controlled by{" "}
          <code className="rounded bg-ink-700 px-1 text-ink-200">
            agent.new_service_grace
          </code>
          . During the grace window, signals from a service are learned but not
          surfaced as shadow / detect events.
        </p>
      </main>
    </>
  );
}
