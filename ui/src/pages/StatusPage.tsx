import { useQuery } from "@tanstack/react-query";
import { CircleDot, Power, PowerOff } from "lucide-react";
import { api } from "@/lib/api";
import { TopBar } from "@/components/TopBar";
import { Pill } from "@/components/Pill";
import { ErrorBox, Spinner } from "@/components/feedback";

type Tone = "good" | "warn" | "bad" | "accent" | "neutral";
type PillTone = "good" | "warn" | "bad" | "accent" | undefined;

function modeTone(mode: string): PillTone {
  switch (mode) {
    case "detect":
      return "bad"; // emitting incidents — loudest signal
    case "shadow":
      return "warn";
    case "training":
      return "accent";
    default:
      return undefined;
  }
}

// Status dashboard — Datadog-ish overview of the AI agent runtime:
// runtime banner + tile row + per-subsystem breakdown tables.
export function StatusPage() {
  const status = useQuery({ queryKey: ["status"], queryFn: api.status });
  const shadow = useQuery({
    queryKey: ["shadow-stats"],
    queryFn: api.shadowStats,
    retry: 0,
  });
  const detect = useQuery({
    queryKey: ["detect-stats"],
    queryFn: api.detectStats,
    retry: 0,
  });
  const services = useQuery({
    queryKey: ["services"],
    queryFn: api.listServices,
  });
  const agentCfg = useQuery({
    queryKey: ["agent-config"],
    queryFn: api.getAgentConfig,
    retry: 0,
    refetchInterval: false,
  });

  const mode = agentCfg.data?.mode ?? "";
  const enabledSources =
    agentCfg.data?.sources?.filter((s) => s.enable) ?? [];
  const aiEnabled = !!agentCfg.data?.ai?.enable;

  // Flatten DetectStats: keys like outcome_emitted, verdict_spike, severity_high.
  const detectGroup = (prefix: string): Array<[string, number]> => {
    if (!detect.data) return [];
    return Object.entries(detect.data)
      .filter(([k]) => k.startsWith(prefix))
      .map(([k, v]) => [k.slice(prefix.length), v as number] as [string, number])
      .sort((a, b) => b[1] - a[1]);
  };
  const outcomes = detectGroup("outcome_");
  const detectVerdicts = detectGroup("verdict_");
  const severities = detectGroup("severity_");

  // outcome_emitted is the "real" incident count emitted by detect mode.
  const emitted = detect.data?.["outcome_emitted"] ?? 0;
  const cached = detect.data?.["outcome_cached"] ?? 0;
  const aiErrors =
    (detect.data?.["outcome_ai_error"] ?? 0) +
    (detect.data?.["outcome_send_error"] ?? 0);

  return (
    <>
      <TopBar
        title="Status"
        subtitle="Live overview of the AI agent runtime."
      />

      <main className="flex-1 overflow-auto p-6">
        {status.isError && <ErrorBox error={status.error} />}

        {/* Runtime banner */}
        <div className="card mb-4">
          <div className="card-body flex flex-wrap items-center gap-4 text-xs">
            <div className="flex items-center gap-2">
              {agentCfg.data?.enable ? (
                <Power size={14} className="text-good" />
              ) : (
                <PowerOff size={14} className="text-ink-400" />
              )}
              <span className="font-medium text-ink-800">Agent</span>
              <Pill tone={agentCfg.data?.enable ? "good" : undefined}>
                {agentCfg.data?.enable ? "enabled" : "disabled"}
              </Pill>
            </div>
            {mode && (
              <div className="flex items-center gap-2">
                <CircleDot size={14} className="text-ink-400" />
                <span className="text-ink-500">Mode</span>
                <Pill tone={modeTone(mode)}>{mode}</Pill>
              </div>
            )}
            <div className="flex items-center gap-2">
              <span className="text-ink-500">Sources</span>
              <span className="font-mono text-ink-800">
                {agentCfg.isLoading
                  ? "…"
                  : `${enabledSources.length}/${
                      agentCfg.data?.sources?.length ?? 0
                    } enabled`}
              </span>
            </div>
            <div className="flex items-center gap-2">
              <span className="text-ink-500">AI SRE</span>
              <Pill tone={aiEnabled ? "accent" : undefined}>
                {aiEnabled ? agentCfg.data?.ai?.model || "on" : "off"}
              </Pill>
            </div>
            {agentCfg.data?.poll_interval && (
              <div className="flex items-center gap-2 text-ink-500">
                Poll every{" "}
                <span className="font-mono text-ink-800">
                  {agentCfg.data.poll_interval}
                </span>
              </div>
            )}
          </div>
        </div>

        {/* Top tile row — agent-wide counters */}
        <div className="grid grid-cols-1 gap-3 sm:grid-cols-2 lg:grid-cols-4">
          <Tile
            label="Patterns learned"
            value={status.data?.patterns}
            loading={status.isLoading}
            foot={status.data?.dirty ? "Unsaved changes" : "Persisted"}
          />
          <Tile
            label="Services tracked"
            value={
              services.data ? Object.keys(services.data).length : undefined
            }
            loading={services.isLoading}
            foot="Discovered from logs"
          />
          <Tile
            label="Shadow events"
            value={status.data?.shadow_events ?? shadow.data?.events ?? 0}
            loading={status.isLoading}
            foot={
              status.data?.shadow_dirty ? "Unsaved changes" : "Disk in sync"
            }
          />
          <Tile
            label="Total signals (shadow)"
            value={shadow.data?.total_signals}
            loading={shadow.isLoading}
            foot="Across every shadow tick"
          />
        </div>

        {/* Detect tile row — AI SRE counters */}
        <div className="mt-3 grid grid-cols-1 gap-3 sm:grid-cols-2 lg:grid-cols-4">
          <Tile
            label="Detect events"
            value={status.data?.detect_events ?? detect.data?.["events"] ?? 0}
            loading={detect.isLoading}
            foot={
              status.data?.detect_dirty ? "Unsaved changes" : "Disk in sync"
            }
          />
          <Tile
            label="Incidents emitted"
            value={emitted}
            loading={detect.isLoading}
            tone="bad"
            foot="From detect mode"
          />
          <Tile
            label="AI cache hits"
            value={cached}
            loading={detect.isLoading}
            tone="good"
            foot="No model call needed"
          />
          <Tile
            label="AI / send errors"
            value={aiErrors}
            loading={detect.isLoading}
            tone={aiErrors > 0 ? "warn" : undefined}
            foot="Failed analyses + sends"
          />
        </div>

        {/* Breakdown cards */}
        <section className="mt-6 grid grid-cols-1 gap-6 lg:grid-cols-2">
          <BreakdownCard
            title="Verdict breakdown (shadow)"
            loading={shadow.isLoading}
            error={shadow.isError ? shadow.error : null}
            rows={Object.entries(shadow.data?.verdicts || {}).sort(
              (a, b) => b[1] - a[1],
            )}
            emptyText="No verdicts recorded yet."
          />

          <BreakdownCard
            title="Detect outcomes"
            loading={detect.isLoading}
            error={detect.isError ? detect.error : null}
            rows={outcomes}
            emptyText="No detect-mode calls yet."
          />

          <BreakdownCard
            title="Detect verdicts"
            loading={detect.isLoading}
            error={detect.isError ? detect.error : null}
            rows={detectVerdicts}
            emptyText="No detect-mode calls yet."
          />

          <BreakdownCard
            title="AI severity"
            loading={detect.isLoading}
            error={detect.isError ? detect.error : null}
            rows={severities}
            emptyText="No findings parsed yet."
          />
        </section>

        {/* Enabled sources */}
        <section className="mt-6 grid grid-cols-1 gap-6 lg:grid-cols-2">
          <div className="card">
            <div className="card-header">
              <h2 className="card-title">Signal sources</h2>
              <span className="text-2xs text-ink-400">
                {agentCfg.data?.sources_path || "agent_sources.yaml"}
              </span>
            </div>
            <div className="card-body">
              {agentCfg.isLoading && <Spinner />}
              {agentCfg.isError && <ErrorBox error={agentCfg.error} />}
              {agentCfg.data && agentCfg.data.sources.length === 0 && (
                <p className="text-xs text-ink-400">No sources configured.</p>
              )}
              {agentCfg.data && agentCfg.data.sources.length > 0 && (
                <table className="ddt">
                  <thead>
                    <tr>
                      <th>Name</th>
                      <th>Type</th>
                      <th className="w-20 text-right">State</th>
                    </tr>
                  </thead>
                  <tbody>
                    {agentCfg.data.sources.map((s) => (
                      <tr key={s.name}>
                        <td className="font-mono">{s.name}</td>
                        <td className="font-mono text-ink-600">{s.type}</td>
                        <td className="text-right">
                          {s.enable ? (
                            <Pill tone="good">on</Pill>
                          ) : (
                            <Pill>off</Pill>
                          )}
                        </td>
                      </tr>
                    ))}
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
                <strong className="text-ink-800">Mode</strong> —{" "}
                <code>training</code> observes only,{" "}
                <code>shadow</code> classifies and logs would-have-alerted
                events, <code>detect</code> calls the AI SRE and emits real
                incidents.
              </p>
              <p>
                <strong className="text-ink-800">Patterns learned</strong>{" "}
                — every distinct log template the Drain-style miner has
                clustered. Grows in training, plateaus in shadow/detect.
              </p>
              <p>
                <strong className="text-ink-800">Detect outcomes</strong>{" "}
                — <code>emitted</code> sent an incident,{" "}
                <code>cached</code> reused a prior AI finding,{" "}
                <code>dry</code> means no AI configured,{" "}
                <code>quota</code> hit the hourly cap,{" "}
                <code>ai_error</code> / <code>send_error</code> failed
                analyses and channel sends.
              </p>
              <p>
                <strong className="text-ink-800">Verdicts</strong> —{" "}
                <code>spike</code> means a known pattern blew past its EWMA
                baseline; <code>unknown</code> means a pattern has not yet
                been auto-promoted or marked known by an operator.
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
  tone,
}: {
  label: string;
  value: number | undefined;
  foot?: string;
  loading?: boolean;
  tone?: Tone;
}) {
  const valueTone =
    tone === "good"
      ? "text-good"
      : tone === "warn"
      ? "text-warn"
      : tone === "bad"
      ? "text-bad"
      : tone === "accent"
      ? "text-accent"
      : "";
  return (
    <div className="stat-card">
      <div className="stat-label">{label}</div>
      <div className={`stat-value tabular-nums ${valueTone}`}>
        {loading ? <Spinner /> : (value ?? "—")}
      </div>
      {foot && <div className="stat-foot">{foot}</div>}
    </div>
  );
}

function BreakdownCard({
  title,
  rows,
  loading,
  error,
  emptyText,
}: {
  title: string;
  rows: Array<[string, number]>;
  loading?: boolean;
  error?: unknown;
  emptyText: string;
}) {
  return (
    <div className="card">
      <div className="card-header">
        <h2 className="card-title">{title}</h2>
      </div>
      <div className="card-body">
        {loading && <Spinner />}
        {error ? <ErrorBox error={error} /> : null}
        {!loading && !error && (
          <table className="ddt">
            <thead>
              <tr>
                <th>Key</th>
                <th className="w-24 text-right">Count</th>
              </tr>
            </thead>
            <tbody>
              {rows.length === 0 ? (
                <tr>
                  <td colSpan={2} className="text-ink-400">
                    {emptyText}
                  </td>
                </tr>
              ) : (
                rows.map(([k, n]) => (
                  <tr key={k}>
                    <td className="font-mono">{k}</td>
                    <td className="text-right tabular-nums">{n}</td>
                  </tr>
                ))
              )}
            </tbody>
          </table>
        )}
      </div>
    </div>
  );
}
