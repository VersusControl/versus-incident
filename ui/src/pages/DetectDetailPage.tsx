import { Link, useParams } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { ArrowLeft, ExternalLink } from "lucide-react";
import { api, type AIFinding } from "@/lib/api";
import { fmtAbs } from "@/lib/format";
import { TopBar } from "@/components/TopBar";
import { Pill, VerdictPill } from "@/components/Pill";
import { ErrorBox, Spinner } from "@/components/feedback";
import { OutcomePill, SeverityPill } from "./DetectPage";

export function DetectDetailPage() {
  const { id = "" } = useParams();
  const event = useQuery({
    queryKey: ["detect", id],
    queryFn: () => api.getDetect(id),
    enabled: !!id,
  });

  return (
    <>
      <TopBar
        title="Detect event"
        subtitle={id}
        actions={
          <Link to="/detect" className="btn">
            <ArrowLeft size={12} /> Back
          </Link>
        }
      />

      <main className="flex-1 overflow-auto p-6">
        {event.isLoading && <Spinner />}
        {event.isError && <ErrorBox error={event.error} />}
        {event.data && (
          <div className="space-y-4">
            {/* Summary card */}
            <div className="card">
              <div className="card-header">
                <div className="card-title">Summary</div>
              </div>
              <div className="card-body">
                <dl className="grid grid-cols-2 gap-x-6 gap-y-2 text-xs md:grid-cols-4">
                  <Field label="When" value={fmtAbs(event.data.timestamp)} />
                  <Field
                    label="Outcome"
                    valueNode={<OutcomePill outcome={event.data.outcome} />}
                  />
                  <Field
                    label="Verdict"
                    valueNode={<VerdictPill verdict={event.data.verdict} />}
                  />
                  <Field
                    label="Severity"
                    valueNode={
                      <SeverityPill severity={event.data.finding?.Severity} />
                    }
                  />
                  <Field label="Source" value={event.data.source} />
                  <Field label="Service" value={event.data.service || "—"} />
                  <Field label="Frequency" value={String(event.data.frequency)} />
                  <Field
                    label="Baseline"
                    value={event.data.baseline.toFixed(2)}
                  />
                  <Field label="Model" value={event.data.model || "—"} />
                  <Field
                    label="Duration"
                    value={
                      event.data.duration_ms != null
                        ? `${event.data.duration_ms} ms`
                        : "—"
                    }
                  />
                  <Field
                    label="Pattern"
                    valueNode={
                      <Link
                        to={`/patterns/${encodeURIComponent(event.data.pattern_id)}`}
                        className="font-mono text-2xs text-accent hover:underline"
                      >
                        {event.data.pattern_id}{" "}
                        <ExternalLink size={10} className="inline" />
                      </Link>
                    }
                  />
                  <Field
                    label="Confidence"
                    value={
                      event.data.finding?.Confidence != null
                        ? event.data.finding.Confidence.toFixed(2)
                        : "—"
                    }
                  />
                </dl>
                {event.data.error && (
                  <div className="mt-3 rounded-md border border-bad/40 bg-bad/5 px-3 py-2 text-xs text-bad">
                    <span className="font-medium">Error:</span> {event.data.error}
                  </div>
                )}
              </div>
            </div>

            {/* Pattern template */}
            <Card title="Pattern template">
              <pre className="whitespace-pre-wrap break-words font-mono text-2xs text-ink-700">
                {event.data.template || "—"}
              </pre>
            </Card>

            {/* Samples */}
            {event.data.samples && event.data.samples.length > 0 && (
              <Card title={`Samples (${event.data.samples.length})`}>
                <ul className="space-y-2">
                  {event.data.samples.map((s, i) => (
                    <li
                      key={i}
                      className="rounded-md border border-ink-100 bg-ink-50/40 p-2 font-mono text-2xs text-ink-700"
                    >
                      {s}
                    </li>
                  ))}
                </ul>
              </Card>
            )}

            {/* Finding */}
            {event.data.finding && (
              <Card title="AI Finding">
                <FindingBlock f={event.data.finding} />
              </Card>
            )}

            {/* Full prompt: system + user, exactly as the model receives it */}
            <Card
              title="Prompt"
              subtitle="Full system + user prompt sent to the model on this call."
            >
              {event.data.user_prompt ? (
                <FullPrompt userPrompt={event.data.user_prompt} />
              ) : (
                <EmptyHint text="No prompt recorded for this outcome (cache hit, dry run, or quota skip)." />
              )}
            </Card>

            {/* Raw response */}
            <Card
              title="Raw response"
              subtitle="Verbatim model output before JSON parsing."
            >
              {event.data.raw_response ? (
                <pre className="max-h-96 overflow-auto whitespace-pre-wrap break-words rounded-md bg-ink-900/95 p-3 font-mono text-2xs text-ink-100">
                  {event.data.raw_response}
                </pre>
              ) : (
                <EmptyHint text="No model response recorded for this outcome." />
              )}
            </Card>
          </div>
        )}
      </main>
    </>
  );
}

