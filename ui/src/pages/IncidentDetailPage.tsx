import { Link, useParams } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { ArrowLeft } from "lucide-react";
import { api } from "@/lib/api";
import { fmtAbs, fmtRel } from "@/lib/format";
import { TopBar } from "@/components/TopBar";
import { Pill } from "@/components/Pill";
import { ErrorBox, Spinner } from "@/components/feedback";

// IncidentDetailPage shows the full persisted record including the raw
// content payload that drove the alert templates.
export function IncidentDetailPage() {
  const { id = "" } = useParams<{ id: string }>();
  const { data, isLoading, isError, error } = useQuery({
    queryKey: ["incident", id],
    queryFn: () => api.getIncident(id),
    enabled: !!id,
  });

  return (
    <>
      <TopBar
        title="Incident"
        subtitle={data?.title || data?.id?.slice(0, 8)}
        actions={
          <Link to="/incidents" className="btn">
            <ArrowLeft size={12} />
            Back
          </Link>
        }
      />

      <main className="flex-1 overflow-auto p-6">
        {isLoading && <Spinner />}
        {isError && <ErrorBox error={error} />}

        {data && (
          <div className="grid gap-4 lg:grid-cols-[2fr,1fr]">
            <div className="card">
              <div className="card-header">
                <span className="card-title">Payload</span>
              </div>
              <div className="card-body">
                <pre className="overflow-auto rounded-md bg-ink-50 p-3 font-mono text-xs leading-snug text-ink-800">
                  {JSON.stringify(data.content ?? {}, null, 2)}
                </pre>
              </div>
            </div>

            <div className="space-y-4">
              <div className="card">
                <div className="card-header">
                  <span className="card-title">Facts</span>
                </div>
                <div className="card-body grid grid-cols-2 gap-x-4 gap-y-3 text-xs">
                  <Fact k="ID" v={<span className="font-mono">{data.id}</span>} />
                  <Fact k="Service" v={data.service || "—"} />
                  <Fact k="Source" v={data.source || "—"} />
                  <Fact k="Team" v={data.team_id || "—"} />
                  <Fact
                    k="Created"
                    v={
                      <span title={fmtAbs(data.created_at)}>
                        {fmtRel(data.created_at)}
                      </span>
                    }
                  />
                  <Fact
                    k="Acked"
                    v={
                      data.acked_at ? (
                        <span title={fmtAbs(data.acked_at)}>
                          {fmtRel(data.acked_at)}
                        </span>
                      ) : (
                        "—"
                      )
                    }
                  />
                  <Fact
                    k="Resolved"
                    v={data.resolved ? "yes" : "no"}
                  />
                  <Fact
                    k="On-call"
                    v={data.oncall_triggered ? "triggered" : "—"}
                  />
                  <Fact
                    k="Notify"
                    v={
                      data.notify_status === "sent" ? (
                        <Pill tone="good">sent</Pill>
                      ) : data.notify_status === "failed" ? (
                        <span title={data.notify_error}>
                          <Pill tone="bad">failed</Pill>
                        </span>
                      ) : data.notify_status ? (
                        <Pill tone="accent">{data.notify_status}</Pill>
                      ) : (
                        "—"
                      )
                    }
                  />
                  {data.notify_status === "failed" && data.notify_error && (
                    <Fact
                      k="Notify error"
                      v={
                        <span className="break-all font-mono text-2xs text-rose-700">
                          {data.notify_error}
                        </span>
                      }
                    />
                  )}
                </div>
              </div>

              <div className="card">
                <div className="card-header">
                  <span className="card-title">Channels notified</span>
                </div>
                <div className="card-body flex flex-wrap gap-1.5">
                  {(data.channels_notified ?? []).length === 0 && (
                    <span className="text-xs text-ink-400">
                      None enabled at the time.
                    </span>
                  )}
                  {(data.channels_notified ?? []).map((c) => (
                    <Pill key={c} tone="accent">
                      {c}
                    </Pill>
                  ))}
                </div>
              </div>

              <div className="card">
                <div className="card-header">
                  <span className="card-title">Status</span>
                </div>
                <div className="card-body text-xs">
                  {data.resolved && (
                    <Pill tone="good">resolved</Pill>
                  )}
                  {!data.resolved && data.acked_at && (
                    <Pill tone="accent">acknowledged</Pill>
                  )}
                  {!data.resolved && !data.acked_at && (
                    <Pill tone="bad">open</Pill>
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
      <div className="text-2xs uppercase tracking-wider text-ink-400">{k}</div>
      <div className="text-ink-800">{v}</div>
    </div>
  );
}
