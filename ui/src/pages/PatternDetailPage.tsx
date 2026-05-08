import { useEffect, useState } from "react";
import { useNavigate, useParams, Link } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ArrowLeft, Trash2 } from "lucide-react";
import { api } from "@/lib/api";
import { fmtAbs, fmtRel } from "@/lib/format";
import { TopBar } from "@/components/TopBar";
import { Pill, VerdictPill } from "@/components/Pill";
import { ErrorBox, Spinner } from "@/components/feedback";

export function PatternDetailPage() {
  const { id = "" } = useParams();
  const nav = useNavigate();
  const qc = useQueryClient();

  const { data, isLoading, isError, error } = useQuery({
    queryKey: ["pattern", id],
    queryFn: () => api.getPattern(id),
    enabled: !!id,
  });

  const [verdict, setVerdict] = useState("");
  const [tagsText, setTagsText] = useState("");

  // Sync local form state when the underlying record loads or refreshes.
  useEffect(() => {
    if (data) {
      setVerdict(data.verdict || "");
      setTagsText((data.tags || []).join(", "));
    }
  }, [data]);

  const update = useMutation({
    mutationFn: () =>
      api.updatePattern(id, {
        verdict,
        tags: tagsText
          .split(",")
          .map((t) => t.trim())
          .filter(Boolean),
      }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["pattern", id] });
      qc.invalidateQueries({ queryKey: ["patterns"] });
    },
  });

  const del = useMutation({
    mutationFn: () => api.deletePattern(id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["patterns"] });
      nav("/patterns", { replace: true });
    },
  });

  return (
    <>
      <TopBar
        title="Pattern detail"
        subtitle={id}
        actions={
          <Link to="/patterns" className="btn">
            <ArrowLeft size={12} /> Back
          </Link>
        }
      />

      <main className="flex-1 overflow-auto p-6">
        {isLoading && <Spinner />}
        {isError && <ErrorBox error={error} />}
        {data && (
          <div className="grid grid-cols-1 gap-6 lg:grid-cols-3">
            <section className="card lg:col-span-2">
              <div className="card-header">
                <h2 className="card-title">Template</h2>
                <VerdictPill verdict={data.verdict} />
              </div>
              <div className="card-body">
                <pre className="overflow-auto rounded-md bg-ink-900 p-3 font-mono text-2xs leading-relaxed text-ink-100">
                  {data.template}
                </pre>
                <dl className="mt-4 grid grid-cols-2 gap-x-6 gap-y-2 text-xs sm:grid-cols-3">
                  <Fact label="Rule"     value={data.rule_name || "—"} />
                  <Fact label="Service"  value={data.service || "—"} />
                  <Fact label="Source"   value={data.source} />
                  <Fact label="Count"    value={data.count} />
                  <Fact
                    label="Baseline (EWMA)"
                    value={data.baseline_frequency.toFixed(3)}
                  />
                  <Fact
                    label="First seen"
                    value={`${fmtAbs(data.first_seen)} (${fmtRel(data.first_seen)})`}
                  />
                  <Fact
                    label="Last seen"
                    value={`${fmtAbs(data.last_seen)} (${fmtRel(data.last_seen)})`}
                  />
                </dl>
              </div>
            </section>

            <section className="card">
              <div className="card-header">
                <h2 className="card-title">Curate</h2>
              </div>
              <div className="card-body space-y-3">
                <label className="block">
                  <div className="mb-1 text-2xs uppercase tracking-wider text-ink-400">
                    Verdict
                  </div>
                  <select
                    className="input"
                    value={verdict}
                    onChange={(e) => setVerdict(e.target.value)}
                  >
                    <option value="">(none)</option>
                    <option value="known">known</option>
                  </select>
                </label>
                <label className="block">
                  <div className="mb-1 text-2xs uppercase tracking-wider text-ink-400">
                    Tags
                  </div>
                  <input
                    className="input"
                    value={tagsText}
                    onChange={(e) => setTagsText(e.target.value)}
                    placeholder="comma, separated"
                  />
                  {data.tags && data.tags.length > 0 && (
                    <div className="mt-2 flex flex-wrap gap-1">
                      {data.tags.map((t) => (
                        <Pill key={t} tone="accent">
                          {t}
                        </Pill>
                      ))}
                    </div>
                  )}
                </label>

                <div className="flex flex-wrap gap-2 pt-2">
                  <button
                    className="btn btn-primary"
                    disabled={update.isPending}
                    onClick={() => update.mutate()}
                  >
                    Save
                  </button>
                  <button
                    className="btn btn-danger"
                    disabled={del.isPending}
                    onClick={() => {
                      if (confirm(`Delete pattern ${id}?`)) del.mutate();
                    }}
                  >
                    <Trash2 size={12} /> Delete
                  </button>
                </div>
                {update.isError && <ErrorBox error={update.error} />}
                {del.isError && <ErrorBox error={del.error} />}
              </div>
            </section>
          </div>
        )}
      </main>
    </>
  );
}

function Fact({ label, value }: { label: string; value: React.ReactNode }) {
  return (
    <div>
      <dt className="text-2xs uppercase tracking-wider text-ink-400">
        {label}
      </dt>
      <dd className="font-mono text-xs text-ink-800">{value}</dd>
    </div>
  );
}