function Card({
  title,
  subtitle,
  children,
}: {
  title: string;
  subtitle?: string;
  children: React.ReactNode;
}) {
  return (
    <div className="card">
      <div className="card-header">
        <div>
          <div className="card-title">{title}</div>
          {subtitle && <div className="text-2xs text-ink-500">{subtitle}</div>}
        </div>
      </div>
      <div className="card-body">{children}</div>
    </div>
  );
}

function Field({
  label,
  value,
  valueNode,
}: {
  label: string;
  value?: string;
  valueNode?: React.ReactNode;
}) {
  return (
    <div>
      <dt className="text-2xs uppercase tracking-wider text-ink-500">{label}</dt>
      <dd className="mt-0.5 text-xs text-ink-800">{valueNode ?? value ?? "—"}</dd>
    </div>
  );
}

function FindingBlock({ f }: { f: AIFinding }) {
  if (!f) return <EmptyHint text="No finding parsed." />;
  return (
    <div className="space-y-3 text-xs">
      {f.Title && (
        <div>
          <div className="text-2xs uppercase tracking-wider text-ink-500">
            Title
          </div>
          <div className="mt-0.5 text-sm font-medium text-ink-900">
            {f.Title}
          </div>
        </div>
      )}
      {f.Summary && (
        <div>
          <div className="text-2xs uppercase tracking-wider text-ink-500">
            Summary
          </div>
          <p className="mt-0.5 whitespace-pre-wrap text-ink-700">{f.Summary}</p>
        </div>
      )}
      <div className="flex flex-wrap gap-2">
        {f.Category && <Pill tone="accent">{f.Category}</Pill>}
        {f.SampleIDs?.map((s) => (
          <Pill key={s}>{s}</Pill>
        ))}
      </div>
      {f.Suggestions && f.Suggestions.length > 0 && (
        <div>
          <div className="text-2xs uppercase tracking-wider text-ink-500">
            Suggestions
          </div>
          <ol className="mt-1 list-decimal space-y-1 pl-5 text-ink-700">
            {f.Suggestions.map((s, i) => (
              <li key={i}>{s}</li>
            ))}
          </ol>
        </div>
      )}
    </div>
  );
}

function EmptyHint({ text }: { text: string }) {
  return <div className="text-2xs italic text-ink-500">{text}</div>;
}

// FullPrompt fetches the constant system prompt once and shows it
// concatenated with the per-call user prompt so the operator sees the
// exact payload delivered to the model.
function FullPrompt({ userPrompt }: { userPrompt: string }) {
  const sys = useQuery({
    queryKey: ["system-prompt"],
    queryFn: api.getSystemPrompt,
    staleTime: 60_000,
    refetchInterval: false,
  });

  return (
    <div className="space-y-2">
      <div>
        <div className="mb-1 flex items-center justify-between text-2xs uppercase tracking-wider text-ink-500">
          <span>System</span>
          {sys.isLoading && <span className="italic text-ink-400">loading…</span>}
          {sys.isError && (
            <span className="italic text-bad">failed to load system prompt</span>
          )}
        </div>
        <pre className="max-h-72 overflow-auto whitespace-pre-wrap break-words rounded-md bg-ink-900/95 p-3 font-mono text-2xs text-ink-100">
          {sys.data ?? ""}
        </pre>
      </div>
      <div>
        <div className="mb-1 text-2xs uppercase tracking-wider text-ink-500">
          User
        </div>
        <pre className="max-h-96 overflow-auto whitespace-pre-wrap break-words rounded-md bg-ink-900/95 p-3 font-mono text-2xs text-ink-100">
          {userPrompt}
        </pre>
      </div>
    </div>
  );
}
