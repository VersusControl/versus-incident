import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Play, Square } from "lucide-react";
import { api } from "@/lib/api";
import { fmtAbs, fmtRel } from "@/lib/format";
import { TopBar } from "@/components/TopBar";
import { Pill } from "@/components/Pill";
import { EmptyState, Spinner } from "@/components/feedback";
import { RetryableError } from "@/components/RetryableError";
import { SkRows } from "@/components/Skeleton";
import { useToast } from "@/components/Toast";

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

  const entries = data ? Object.entries(data) : [];

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
            <div className="max-h-[calc(100vh-180px)] overflow-auto">
              <table className="ddt">
                <thead>
                  <tr>
                    <th>Service</th>
                    <th className="w-48">First seen</th>
                    <th className="w-32">Status</th>
                    <th className="w-56">Grace control</th>
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
                      return (
                        <tr key={name}>
                          <td className="font-mono">
                            {name}
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
