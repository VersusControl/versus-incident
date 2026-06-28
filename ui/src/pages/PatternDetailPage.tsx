import { useEffect, useState } from "react";
import { useNavigate, useParams, Link } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ArrowLeft, Trash2 } from "lucide-react";
import { api } from "@/lib/api";
import { fmtAbs, fmtRel } from "@/lib/format";
import { TopBar } from "@/components/TopBar";
import { Pill, VerdictPill } from "@/components/Pill";
import { ErrorBox } from "@/components/feedback";
import { SkCard } from "@/components/Skeleton";
import { RetryableError } from "@/components/RetryableError";
import { ConfirmDialog } from "@/components/ConfirmDialog";
import { useToast } from "@/components/toastContext";

const BUILTIN_VERDICTS = ["", "known", "spike"];

export function PatternDetailPage() {
  const { id = "" } = useParams();
  const nav = useNavigate();
  const qc = useQueryClient();
  const toast = useToast();

  const { data, isLoading, isError, error, refetch, isRefetching } = useQuery({
    queryKey: ["pattern", id],
    queryFn: () => api.getPattern(id),
    enabled: !!id,
  });

  const [verdict, setVerdict] = useState("");
  const [tagsText, setTagsText] = useState("");
  const [confirmDelete, setConfirmDelete] = useState(false);

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
      toast.push({
        tone: "ok",
        title: "Pattern updated",
        description: `Verdict ${verdict || "(none)"} saved`,
      });
    },
    onError: (err) => {
      toast.push({
        tone: "error",
        title: "Update failed",
        description: err.message,
        action: { label: "Retry", onClick: () => update.mutate() },
      });
    },
  });

  const del = useMutation({
    mutationFn: () => api.deletePattern(id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["patterns"] });
      toast.push({ tone: "ok", title: "Pattern deleted", description: id });
      nav("/agent/logs", { replace: true });
    },
    onError: (err) => {
      toast.push({
        tone: "error",
        title: "Delete failed",
        description: err.message,
      });
    },
  });

  return (
    <>
      <TopBar
        title="Logs"
        subtitle={id}
        actions={
          <Link to="/agent/logs" className="btn">
            <ArrowLeft size={12} /> Back
          </Link>
        }
      />

      <main className="flex-1 overflow-auto p-4 lg:p-6">
        {isLoading && (
          <div className="grid grid-cols-1 gap-6 lg:grid-cols-3">
            <SkCard lines={6} className="lg:col-span-2" />
            <SkCard lines={4} />
          </div>
        )}
        {isError && (
          <RetryableError
            error={error}
            onRetry={() => refetch()}
            retrying={isRefetching}
            context={`Couldn't load pattern ${id}`}
          />
        )}
        {data && (
          <div className="grid grid-cols-1 gap-6 lg:grid-cols-3">
            <section className="card lg:col-span-2">
              <div className="card-header">
                <h2 className="card-title">Template</h2>
                <VerdictPill verdict={data.verdict} />
              </div>
              <div className="card-body">
                <pre className="overflow-auto rounded-md border border-ink-600 bg-surface-sunken p-3 font-mono text-2xs leading-relaxed text-ink-100">
                  {data.template}
                </pre>
                <dl className="mt-4 grid grid-cols-2 gap-x-6 gap-y-2 text-xs sm:grid-cols-3">
                  <Fact label="Rule" value={data.rule_name || "—"} />
                  <Fact label="Service" value={data.service || "—"} />
                  <Fact label="Source" value={data.source} />
                  <Fact label="Count" value={data.count} />
                  <Fact
                    label="Normal"
                    value={`≈ ${data.baseline_frequency.toFixed(1)}`}
                  />
                  <Fact
                    label="First seen"
                    value={
                      <span title={fmtAbs(data.first_seen)}>
                        {fmtAbs(data.first_seen)} ({fmtRel(data.first_seen)})
                      </span>
                    }
                  />
                  <Fact
                    label="Last seen"
                    value={
                      <span title={fmtAbs(data.last_seen)}>
                        {fmtAbs(data.last_seen)} ({fmtRel(data.last_seen)})
                      </span>
                    }
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
                  <div className="mb-1 text-2xs uppercase tracking-wider text-ink-300">
                    Verdict
                  </div>
                  <select
                    className="input"
                    value={verdict}
                    onChange={(e) => setVerdict(e.target.value)}
                  >
                    <option value="">(none)</option>
                    <option value="known">known</option>
                    <option value="spike">spike</option>
                    {!BUILTIN_VERDICTS.includes(verdict) && (
                      <option value={verdict}>{verdict}</option>
                    )}
                  </select>
                </label>
                <label className="block">
                  <div className="mb-1 text-2xs uppercase tracking-wider text-ink-300">
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
                    {update.isPending ? "Saving…" : "Save"}
                  </button>
                  <button
                    className="btn btn-danger"
                    disabled={del.isPending}
                    onClick={() => setConfirmDelete(true)}
                  >
                    <Trash2 size={12} /> Delete
                  </button>
                </div>
                {update.isError && <ErrorBox error={update.error} />}
              </div>
            </section>
          </div>
        )}
      </main>

      {confirmDelete && (
        <ConfirmDialog
          title="Delete pattern"
          message={
            <>
              Delete pattern <span className="font-mono">{id}</span>? The agent
              may re-learn it from future logs.
            </>
          }
          confirmLabel="Delete"
          tone="danger"
          busy={del.isPending}
          error={del.error}
          onConfirm={() => del.mutate()}
          onClose={() => setConfirmDelete(false)}
        />
      )}
    </>
  );
}

function Fact({ label, value }: { label: string; value: React.ReactNode }) {
  return (
    <div>
      <dt className="text-2xs uppercase tracking-wider text-ink-300">
        {label}
      </dt>
      <dd className="font-mono text-xs text-ink-100">{value}</dd>
    </div>
  );
}
