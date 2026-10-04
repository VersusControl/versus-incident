import { useQuery } from "@tanstack/react-query";
import { RetryableError } from "@/components/RetryableError";
import { SkCard } from "@/components/Skeleton";
import {
  EnablePill,
  KV,
  KVGrid,
  ReadOnlyCard,
  SecretField,
  SubCard,
} from "@/components/settings/ReadOnlyValues";
import { api } from "@/lib/api";

export type AgentConfigPart = "runtime" | "sources" | "ai" | "patterns";

// Read-only view of the AI agent configuration, one part per Settings section.
// Secrets (AI API key, source credentials) render only as configured / not set.
export function AgentConfigSection({ part }: { part: AgentConfigPart }) {
  const cfg = useQuery({
    queryKey: ["config-agent"],
    queryFn: api.getAgentConfig,
  });

  if (cfg.isLoading) {
    return <SkCard lines={4} />;
  }

  if (cfg.isError) {
    return (
      <RetryableError
        error={cfg.error}
        onRetry={() => cfg.refetch()}
        retrying={cfg.isRefetching}
        context="Couldn't load agent configuration"
      />
    );
  }

  if (!cfg.data) return null;
  const data = cfg.data;

  if (part === "runtime") {
    return (
      <div className="space-y-4">
        <ReadOnlyCard title="Agent" aside={<EnablePill enabled={data.enable} />}>
          <KVGrid>
            <KV k="Mode" v={data.mode || "—"} />
            <KV k="Poll interval" v={data.poll_interval || "—"} />
            <KV k="Lookback" v={data.lookback || "—"} />
            <KV k="Batch max" v={String(data.batch_max)} />
            <KV k="Signal max bytes" v={String(data.signal_max_bytes)} />
            <KV k="New service grace" v={data.new_service_grace || "0 (disabled)"} />
            <KV k="Sources file" v={data.sources_path || "(inline)"} />
          </KVGrid>
        </ReadOnlyCard>
        <ReadOnlyCard title="Redaction" aside={<EnablePill enabled={data.redaction.enable} />}>
          <KVGrid>
            <KV k="Redact IPs" v={String(data.redaction.redact_ips)} />
            <KV k="Extra patterns" v={String(data.redaction.extra_pattern_count)} />
          </KVGrid>
        </ReadOnlyCard>
      </div>
    );
  }

  if (part === "sources") {
    if (data.sources.length === 0) {
      return (
        <ReadOnlyCard>
          <p className="text-xs text-ink-400">
            No sources defined. See <code>agent.sources_path</code> or the inline{" "}
            <code>agent.sources</code> list.
          </p>
        </ReadOnlyCard>
      );
    }
    return (
      <div className="space-y-3">
        <p className="text-2xs text-ink-400">{data.sources.length} configured</p>
        {data.sources.map((s) => (
          <SubCard
            key={s.name}
            title={
              <div className="flex items-center gap-2">
                <span className="font-mono text-xs text-ink-100">{s.name}</span>
                <span className="pill">{s.type}</span>
                <EnablePill enabled={s.enable} />
              </div>
            }
          >
            {renderDetailsKV(s.details)}
          </SubCard>
        ))}
      </div>
    );
  }

  if (part === "ai") {
    return (
      <ReadOnlyCard title="Model" aside={<EnablePill enabled={data.ai.enable} />}>
        <KVGrid>
          <KV k="Model" v={data.ai.model || "—"} />
          <SecretField k="API key" configured={data.ai.api_key === "set"} />
          <KV k="Temperature" v={String(data.ai.temperature)} />
          <KV k="Max tokens" v={String(data.ai.max_tokens)} />
          <KV k="Max calls/hour" v={String(data.ai.max_calls_per_hour)} />
          <KV k="Cache TTL" v={data.ai.cache_ttl || "—"} />
        </KVGrid>
      </ReadOnlyCard>
    );
  }

  return (
    <div className="space-y-4">
      <ReadOnlyCard title="Catalog & miner">
        <SubCard title="Catalog">
          <KVGrid>
            <KV k="Persist interval" v={data.catalog.persist_interval || "—"} />
            <KV k="Auto promote after" v={String(data.catalog.auto_promote_after)} />
            <KV k="Spike multiplier" v={String(data.catalog.spike_multiplier)} />
            <KV k="Spike min frequency" v={String(data.catalog.spike_min_frequency)} />
            <KV k="Spike min baseline" v={String(data.catalog.spike_min_baseline_count)} />
          </KVGrid>
        </SubCard>
        <SubCard title="Miner">
          <KVGrid>
            <KV k="Similarity threshold" v={String(data.miner.similarity_threshold)} />
            <KV k="Tree depth" v={String(data.miner.tree_depth)} />
            <KV k="Max children" v={String(data.miner.max_children)} />
          </KVGrid>
        </SubCard>
      </ReadOnlyCard>

      <ReadOnlyCard
        title="Regex pre-filter"
        aside={<span className="text-xs text-ink-400">{data.regex.rules.length} rule(s)</span>}
      >
        <KV k="Default pattern" v={data.regex.default_pattern || "(none — strict mode)"} />
        {data.regex.rules.length > 0 && (
          <table className="ddt">
            <thead>
              <tr>
                <th>Name</th>
                <th>Pattern</th>
              </tr>
            </thead>
            <tbody>
              {data.regex.rules.map((r) => (
                <tr key={r.name}>
                  <td className="font-mono text-xs">{r.name}</td>
                  <td className="font-mono text-xs text-ink-300">{r.pattern}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </ReadOnlyCard>

      <ReadOnlyCard
        title="Service patterns"
        aside={<span className="text-xs text-ink-400">{data.service_patterns.length} configured</span>}
      >
        {data.service_patterns.length === 0 ? (
          <p className="text-xs text-ink-400">
            Not configured — service detection is off and signals are attributed to{" "}
            <code>_unknown</code>.
          </p>
        ) : (
          <ul className="space-y-1">
            {data.service_patterns.map((p, i) => (
              <li key={i} className="rounded bg-ink-700 px-2 py-1 font-mono text-xs text-ink-200">
                {p}
              </li>
            ))}
          </ul>
        )}
      </ReadOnlyCard>
    </div>
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
    <KVGrid>
      {entries.map(([k, v]) => (
        <KV key={k} k={prettyKey(k)} v={formatDetailValue(v)} />
      ))}
    </KVGrid>
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
