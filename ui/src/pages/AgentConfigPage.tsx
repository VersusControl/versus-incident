import { useQuery } from "@tanstack/react-query";
import { Lock, ExternalLink } from "lucide-react";
import { TopBar } from "@/components/TopBar";
import { ErrorBox, Spinner } from "@/components/feedback";
import { api } from "@/lib/api";

// Read-only view of the AI agent configuration. Like the incidents
// config page, secrets (AI API key, source credentials) are reduced to
// a simple "configured / not set" pill.
export function AgentConfigPage() {
  const cfg = useQuery({
    queryKey: ["config-agent"],
    queryFn: api.getAgentConfig,
  });

  return (
    <>
      <TopBar
        title="AI Agent · Configuration"
        subtitle="Read-only view of the running agent config."
      />
      <main className="flex-1 overflow-auto p-6">
        {cfg.isLoading && <Spinner />}
        {cfg.isError && <ErrorBox error={cfg.error} />}
        {cfg.data && (
          <div className="space-y-6">
            <SecretBanner />

            <div className="card">
              <div className="card-header">
                <h2 className="card-title">Runtime</h2>
                <EnablePill enabled={cfg.data.enable} />
              </div>
              <div className="card-body">
                <Grid>
                  <KV k="Mode" v={cfg.data.mode || "—"} />
                  <KV k="Poll interval" v={cfg.data.poll_interval || "—"} />
                  <KV k="Lookback" v={cfg.data.lookback || "—"} />
                  <KV k="Batch max" v={String(cfg.data.batch_max)} />
                  <KV
                    k="Signal max bytes"
                    v={String(cfg.data.signal_max_bytes)}
                  />
                  <KV
                    k="New service grace"
                    v={cfg.data.new_service_grace || "0 (disabled)"}
                  />
                  <KV
                    k="Sources file"
                    v={cfg.data.sources_path || "(inline)"}
                  />
                </Grid>
              </div>
            </div>

            <div className="card">
              <div className="card-header">
                <h2 className="card-title">Sources</h2>
                <span className="text-xs text-ink-400">
                  {cfg.data.sources.length} configured
                </span>
              </div>
              <div className="card-body">
                {cfg.data.sources.length === 0 ? (
                  <div className="text-xs text-ink-400">
                    No sources defined. See{" "}
                    <code>agent.sources_path</code> or the inline{" "}
                    <code>agent.sources</code> list.
                  </div>
                ) : (
                  <div className="space-y-3">
                    {cfg.data.sources.map((s) => (
                      <SubCard
                        key={s.name}
                        title={
                          <div className="flex items-center gap-2">
                            <span className="font-mono text-xs text-ink-800">
                              {s.name}
                            </span>
                            <span className="pill">{s.type}</span>
                            <EnablePill enabled={s.enable} />
                          </div>
                        }
                      >
                        {renderDetailsKV(s.details)}
                      </SubCard>
                    ))}
                  </div>
                )}
              </div>
            </div>

            <div className="card">
              <div className="card-header">
                <h2 className="card-title">AI analyzer</h2>
                <EnablePill enabled={cfg.data.ai.enable} />
              </div>
              <div className="card-body">
                <Grid>
                  <KV k="Model" v={cfg.data.ai.model || "—"} />
                  <SecretField
                    k="API key"
                    configured={cfg.data.ai.api_key === "set"}
                  />
                  <KV
                    k="Temperature"
                    v={String(cfg.data.ai.temperature)}
                  />
                  <KV k="Max tokens" v={String(cfg.data.ai.max_tokens)} />
                  <KV
                    k="Max calls/hour"
                    v={String(cfg.data.ai.max_calls_per_hour)}
                  />
                  <KV k="Cache TTL" v={cfg.data.ai.cache_ttl || "—"} />
                </Grid>
              </div>
            </div>

            <div className="card">
              <div className="card-header">
                <h2 className="card-title">Catalog & Miner</h2>
              </div>
              <div className="card-body space-y-3">
                <SubCard title="Catalog">
                  <Grid>
                    <KV
                      k="Persist interval"
                      v={cfg.data.catalog.persist_interval || "—"}
                    />
                    <KV
                      k="Auto promote after"
                      v={String(cfg.data.catalog.auto_promote_after)}
                    />
                    <KV
                      k="Spike multiplier"
                      v={String(cfg.data.catalog.spike_multiplier)}
                    />
                    <KV
                      k="Spike min frequency"
                      v={String(cfg.data.catalog.spike_min_frequency)}
                    />
                    <KV
                      k="Spike min baseline"
                      v={String(cfg.data.catalog.spike_min_baseline_count)}
                    />
                  </Grid>
                </SubCard>
                <SubCard title="Miner">
                  <Grid>
                    <KV
                      k="Similarity threshold"
                      v={String(cfg.data.miner.similarity_threshold)}
                    />
                    <KV k="Tree depth" v={String(cfg.data.miner.tree_depth)} />
                    <KV
                      k="Max children"
                      v={String(cfg.data.miner.max_children)}
                    />
                  </Grid>
                </SubCard>
              </div>
            </div>

            <div className="card">
              <div className="card-header">
                <h2 className="card-title">Redaction</h2>
                <EnablePill enabled={cfg.data.redaction.enable} />
              </div>
              <div className="card-body">
                <Grid>
                  <KV
                    k="Redact IPs"
                    v={String(cfg.data.redaction.redact_ips)}
                  />
                  <KV
                    k="Extra patterns"
                    v={String(cfg.data.redaction.extra_pattern_count)}
                  />
                </Grid>
              </div>
            </div>

            <div className="card">
              <div className="card-header">
                <h2 className="card-title">Regex pre-filter</h2>
                <span className="text-xs text-ink-400">
                  {cfg.data.regex.rules.length} rule(s)
                </span>
              </div>
              <div className="card-body">
                <div className="mb-3">
                  <KV
                    k="Default pattern"
                    v={cfg.data.regex.default_pattern || "(none — strict mode)"}
                  />
                </div>
                {cfg.data.regex.rules.length > 0 && (
                  <table className="ddt">
                    <thead>
                      <tr>
                        <th>Name</th>
                        <th>Pattern</th>
                      </tr>
                    </thead>
                    <tbody>
                      {cfg.data.regex.rules.map((r) => (
                        <tr key={r.name}>
                          <td className="font-mono text-xs">{r.name}</td>
                          <td className="font-mono text-xs text-ink-600">
                            {r.pattern}
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
                <h2 className="card-title">Service patterns</h2>
                <span className="text-xs text-ink-400">
                  {cfg.data.service_patterns.length} configured
                </span>
              </div>
              <div className="card-body">
                {cfg.data.service_patterns.length === 0 ? (
                  <div className="text-xs text-ink-400">
                    Not configured — service detection is OFF and signals are
                    attributed to <code>_unknown</code>.
                  </div>
                ) : (
                  <ul className="space-y-1">
                    {cfg.data.service_patterns.map((p, i) => (
                      <li
                        key={i}
                        className="rounded bg-ink-50 px-2 py-1 font-mono text-xs text-ink-700"
                      >
                        {p}
                      </li>
                    ))}
                  </ul>
                )}
              </div>
            </div>
          </div>
        )}
      </main>
    </>
  );
}

function renderDetailsKV(d?: Record<string, unknown>): React.ReactNode {
  if (!d) {
    return <div className="text-xs text-ink-400">No details.</div>;
  }
  const entries = Object.entries(d).filter(
    ([, v]) => v !== "" && v !== 0 && v != null && v !== false,
  );
  if (entries.length === 0) {
    return <div className="text-xs text-ink-400">No details.</div>;
  }
  return (
    <Grid>
      {entries.map(([k, v]) => (
        <KV key={k} k={prettyKey(k)} v={formatDetailValue(v)} />
      ))}
    </Grid>
  );
}

function prettyKey(k: string): string {
  return k.replace(/_/g, " ");
}

function formatDetailValue(v: unknown): string {
  if (Array.isArray(v)) return v.map(String).join(", ");
  if (typeof v === "object" && v !== null) return JSON.stringify(v);
  return String(v);
}

function KV({ k, v }: { k: string; v: string }) {
  return (
    <div>
      <div className="text-2xs uppercase tracking-wider text-ink-400">{k}</div>
      <div className="mt-0.5 break-words font-mono text-xs text-ink-800">
        {v}
      </div>
    </div>
  );
}

function SecretField({
  k,
  configured,
}: {
  k: string;
  configured: boolean;
}) {
  return (
    <div>
      <div className="text-2xs uppercase tracking-wider text-ink-400">{k}</div>
      <div className="mt-0.5 flex items-center gap-1.5 text-xs">
        <Lock size={11} className="text-ink-400" />
        {configured ? (
          <span className="pill pill-good">Configured</span>
        ) : (
          <span className="pill">Not set</span>
        )}
      </div>
    </div>
  );
}

function Grid({ children }: { children: React.ReactNode }) {
  return (
    <div className="grid grid-cols-1 gap-x-6 gap-y-3 sm:grid-cols-2 lg:grid-cols-3">
      {children}
    </div>
  );
}

function SubCard({
  title,
  children,
}: {
  title: React.ReactNode;
  children: React.ReactNode;
}) {
  return (
    <div className="rounded-md border border-ink-100 bg-ink-50/40 px-3 py-2">
      <div className="mb-2 text-xs font-medium text-ink-700">{title}</div>
      {children}
    </div>
  );
}

function EnablePill({ enabled }: { enabled: boolean }) {
  return (
    <span className={`pill ${enabled ? "pill-good" : ""}`}>
      {enabled ? "Enabled" : "Disabled"}
    </span>
  );
}

function SecretBanner() {
  return (
    <div className="flex items-start gap-2 rounded-md border border-ink-100 bg-ink-50 px-3 py-2 text-xs text-ink-600">
      <Lock size={13} className="mt-0.5 shrink-0 text-ink-400" />
      <div>
        Read-only view. API keys and source credentials are{" "}
        <strong>never sent to the browser</strong> — only their presence is
        shown. To change any value edit{" "}
        <code className="rounded bg-white px-1 py-0.5 font-mono">
          config/config.yaml
        </code>{" "}
        or the corresponding environment variable.
        <a
          href="https://docs.versusincident.com/#/agent/configuration"
          target="_blank"
          rel="noreferrer"
          className="ml-2 inline-flex items-center gap-1 text-accent hover:underline"
        >
          Agent configuration reference <ExternalLink size={11} />
        </a>
      </div>
    </div>
  );
}
