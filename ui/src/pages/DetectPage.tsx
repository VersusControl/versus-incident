import { useMemo, useState } from "react";
import { Link } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Eraser, Save } from "lucide-react";
import { api, type DetectEvent } from "@/lib/api";
import { fmtRel, truncate } from "@/lib/format";
import { TopBar } from "@/components/TopBar";
import { Pill, VerdictPill } from "@/components/Pill";
import { EmptyState, ErrorBox, Spinner } from "@/components/feedback";

const OUTCOMES = [
  "all",
  "emitted",
  "cached",
  "dry",
  "quota",
  "ai_error",
  "send_error",
] as const;
type OutcomeFilter = (typeof OUTCOMES)[number];

export function DetectPage() {
  const qc = useQueryClient();
  const events = useQuery({ queryKey: ["detect"], queryFn: api.listDetect });
  const stats = useQuery({
    queryKey: ["detect-stats"],
    queryFn: api.detectStats,
  });

  const flush = useMutation({
    mutationFn: api.flushDetect,
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["detect"] });
      qc.invalidateQueries({ queryKey: ["detect-stats"] });
    },
  });
  const clear = useMutation({
    mutationFn: api.clearDetect,
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["detect"] });
      qc.invalidateQueries({ queryKey: ["detect-stats"] });
    },
  });

  const [filter, setFilter] = useState<OutcomeFilter>("all");

  const list = useMemo(() => {
    if (!events.data) return [];
    if (filter === "all") return events.data;
    return events.data.filter((e) => e.outcome === filter);
  }, [events.data, filter]);

  const totals = stats.data ?? {};

  return (
    <>
      <TopBar
        title="Detect"
        subtitle={
          stats.data
            ? `${totals.events ?? 0} AI calls audited`
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
                if (confirm("Clear every detect event from disk?"))
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

        <div className="mb-3 inline-flex flex-wrap gap-0.5 rounded-md border border-ink-200 bg-white p-0.5 text-xs">
          {OUTCOMES.map((f) => {
            const count =
              f === "all" ? totals.events ?? 0 : totals[`outcome_${f}`] ?? 0;
            return (
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
                {f === "all" ? "All" : f}
                <span className="ml-1 text-2xs opacity-70">({count})</span>
              </button>
            );
          })}
        </div>

        <div className="card overflow-hidden">
          <div className="max-h-[calc(100vh-220px)] overflow-auto">
            <table className="ddt">
              <thead>
                <tr>
                  <th className="w-32">When</th>
                  <th className="w-24">Outcome</th>
                  <th className="w-24">Verdict</th>
                  <th className="w-20">Severity</th>
                  <th className="w-32">Service</th>
                  <th className="w-32">Pattern</th>
                  <th>Title / Sample</th>
                  <th className="w-16 text-right">Freq</th>
                  <th className="w-16 text-right">ms</th>
                </tr>
              </thead>
              <tbody>
                {events.isLoading && (
                  <tr>
                    <td colSpan={9}>
                      <Spinner />
                    </td>
                  </tr>
                )}
                {!events.isLoading && list.length === 0 && (
                  <tr>
                    <td colSpan={9}>
                      <EmptyState
                        title="No detect events match this filter."
                        hint="Switch the agent to detect mode and let it call the AI SRE."
                      />
                    </td>
                  </tr>
                )}
                {list.map((e) => (
                  <DetectRow key={e.id} e={e} />
                ))}
              </tbody>
            </table>
          </div>
        </div>
      </main>
    </>
  );
}

function DetectRow({ e }: { e: DetectEvent }) {
  const titleOrSample =
    e.finding?.Title ||
    (e.samples && e.samples[0]) ||
    e.template ||
    e.error ||
    "—";
  return (
    <tr>
      <td className="text-2xs text-ink-500">
        <Link to={`/detect/${encodeURIComponent(e.id)}`} className="text-accent hover:underline">
          {fmtRel(e.timestamp)}
        </Link>
      </td>
      <td>
        <OutcomePill outcome={e.outcome} />
      </td>
      <td>
        <VerdictPill verdict={e.verdict} />
      </td>
      <td>
        <SeverityPill severity={e.finding?.Severity} />
      </td>
      <td className="text-2xs">{e.service || "—"}</td>
      <td className="font-mono text-2xs">
        <Link
          to={`/patterns/${encodeURIComponent(e.pattern_id)}`}
          className="text-accent hover:underline"
        >
          {truncate(e.pattern_id, 14)}
        </Link>
      </td>
      <td className="font-mono text-2xs text-ink-700">
        <Link
          to={`/detect/${encodeURIComponent(e.id)}`}
          className="hover:text-accent hover:underline"
        >
          {truncate(titleOrSample, 120)}
        </Link>
      </td>
      <td className="text-right tabular-nums">{e.frequency}</td>
      <td className="text-right tabular-nums text-ink-500">
        {e.duration_ms ?? "—"}
      </td>
    </tr>
  );
}

export function OutcomePill({ outcome }: { outcome: string }) {
  const o = (outcome || "").toLowerCase();
  if (o === "emitted") return <Pill tone="good">emitted</Pill>;
  if (o === "cached") return <Pill tone="accent">cached</Pill>;
  if (o === "dry") return <Pill>dry</Pill>;
  if (o === "quota") return <Pill tone="warn">quota</Pill>;
  if (o === "ai_error") return <Pill tone="bad">ai_error</Pill>;
  if (o === "send_error") return <Pill tone="bad">send_error</Pill>;
  return <Pill>{outcome || "—"}</Pill>;
}

export function SeverityPill({ severity }: { severity?: string }) {
  const s = (severity || "").toLowerCase();
  if (!s) return <Pill>—</Pill>;
  if (s === "critical" || s === "high") return <Pill tone="bad">{s}</Pill>;
  if (s === "medium") return <Pill tone="warn">{s}</Pill>;
  if (s === "low") return <Pill tone="good">{s}</Pill>;
  return <Pill tone="accent">{s}</Pill>;
}
