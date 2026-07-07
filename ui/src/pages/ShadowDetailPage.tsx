import { Link, useParams } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { ArrowLeft } from "lucide-react";
import { api, ApiError } from "@/lib/api";
import { fmtAbs, fmtRel } from "@/lib/format";
import { TopBar } from "@/components/TopBar";
import { Pill, VerdictPill } from "@/components/Pill";
import { SkCard } from "@/components/Skeleton";
import { RetryableError } from "@/components/RetryableError";

// ShadowDetailPage shows a single shadow event picked out of the list
// returned by /api/agent/shadow. The server doesn't expose a per-event
// GET, so we filter the cached list by pattern_id (latest match wins).
//
// Linked from the Decisions › Shadow tab and the agent overview's
// recent-shadow card.
export function ShadowDetailPage() {
  const { patternId = "" } = useParams<{ patternId: string }>();
  const id = decodeURIComponent(patternId);

  const events = useQuery({
    queryKey: ["shadow"],
    queryFn: api.listShadow,
  });
  const pattern = useQuery({
    queryKey: ["pattern", id],
    queryFn: () => api.getPattern(id),
    enabled: !!id,
    retry: 0,
  });

  const event = (events.data ?? []).find((e) => e.pattern_id === id);

  // The catalog entry is optional context — a 404 just means the pattern
  // was pruned from the catalog, which is normal, not an error to retry.
  const patternLoadFailed =
    pattern.isError &&
    !(pattern.error instanceof ApiError && pattern.error.status === 404);

  return (
    <>
      <TopBar
        title="Shadow event"
        subtitle={id}
        actions={
          <Link to="/agent/decisions?tab=shadow" className="btn">
            <ArrowLeft size={12} aria-hidden />
            Back
          </Link>
        }
      />

      <main className="flex-1 overflow-auto p-6">
        {events.isLoading && (
          <div className="grid gap-4 lg:grid-cols-[2fr,1fr]">
            <div className="min-w-0 space-y-4">
              <SkCard lines={3} />
              <SkCard lines={3} />
            </div>
            <div className="min-w-0 space-y-4">
              <SkCard lines={4} />
              <SkCard lines={3} />
            </div>
          </div>
        )}
        {events.isError && (
          <RetryableError
            error={events.error}
            onRetry={() => events.refetch()}
            retrying={events.isRefetching}
            context="Couldn't load shadow events"
          />
        )}

        {events.isSuccess && !event && (
          <div className="card">
            <div className="card-body py-12 text-center text-sm text-ink-300">
              No shadow event found for pattern{" "}
              <code className="font-mono text-ink-100">{id}</code>.
              <div className="mt-2 text-xs text-ink-400">
                The shadow log may have been cleared. Try{" "}
                <Link
                  to="/agent/decisions?tab=shadow"
                  className="text-link hover:underline"
                >
                  the shadow list
                </Link>
                .
              </div>
            </div>
          </div>
        )}

        {event && (
          <div className="grid gap-4 lg:grid-cols-[2fr,1fr]">
            <div className="min-w-0 space-y-4">
              <div className="card">
                <div className="card-header">
                  <span className="card-title">Sample message</span>
                </div>
                <div className="card-body">
                  <pre className="max-w-full overflow-auto whitespace-pre-wrap break-all rounded-md border border-ink-600 bg-surface-sunken p-3 font-mono text-xs leading-snug text-ink-100">
                    {event.sample_message}
                  </pre>
                </div>
              </div>

              <div className="card">
                <div className="card-header">
                  <span className="card-title">Template</span>
                </div>
                <div className="card-body">
                  <pre className="max-w-full overflow-auto whitespace-pre-wrap break-all rounded-md border border-ink-600 bg-surface-sunken p-3 font-mono text-xs leading-snug text-ink-100">
                    {event.template}
                  </pre>
                  <p className="mt-2 text-2xs text-ink-300">
                    The clustered template the miner extracted.
                    Variable parts are replaced with{" "}
                    <code className="rounded bg-ink-700 px-1">{"<*>"}</code>.
                  </p>
                </div>
              </div>

              {pattern.isLoading && <SkCard lines={4} />}
              {patternLoadFailed && (
                <RetryableError
                  error={pattern.error}
                  onRetry={() => pattern.refetch()}
                  retrying={pattern.isRefetching}
                  context="Couldn't load the catalog entry"
                />
              )}
              {pattern.data && (
                <div className="card">
                  <div className="card-header flex items-center justify-between">
                    <span className="card-title">Catalog entry</span>
                    <Link
                      to={`/agent/logs/${encodeURIComponent(event.pattern_id)}`}
                      className="text-2xs text-link hover:underline"
                    >
                      Open pattern →
                    </Link>
                  </div>
                  <div className="card-body grid grid-cols-2 gap-x-4 gap-y-3 text-xs">
                    <Fact
                      k="Total sightings"
                      v={
                        <span className="tabular-nums">
                          {pattern.data.count}
                        </span>
                      }
                    />
                    <Fact
                      k="Baseline rate"
                      v={
                        <span className="tabular-nums">
                          {pattern.data.baseline_frequency.toFixed(2)}/s
                        </span>
                      }
                    />
                    <Fact
                      k="Verdict"
                      v={<VerdictPill verdict={pattern.data.verdict} />}
                    />
                    <Fact
                      k="Service"
                      v={pattern.data.service || "—"}
                    />
                    <Fact
                      k="First seen"
                      v={
                        <span title={fmtAbs(pattern.data.first_seen)}>
                          {fmtRel(pattern.data.first_seen)}
                        </span>
                      }
                    />
                    <Fact
                      k="Last seen"
                      v={
                        <span title={fmtAbs(pattern.data.last_seen)}>
                          {fmtRel(pattern.data.last_seen)}
                        </span>
                      }
                    />
                    {pattern.data.tags && pattern.data.tags.length > 0 && (
                      <div className="col-span-2">
                        <div className="text-2xs uppercase tracking-wider text-ink-400">
                          Tags
                        </div>
                        <div className="mt-1 flex flex-wrap gap-1">
                          {pattern.data.tags.map((t) => (
                            <Pill key={t}>{t}</Pill>
                          ))}
                        </div>
                      </div>
                    )}
                  </div>
                </div>
              )}
            </div>

            <div className="min-w-0 space-y-4">
              <div className="card">
                <div className="card-header">
                  <span className="card-title">Facts</span>
                </div>
                <div className="card-body grid grid-cols-2 gap-x-4 gap-y-3 text-xs">
                  <Fact
                    k="Verdict"
                    v={<VerdictPill verdict={event.verdict} />}
                  />
                  <Fact k="Source" v={event.source} />
                  <Fact k="Rule" v={event.rule_name || "—"} />
                  <Fact
                    k="Pattern ID"
                    v={
                      <span className="font-mono text-2xs">
                        {event.pattern_id}
                      </span>
                    }
                  />
                  <Fact
                    k="Signals"
                    v={
                      <span className="tabular-nums">{event.count}</span>
                    }
                  />
                  <Fact
                    k="Ticks"
                    v={
                      <span className="tabular-nums">
                        {event.occurrences}
                      </span>
                    }
                  />
                  <Fact
                    k="First seen"
                    v={
                      <span title={fmtAbs(event.first_seen)}>
                        {fmtRel(event.first_seen)}
                      </span>
                    }
                  />
                  <Fact
                    k="Last seen"
                    v={
                      <span title={fmtAbs(event.last_seen)}>
                        {fmtRel(event.last_seen)}
                      </span>
                    }
                  />
                </div>
              </div>

              <div className="card">
                <div className="card-header">
                  <span className="card-title">What this means</span>
                </div>
                <div className="card-body space-y-2 text-xs leading-relaxed text-ink-300">
                  {event.verdict === "spike" && (
                    <p>
                      A previously-known pattern's tick frequency
                      exceeded the EWMA baseline by{" "}
                      <code className="rounded bg-ink-700 px-1">
                        spike_multiplier
                      </code>
                      . In detect mode this would have been escalated.
                    </p>
                  )}
                  {event.verdict === "unknown" && (
                    <p>
                      The agent has not seen this template enough times
                      (or has not been told it's known). In detect mode
                      this would have generated an incident.
                    </p>
                  )}
                  {event.verdict !== "spike" &&
                    event.verdict !== "unknown" && (
                      <p>Operator-set verdict: {event.verdict}.</p>
                    )}
                </div>
              </div>
            </div>
          </div>
        )}
      </main>
    </>
  );
}

function Fact({ k, v }: { k: string; v: React.ReactNode }) {
  return (
    <div>
      <div className="text-2xs uppercase tracking-wider text-ink-400">
        {k}
      </div>
      <div className="text-ink-100">{v}</div>
    </div>
  );
}
