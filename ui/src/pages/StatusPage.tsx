import { useQuery } from "@tanstack/react-query";
import { api } from "@/lib/api";
import { TopBar } from "@/components/TopBar";
import { ErrorBox, Spinner } from "@/components/feedback";

// Status dashboard — Datadog-ish "metric tiles" laid out across the top,
// followed by a verdict breakdown table for the shadow log.
export function StatusPage() {
  const status = useQuery({ queryKey: ["status"], queryFn: api.status });
  const shadow = useQuery({
    queryKey: ["shadow-stats"],
    queryFn: api.shadowStats,
    retry: 0,
  });
  const services = useQuery({
    queryKey: ["services"],
    queryFn: api.listServices,
  });

  return (
    <>
      <TopBar
        title="Status"
        subtitle="Live overview of the AI agent runtime."
      />

      <main className="flex-1 overflow-auto p-6">
        {status.isError && <ErrorBox error={status.error} />}

        <div className="grid grid-cols-1 gap-3 sm:grid-cols-2 lg:grid-cols-4">
          <Tile
            label="Patterns learned"
            value={status.data?.patterns}
            loading={status.isLoading}
            foot={status.data?.dirty ? "Unsaved changes" : "Persisted"}
          />
          <Tile
            label="Shadow events"
            value={status.data?.shadow_events ?? shadow.data?.events ?? 0}
            loading={status.isLoading}
            foot={
              status.data?.shadow_dirty
                ? "Unsaved changes"
                : "Disk in sync"
            }
          />
          <Tile
            label="Total signals (shadow)"
            value={shadow.data?.total_signals}
            loading={shadow.isLoading}
            foot="Across every shadow tick"
          />
          <Tile
            label="Services tracked"
            value={
              services.data ? Object.keys(services.data).length : undefined
            }
            loading={services.isLoading}
            foot="Discovered from logs"
          />
        </div>

        <section className="mt-6 grid grid-cols-1 gap-6 lg:grid-cols-2">
          <div className="card">
            <div className="card-header">
              <h2 className="card-title">Verdict breakdown (shadow)</h2>
            </div>
            <div className="card-body">
              {shadow.isLoading && <Spinner />}
              {shadow.isError && <ErrorBox error={shadow.error} />}
              {shadow.data && (
                <table className="ddt">
                  <thead>
                    <tr>
                      <th>Verdict</th>
                      <th className="w-24 text-right">Count</th>
                    </tr>
                  </thead>
                  <tbody>
                    {Object.entries(shadow.data.verdicts || {}).length === 0 ? (
                      <tr>
                        <td colSpan={2} className="text-ink-400">
                          No verdicts recorded yet.
                        </td>
                      </tr>
                    ) : (
                      Object.entries(shadow.data.verdicts).map(([v, n]) => (
                        <tr key={v}>
                          <td className="font-mono">{v}</td>
                          <td className="text-right tabular-nums">{n}</td>
                        </tr>
                      ))
                    )}
                  </tbody>
                </table>
              )}
            </div>
          </div>

          <div className="card">
            <div className="card-header">
              <h2 className="card-title">How to read this</h2>
            </div>
            <div className="card-body space-y-2 text-xs leading-relaxed text-ink-600">
              <p>
                <strong className="text-ink-800">Patterns learned</strong>{" "}
                — every distinct log template the agent has clustered. Grows
                in training, plateaus in shadow/detect.
              </p>
              <p>
                <strong className="text-ink-800">Shadow events</strong>{" "}
                — would-have-alerted entries written to{" "}
                <code className="rounded bg-ink-50 px-1">data/shadow.json</code>
                . Visible only in shadow mode.
              </p>
              <p>
                <strong className="text-ink-800">Verdicts</strong>{" "}
                — <code>spike</code> means a known pattern blew past its
                EWMA baseline; <code>unknown</code> means a pattern has not
                yet been auto-promoted or marked known by an operator.
              </p>
            </div>
          </div>
        </section>
      </main>
    </>
  );
}

function Tile({
  label,
  value,
  foot,
  loading,
}: {
  label: string;
  value: number | undefined;
  foot?: string;
  loading?: boolean;
}) {
  return (
    <div className="stat-card">
      <div className="stat-label">{label}</div>
      <div className="stat-value tabular-nums">
        {loading ? <Spinner /> : (value ?? "—")}
      </div>
      {foot && <div className="stat-foot">{foot}</div>}
    </div>
  );
}
