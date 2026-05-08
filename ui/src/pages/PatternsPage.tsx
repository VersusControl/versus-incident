import { useMemo, useState } from "react";
import { Link } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Save, Search } from "lucide-react";
import { api } from "@/lib/api";
import { fmtRel, truncate } from "@/lib/format";
import { TopBar } from "@/components/TopBar";
import { VerdictPill } from "@/components/Pill";
import { ErrorBox, Spinner } from "@/components/feedback";

export function PatternsPage() {
  const qc = useQueryClient();
  const { data, isLoading, isError, error } = useQuery({
    queryKey: ["patterns"],
    queryFn: api.listPatterns,
  });
  const flush = useMutation({
    mutationFn: api.flushPatterns,
    onSuccess: () => qc.invalidateQueries({ queryKey: ["patterns"] }),
  });

  const [q, setQ] = useState("");
  const [verdictFilter, setVerdictFilter] = useState("");

  const filtered = useMemo(() => {
    if (!data) return [];
    const needle = q.trim().toLowerCase();
    return data.filter((p) => {
      if (verdictFilter && p.verdict !== verdictFilter) return false;
      if (!needle) return true;
      return (
        p.template.toLowerCase().includes(needle) ||
        p.id.toLowerCase().includes(needle) ||
        (p.service ?? "").toLowerCase().includes(needle) ||
        (p.rule_name ?? "").toLowerCase().includes(needle)
      );
    });
  }, [data, q, verdictFilter]);

  return (
    <>
      <TopBar
        title="Patterns"
        subtitle={data ? `${data.length} learned` : undefined}
        actions={
          <button
            className="btn"
            disabled={flush.isPending}
            onClick={() => flush.mutate()}
          >
            <Save size={12} />
            Flush to disk
          </button>
        }
      />

      <main className="flex-1 overflow-auto p-6">
        <div className="mb-3 flex items-center gap-2">
          <div className="relative flex-1 max-w-md">
            <Search
              size={12}
              className="absolute left-2.5 top-1/2 -translate-y-1/2 text-ink-300"
            />
            <input
              className="input pl-7"
              placeholder="Search by id, service, rule, or template…"
              value={q}
              onChange={(e) => setQ(e.target.value)}
            />
          </div>
          <select
            className="input w-40"
            value={verdictFilter}
            onChange={(e) => setVerdictFilter(e.target.value)}
          >
            <option value="">All verdicts</option>
            <option value="">(empty)</option>
            <option value="known">known</option>
          </select>
        </div>

        {isError && <ErrorBox error={error} />}

        <div className="card overflow-hidden">
          <div className="max-h-[calc(100vh-220px)] overflow-auto">
            <table className="ddt">
              <thead>
                <tr>
                  <th className="w-32">ID</th>
                  <th className="w-28">Service</th>
                  <th>Template</th>
                  <th className="w-24">Rule</th>
                  <th className="w-24">Verdict</th>
                  <th className="w-20 text-right">Count</th>
                  <th className="w-28 text-right">EWMA</th>
                  <th className="w-32">Last seen</th>
                </tr>
              </thead>
              <tbody>
                {isLoading && (
                  <tr>
                    <td colSpan={8}>
                      <Spinner />
                    </td>
                  </tr>
                )}
                {!isLoading && filtered.length === 0 && (
                  <tr>
                    <td colSpan={8} className="py-8 text-center text-ink-400">
                      No patterns match your filters.
                    </td>
                  </tr>
                )}
                {filtered.map((p) => (
                  <tr key={p.id}>
                    <td className="font-mono text-2xs">
                      <Link
                        className="text-accent hover:underline"
                        to={`/patterns/${p.id}`}
                      >
                        {p.id}
                      </Link>
                    </td>
                    <td className="font-mono text-2xs">{p.service || "—"}</td>
                    <td className="font-mono text-2xs text-ink-700">
                      {truncate(p.template, 110)}
                    </td>
                    <td className="text-2xs">{p.rule_name || "—"}</td>
                    <td>
                      <VerdictPill verdict={p.verdict} />
                    </td>
                    <td className="text-right tabular-nums">{p.count}</td>
                    <td className="text-right tabular-nums">
                      {p.baseline_frequency.toFixed(2)}
                    </td>
                    <td className="text-2xs text-ink-500">
                      {fmtRel(p.last_seen)}
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
