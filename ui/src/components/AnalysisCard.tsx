import { useState } from "react";
import { Link } from "react-router-dom";
import { Brain, ChevronDown, ChevronRight } from "lucide-react";
import type { AnalysisRecord } from "@/lib/api";
import { fmtAbs, fmtRel } from "@/lib/format";
import { Pill } from "@/components/Pill";
import { ErrorBox } from "@/components/feedback";

// AnalysisCard renders one AnalysisRecord: status header, root-cause
// hypotheses, evidence (collapsible per item), next steps, raw payload
// fallback when the finding could not be parsed, and the tool-call
// audit trail. Shared by the incident detail page and the dedicated
// analysis pages.
export function AnalysisCard({
  rec,
  title,
}: {
  rec: AnalysisRecord;
  title: string;
}) {
  const finding = rec.finding;
  const status = rec.status;
  const statusTone =
    status === "ok" ? "good" : status === "error" ? "bad" : "warn";
  return (
    <div className="card">
      <div className="card-header">
        <span className="card-title flex items-center gap-1.5">
          <Brain size={12} className="text-accent" />
          {title}
        </span>
        <div className="flex items-center gap-1.5">
          <Pill tone={statusTone}>{status}</Pill>
          {rec.model && <Pill tone="accent">{rec.model}</Pill>}
          {rec.duration_ms !== undefined && (
            <Pill>{formatDuration(rec.duration_ms)}</Pill>
          )}
        </div>
      </div>
      <div className="card-body space-y-4 text-xs">
        <div className="flex flex-wrap items-center gap-x-4 gap-y-1 text-2xs text-ink-500">
          <span title={fmtAbs(rec.requested_at)}>
            {fmtRel(rec.requested_at)}
          </span>
          {rec.requested_by && <span>by {rec.requested_by}</span>}
          <span className="font-mono">{rec.id.slice(0, 8)}</span>
        </div>

        {status === "error" && rec.error && (
          <ErrorBox error={new Error(rec.error)} />
        )}

        {finding?.Summary && (
          <p className="whitespace-pre-wrap leading-relaxed text-ink-800">
            {finding.Summary}
          </p>
        )}

        {finding?.root_cause_hypotheses &&
          finding.root_cause_hypotheses.length > 0 && (
            <div>
              <div className="mb-1.5 text-2xs uppercase tracking-wider text-ink-400">
                Root cause hypotheses
              </div>
              <ol className="space-y-2">
                {finding.root_cause_hypotheses.map((h, i) => (
                  <li
                    key={i}
                    className="rounded-md border border-ink-100 bg-ink-50 p-2"
                  >
                    <div className="flex items-start gap-2">
                      <span className="mt-0.5 inline-flex h-5 w-5 flex-none items-center justify-center rounded-full bg-accent/10 font-mono text-2xs text-accent">
                        {i + 1}
                      </span>
                      <div className="min-w-0 flex-1 space-y-1">
                        <div className="flex flex-wrap items-baseline gap-2">
                          <span className="font-medium text-ink-900">
                            {h.hypothesis}
                          </span>
                          <span className="font-mono text-2xs text-ink-500">
                            {(h.confidence * 100).toFixed(0)}%
                          </span>
                        </div>
                        {h.rationale && (
                          <p className="text-2xs leading-relaxed text-ink-600">
                            {h.rationale}
                          </p>
                        )}
                      </div>
                    </div>
                  </li>
                ))}
              </ol>
            </div>
          )}

        {finding?.evidence && finding.evidence.length > 0 && (
          <EvidenceList items={finding.evidence} />
        )}

        {finding?.next_steps && finding.next_steps.length > 0 && (
          <div>
            <div className="mb-1.5 text-2xs uppercase tracking-wider text-ink-400">
              Next steps
            </div>
            <ol className="list-decimal space-y-1 pl-5 leading-relaxed text-ink-800">
              {finding.next_steps.map((s, i) => (
                <li key={i}>{s}</li>
              ))}
            </ol>
          </div>
        )}

        {finding?.related_pattern_ids &&
          finding.related_pattern_ids.length > 0 && (
            <div className="flex flex-wrap items-center gap-1.5">
              <span className="text-2xs uppercase tracking-wider text-ink-400">
                Related patterns
              </span>
              {finding.related_pattern_ids.map((pid) => (
                <Link
                  key={pid}
                  to={`/patterns/${pid}`}
                  className="font-mono text-2xs text-accent hover:underline"
                >
                  {pid.slice(0, 12)}
                </Link>
              ))}
            </div>
          )}

        {!finding && rec.raw_response && (
          <details>
            <summary className="cursor-pointer text-2xs uppercase tracking-wider text-ink-400">
              Raw model response
            </summary>
            <pre className="mt-2 max-h-64 overflow-auto whitespace-pre-wrap break-words rounded-md bg-ink-50 p-2 font-mono text-2xs leading-snug text-ink-800">
              {rec.raw_response}
            </pre>
          </details>
        )}

        {rec.tool_calls && rec.tool_calls.length > 0 && (
          <details>
            <summary className="cursor-pointer text-2xs uppercase tracking-wider text-ink-400">
              Tool calls ({rec.tool_calls.length})
            </summary>
            <ul className="mt-2 space-y-2">
              {rec.tool_calls.map((tc, i) => (
                <li
                  key={i}
                  className="rounded-md border border-ink-100 bg-ink-50 p-2 text-2xs"
                >
                  <div className="flex items-center justify-between">
                    <span className="font-mono text-ink-900">{tc.name}</span>
                    {tc.duration_ms !== undefined && (
                      <span className="text-ink-500">
                        {formatDuration(tc.duration_ms)}
                      </span>
                    )}
                  </div>
                  {tc.error && <p className="mt-1 text-bad">error: {tc.error}</p>}
                  {tc.args !== undefined && tc.args !== null && (
                    <pre className="mt-1 max-h-32 overflow-auto whitespace-pre-wrap break-words font-mono text-ink-700">
                      args: {jsonString(tc.args)}
                    </pre>
                  )}
                  {tc.output !== undefined && tc.output !== null && (
                    <pre className="mt-1 max-h-32 overflow-auto whitespace-pre-wrap break-words font-mono text-ink-700">
                      output: {jsonString(tc.output)}
                    </pre>
                  )}
                </li>
              ))}
            </ul>
          </details>
        )}
      </div>
    </div>
  );
}

