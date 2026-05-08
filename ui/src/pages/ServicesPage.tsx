import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Play, Square } from "lucide-react";
import { api } from "@/lib/api";
import { fmtAbs, fmtRel } from "@/lib/format";
import { TopBar } from "@/components/TopBar";
import { Pill } from "@/components/Pill";
import { EmptyState, ErrorBox, Spinner } from "@/components/feedback";

export function ServicesPage() {
  const qc = useQueryClient();
  const { data, isLoading, isError, error } = useQuery({
    queryKey: ["services"],
    queryFn: api.listServices,
  });

  const control = useMutation({
    mutationFn: ({
      name,
      action,
    }: {
      name: string;
      action: "end" | "restart";
    }) => api.controlGrace(name, action),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["services"] }),
  });

  const entries = data ? Object.entries(data) : [];

  return (
    <>
      <TopBar
        title="Services"
        subtitle={data ? `${entries.length} discovered` : undefined}
      />

      <main className="flex-1 overflow-auto p-6">
        {isError && <ErrorBox error={error} />}

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
                {isLoading && (
                  <tr>
                    <td colSpan={4}>
                      <Spinner />
                    </td>
                  </tr>
                )}
                {!isLoading && entries.length === 0 && (
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
                        <td className="text-2xs text-ink-600">
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
                              disabled={control.isPending}
                              onClick={() =>
                                control.mutate({ name, action: "end" })
                              }
                              title="End grace period now"
                            >
                              <Square size={11} /> End
                            </button>
                            <button
                              className="btn"
                              disabled={control.isPending}
                              onClick={() =>
                                control.mutate({ name, action: "restart" })
                              }
                              title="Restart grace timer (treat as new)"
                            >
                              <Play size={11} /> Restart
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

        <p className="mt-3 text-2xs text-ink-400">
          Grace is controlled by{" "}
          <code className="rounded bg-ink-100 px-1">agent.new_service_grace</code>.
          During the grace window, signals from a service are learned but not
          surfaced as shadow / detect events.
        </p>
      </main>
    </>
  );
}
