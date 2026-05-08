import { useMemo, useState } from "react";
import { Link } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Eraser, Save } from "lucide-react";
import { api } from "@/lib/api";
import { fmtRel, truncate } from "@/lib/format";
import { TopBar } from "@/components/TopBar";
import { VerdictPill } from "@/components/Pill";
import { EmptyState, ErrorBox, Spinner } from "@/components/feedback";

const VERDICT_FILTERS = ["all", "spike", "unknown"] as const;
type VerdictFilter = (typeof VERDICT_FILTERS)[number];

export function ShadowPage() {
  const qc = useQueryClient();
  const events = useQuery({ queryKey: ["shadow"], queryFn: api.listShadow });
  const stats = useQuery({
    queryKey: ["shadow-stats"],
    queryFn: api.shadowStats,
  });

  const flush = useMutation({
    mutationFn: api.flushShadow,
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["shadow"] });
      qc.invalidateQueries({ queryKey: ["shadow-stats"] });
    },
  });
  const clear = useMutation({
    mutationFn: api.clearShadow,
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["shadow"] });
      qc.invalidateQueries({ queryKey: ["shadow-stats"] });
    },
  });

  const [filter, setFilter] = useState<VerdictFilter>("all");

  const list = useMemo(() => {
    if (!events.data) return [];
    if (filter === "all") return events.data;
    return events.data.filter((e) => e.verdict === filter);
  }, [events.data, filter]);

  return (
    <>
      <TopBar
        title="Shadow"
        subtitle={
          stats.data
            ? `${stats.data.events} entries · ${stats.data.total_signals} signals`
            : undefined
        }
        actions={
          <>
            <button
              className="btn"
              onClick={() => flush.mutate()}
              disabled={flush.isPending}
            >
              <Save size={12} /> Flush
            </button>
            <button
              className="btn btn-danger"
              onClick={() => {
                if (confirm("Clear every shadow event from disk?"))
                  clear.mutate();
              }}
              disabled={clear.isPending}
            >
              <Eraser size={12} /> Clear log
            </button>
          </>
        }
      />

      <main className="flex-1 overflow-auto p-6">
        {events.isError && <ErrorBox error={events.error} />}

        <div className="mb-3 inline-flex rounded-md border border-ink-200 bg-white p-0.5 text-xs">
          {VERDICT_FILTERS.map((f) => (
            <button
              key={f}
              onClick={() => setFilter(f)}
              className={
                "rounded-md px-3 py-1 transition-colors " +
                (filter === f
                  ? "bg-accent text-white"
                  : "text-ink-600 hover:bg-ink-50")
              }
            >
              {f === "all"
                ? "All"
                : f === "spike"
                  ? `Spikes (${stats.data?.verdicts?.spike ?? 0})`
                  : `Unknown (${stats.data?.verdicts?.unknown ?? 0})`}
            </button>
          ))}
        </div>

        <div className="card overflow-hidden">
          <div className="max-h-[calc(100vh-220px)] overflow-auto">
            <table className="ddt">
              <thead>
                <tr>
                  <th className="w-28">Verdict</th>
                  <th className="w-32">Pattern</th>
                  <th className="w-28">Source</th>
                  <th className="w-24">Rule</th>
                  <th className="w-20 text-right">Signals</th>
                  <th className="w-20 text-right">Ticks</th>
                  <th>Sample</th>
                  <th className="w-32">Last seen</th>
                </tr>
              </thead>
              <tbody>
                {events.isLoading && (
                  <tr>
                    <td colSpan={8}>
                      <Spinner />
                    </td>
                  </tr>
                )}
                {!events.isLoading && list.length === 0 && (
                  <tr>
                    <td colSpan={8}>
                      <EmptyState
                        title="No shadow events match this filter."
                        hint="Switch to shadow mode and inject some traffic."
                      />
                    </td>
                  </tr>
                )}
                {list.map((e) => (
                  <tr key={`${e.pattern_id}-${e.first_seen}`}>
                    <td>
                      <VerdictPill verdict={e.verdict} />
                    </td>
                    <td className="font-mono text-2xs">
                      <Link
                        to={`/shadow/${encodeURIComponent(e.pattern_id)}`}
                        className="text-accent hover:underline"
                      >
                        {e.pattern_id}
                      </Link>
                    </td>
                    <td className="text-2xs">{e.source}</td>
                    <td className="text-2xs">{e.rule_name || "—"}</td>
                    <td className="text-right tabular-nums">{e.count}</td>
                    <td className="text-right tabular-nums">{e.occurrences}</td>
                    <td className="font-mono text-2xs text-ink-700">
                      <Link
                        to={`/shadow/${encodeURIComponent(e.pattern_id)}`}
                        className="hover:text-accent hover:underline"
                      >
                        {truncate(e.sample_message, 120)}
                      </Link>
                    </td>
                    <td className="text-2xs text-ink-500">
                      {fmtRel(e.last_seen)}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </div>
      </main>
    </>
  );
}
