import { useState } from "react";
import { Link } from "react-router-dom";
import { Brain, ChevronDown, ChevronRight } from "lucide-react";
import type { AnalysisRecord } from "@/lib/api";
import { fmtAbs, fmtRel, formatDuration, jsonString } from "@/lib/format";
import { Pill } from "@/components/Pill";
import { SeverityBadge } from "@/components/SeverityBadge";
import { ErrorBox } from "@/components/feedback";

// AnalysisCard renders one AnalysisRecord. The header leads with the
// finding's own conclusion — Title, SeverityBadge, Category and
// Confidence (audit S5: none of these had a render path before); the
// static `title` prop is only the fallback when the model produced no
// parseable finding. Below: summary, root-cause hypotheses, evidence
// (collapsible per item), suggestions, next steps, raw payload fallback,
// and the tool-call audit trail. Shared by the incident detail page and
// the dedicated analysis pages.
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
      <div className="card-header gap-2">
        <span className="card-title flex min-w-0 items-center gap-1.5">
          <Brain size={12} className="shrink-0 text-link" />
          <span className="truncate">{finding?.Title || title}</span>
        </span>
        <div className="flex flex-wrap items-center justify-end gap-1.5">
          {finding?.Severity && <SeverityBadge severity={finding.Severity} />}
          {finding?.Category && <Pill tone="accent">{finding.Category}</Pill>}
          {finding?.Confidence !== undefined && (
            <Pill title="Model confidence">
              {confidencePct(finding.Confidence)}% conf
            </Pill>
          )}
          <Pill tone={statusTone}>{status}</Pill>
          {rec.model && <Pill tone="accent">{rec.model}</Pill>}
          {rec.duration_ms !== undefined && (
            <Pill>{formatDuration(rec.duration_ms)}</Pill>
          )}
        </div>
      </div>
      <div className="card-body space-y-4 text-xs">
        <div className="flex flex-wrap items-center gap-x-4 gap-y-1 text-2xs text-ink-400">
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
          <p className="whitespace-pre-wrap leading-relaxed text-ink-100">
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
                    className="rounded-md border border-ink-600 bg-surface-raised p-2"
                  >
                    <div className="flex items-start gap-2">
                      <span className="mt-0.5 inline-flex h-5 w-5 flex-none items-center justify-center rounded-full bg-accent/10 font-mono text-2xs text-link">
                        {i + 1}
                      </span>
                      <div className="min-w-0 flex-1 space-y-1">
                        <div className="flex flex-wrap items-baseline gap-2">
                          <span className="font-medium text-ink-50">
                            {h.hypothesis}
                          </span>
                          <span className="font-mono text-2xs text-ink-300">
                            {(h.confidence * 100).toFixed(0)}%
                          </span>
                        </div>
                        {h.rationale && (
                          <p className="text-2xs leading-relaxed text-ink-300">
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

        {finding?.Suggestions && finding.Suggestions.length > 0 && (
          <div>
            <div className="mb-1.5 text-2xs uppercase tracking-wider text-ink-400">
              Suggestions
            </div>
            <ul className="list-disc space-y-1 pl-5 leading-relaxed text-ink-100">
              {finding.Suggestions.map((s, i) => (
                <li key={i}>{s}</li>
              ))}
            </ul>
          </div>
        )}

        {finding?.next_steps && finding.next_steps.length > 0 && (
          <div>
            <div className="mb-1.5 text-2xs uppercase tracking-wider text-ink-400">
              Next steps
            </div>
            <ol className="list-decimal space-y-1 pl-5 leading-relaxed text-ink-100">
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
                  to={`/agent/logs/${pid}`}
                  className="font-mono text-2xs text-link hover:underline"
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
            <pre className="mt-2 max-h-64 overflow-auto whitespace-pre-wrap break-words rounded-md bg-surface-sunken p-2 font-mono text-2xs leading-snug text-ink-100">
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
                  className="rounded-md border border-ink-600 bg-surface-raised p-2 text-2xs"
                >
                  <div className="flex items-center justify-between">
                    <span className="font-mono text-ink-50">{tc.name}</span>
                    {tc.duration_ms !== undefined && (
                      <span className="text-ink-300">
                        {formatDuration(tc.duration_ms)}
                      </span>
                    )}
                  </div>
                  {tc.error && (
                    <p className="mt-1 text-sev-critical">error: {tc.error}</p>
                  )}
                  {tc.args !== undefined && tc.args !== null && (
                    <pre className="mt-1 max-h-32 overflow-auto whitespace-pre-wrap break-words font-mono text-ink-200">
                      args: {jsonString(tc.args)}
                    </pre>
                  )}
                  {tc.output !== undefined && tc.output !== null && (
                    <pre className="mt-1 max-h-32 overflow-auto whitespace-pre-wrap break-words font-mono text-ink-200">
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

// confidencePct mirrors the incident detail page's tolerance for models
// that report confidence as 0..1 or already as a percentage.
function confidencePct(c: number): string {
  return (c * (c <= 1 ? 100 : 1)).toFixed(0);
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
              className="rounded-md border border-ink-600 bg-surface-raised p-2"
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
                    <span className="text-ink-100">{e.summary}</span>
                  </div>
                  {expanded && e.detail && (
                    <pre className="mt-2 max-h-48 overflow-auto whitespace-pre-wrap break-words font-mono text-2xs leading-snug text-ink-200">
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