type AIFindingEvidence = NonNullable<AnalysisRecord["finding"]>["evidence"];

// EvidenceList renders the analyze agent's evidence array. Each row
// shows the source tag + a short summary; the longer Detail field
// expands on click.
function EvidenceList({ items }: { items: NonNullable<AIFindingEvidence> }) {
  const [open, setOpen] = useState<Record<number, boolean>>({});
  return (
    <div>
      <div className="mb-1.5 text-2xs uppercase tracking-wider text-ink-400">
        Evidence
      </div>
      <ul className="space-y-1.5">
        {items.map((e, i) => {
          const expanded = !!open[i];
          const hasDetail = !!e.detail;
          return (
            <li
              key={i}
              className="rounded-md border border-ink-100 bg-ink-50 p-2"
            >
              <button
                type="button"
                className="flex w-full items-start gap-2 text-left"
                onClick={() =>
                  hasDetail && setOpen((s) => ({ ...s, [i]: !expanded }))
                }
                aria-expanded={expanded}
                disabled={!hasDetail}
              >
                <span className="mt-0.5 text-ink-400">
                  {hasDetail ? (
                    expanded ? (
                      <ChevronDown size={11} />
                    ) : (
                      <ChevronRight size={11} />
                    )
                  ) : (
                    <span className="inline-block w-[11px]" />
                  )}
                </span>
                <div className="min-w-0 flex-1">
                  <div className="flex flex-wrap items-baseline gap-2">
                    <Pill tone="accent">{e.source}</Pill>
                    <span className="text-ink-800">{e.summary}</span>
                  </div>
                  {expanded && e.detail && (
                    <pre className="mt-2 max-h-48 overflow-auto whitespace-pre-wrap break-words font-mono text-2xs leading-snug text-ink-700">
                      {e.detail}
                    </pre>
                  )}
                </div>
              </button>
            </li>
          );
        })}
      </ul>
    </div>
  );
}

export function formatDuration(ms: number): string {
  if (ms < 1000) return `${ms}ms`;
  const s = ms / 1000;
  if (s < 60) return `${s.toFixed(1)}s`;
  const m = Math.floor(s / 60);
  const rem = Math.round(s - m * 60);
  return `${m}m${rem.toString().padStart(2, "0")}s`;
}

export function jsonString(v: unknown): string {
  try {
    return typeof v === "string" ? v : JSON.stringify(v, null, 2);
  } catch {
    return String(v);
  }
}
