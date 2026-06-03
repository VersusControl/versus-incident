import { useQuery } from "@tanstack/react-query";
import { Lock, ExternalLink } from "lucide-react";
import { TopBar } from "@/components/TopBar";
import { ErrorBox, Spinner } from "@/components/feedback";
import { ChannelIcon } from "@/components/ChannelIcon";
import { api, type ConfigField } from "@/lib/api";

// Read-only view of the live alert / queue / on-call configuration. All
// secret-bearing fields (tokens, webhook URLs, SMTP password, etc.) are
// rendered as a "configured" pill — the actual value is never sent to
// the browser. Operators wanting to inspect a value must look at
// `config/config.yaml` or the relevant environment variable on the host.
export function IncidentsConfigPage() {
  const cfg = useQuery({
    queryKey: ["config-incidents"],
    queryFn: api.getIncidentsConfig,
  });

  return (
    <>
      <TopBar
        title="Incidents · Configuration"
        subtitle="Read-only view of the running alert config."
      />
      <main className="flex-1 overflow-auto p-6">
        {cfg.isLoading && <Spinner />}
        {cfg.isError && <ErrorBox error={cfg.error} />}
        {cfg.data && (
          <div className="space-y-6">
            <SecretBanner />

            <div className="card">
              <div className="card-header">
                <h2 className="card-title">Server</h2>
              </div>
              <div className="card-body">
                <Grid>
                  <KV k="Name" v={cfg.data.name || "—"} />
                  <KV k="Listen" v={`${cfg.data.host}:${cfg.data.port}`} />
                  <KV k="Public host" v={cfg.data.public_host || "—"} />
                  <KV k="Storage type" v={cfg.data.storage.type || "file"} />
                  <KV k="Data dir" v={cfg.data.storage.file.data_dir || "—"} />
                  <KV
                    k="Max incidents"
                    v={String(cfg.data.storage.file.max_incidents || 0)}
                  />
                </Grid>
              </div>
            </div>

            <div className="card">
              <div className="card-header">
                <h2 className="card-title">Alert channels</h2>
                <span className="text-xs text-ink-400">
                  Debug body:{" "}
                  <code>{String(cfg.data.alert.debug_body)}</code>
                </span>
              </div>
              <div className="card-body space-y-3">
                {cfg.data.alert.channels.map((ch) => (
                  <ChannelCard key={ch.id} channel={ch} />
                ))}
              </div>
            </div>

            <div className="card">
              <div className="card-header">
                <h2 className="card-title">Queue listeners</h2>
                <span className="text-xs text-ink-400">
                  Top-level enable:{" "}
                  <code>{String(cfg.data.queue.enable)}</code>
                </span>
              </div>
              <div className="card-body space-y-3">
                {cfg.data.queue.providers.map((p) => (
                  <ProviderRow key={p.id} provider={p} />
                ))}
              </div>
            </div>

            <div className="card">
              <div className="card-header">
                <h2 className="card-title">On-call</h2>
                <EnablePill enabled={cfg.data.oncall.enable} />
              </div>
              <div className="card-body space-y-3">
                <Grid>
                  <KV k="Provider" v={cfg.data.oncall.provider || "—"} />
                  <KV
                    k="Wait minutes"
                    v={String(cfg.data.oncall.wait_minutes)}
                  />
                  <KV
                    k="Initialized only"
                    v={String(cfg.data.oncall.initialized_only)}
                  />
                </Grid>

                <SubCard title="AWS Incident Manager">
                  <Grid>
                    <SecretField
                      k="Response plan ARN"
                      configured={
                        !!cfg.data.oncall.aws_incident_manager.response_plan_arn
                      }
                    />
                    <KV
                      k="Other plan keys"
                      v={
                        cfg.data.oncall.aws_incident_manager
                          .other_response_plan_keys.length === 0
                          ? "—"
                          : cfg.data.oncall.aws_incident_manager.other_response_plan_keys.join(
                              ", ",
                            )
                      }
                    />
                  </Grid>
                </SubCard>

                <SubCard title="PagerDuty">
                  <Grid>
                    <SecretField
                      k="Routing key"
                      configured={!!cfg.data.oncall.pagerduty.routing_key}
                    />
                    <KV
                      k="Other routing keys"
                      v={
                        cfg.data.oncall.pagerduty.other_routing_keys.length ===
                        0
                          ? "—"
                          : cfg.data.oncall.pagerduty.other_routing_keys.join(
                              ", ",
                            )
                      }
                    />
                  </Grid>
                </SubCard>
              </div>
            </div>
          </div>
        )}
      </main>
    </>
  );
}

function ChannelCard({
  channel,
}: {
  channel: { id: string; name: string; enable: boolean; fields: ConfigField[] };
}) {
  return (
    <div className="rounded-md border border-ink-100 bg-white">
      <div className="flex items-center justify-between border-b border-ink-100 px-3 py-2">
        <div className="flex items-center gap-2">
          <ChannelIcon id={channel.id} />
          <div className="text-sm font-medium text-ink-900">
            {channel.name}
          </div>
        </div>
        <EnablePill enabled={channel.enable} />
      </div>
      <div className="px-3 py-2">
        <Grid>
          {channel.fields.map((f) => (
            <FieldRow key={f.label} field={f} />
          ))}
        </Grid>
      </div>
    </div>
  );
}

function ProviderRow({
  provider,
}: {
  provider: { id: string; name: string; enable: boolean; fields: ConfigField[] };
}) {
  return (
    <div className="rounded-md border border-ink-100 bg-white">
      <div className="flex items-center justify-between border-b border-ink-100 px-3 py-2">
        <div className="text-sm font-medium text-ink-900">{provider.name}</div>
        <EnablePill enabled={provider.enable} />
      </div>
      <div className="px-3 py-2">
        <Grid>
          {provider.fields.map((f) => (
            <FieldRow key={f.label} field={f} />
          ))}
        </Grid>
      </div>
    </div>
  );
}

function FieldRow({ field }: { field: ConfigField }) {
  if (field.secret) {
    return (
      <SecretField k={field.label} configured={field.value === "set"} />
    );
  }
  let display: string;
  if (Array.isArray(field.value)) {
    display = field.value.length === 0 ? "—" : field.value.join(", ");
  } else if (field.value === "" || field.value == null) {
    display = "—";
  } else {
    display = String(field.value);
  }
  return <KV k={field.label} v={display} />;
}

function SecretField({
  k,
  configured,
}: {
  k: string;
  configured: boolean;
}) {
  return (
    <div>
      <div className="text-2xs uppercase tracking-wider text-ink-400">{k}</div>
      <div className="mt-0.5 flex items-center gap-1.5 text-xs">
        <Lock size={11} className="text-ink-400" />
        {configured ? (
          <span className="pill pill-good">Configured</span>
        ) : (
          <span className="pill">Not set</span>
        )}
      </div>
    </div>
  );
}

function KV({ k, v }: { k: string; v: string }) {
  return (
    <div>
      <div className="text-2xs uppercase tracking-wider text-ink-400">{k}</div>
      <div className="mt-0.5 break-words font-mono text-xs text-ink-800">
        {v}
      </div>
    </div>
  );
}

function Grid({ children }: { children: React.ReactNode }) {
  return (
    <div className="grid grid-cols-1 gap-x-6 gap-y-3 sm:grid-cols-2 lg:grid-cols-3">
      {children}
    </div>
  );
}

function SubCard({
  title,
  children,
}: {
  title: string;
  children: React.ReactNode;
}) {
  return (
    <div className="rounded-md border border-ink-100 bg-ink-50/40 px-3 py-2">
      <div className="mb-2 text-xs font-medium text-ink-700">{title}</div>
      {children}
    </div>
  );
}

function EnablePill({ enabled }: { enabled: boolean }) {
  return (
    <span className={`pill ${enabled ? "pill-good" : ""}`}>
      {enabled ? "Enabled" : "Disabled"}
    </span>
  );
}

function SecretBanner() {
  return (
    <div className="flex items-start gap-2 rounded-md border border-ink-100 bg-ink-50 px-3 py-2 text-xs text-ink-600">
      <Lock size={13} className="mt-0.5 shrink-0 text-ink-400" />
      <div>
        Read-only view. Tokens, webhook URLs, and other secrets are{" "}
        <strong>never sent to the browser</strong> — only their presence is
        shown. To change any value edit{" "}
        <code className="rounded bg-white px-1 py-0.5 font-mono">
          config/config.yaml
        </code>{" "}
        or the corresponding environment variable on the host.
        <a
          href="https://versuscontrol.github.io/versus-incident/configuration/configuration.html"
          target="_blank"
          rel="noreferrer"
          className="ml-2 inline-flex items-center gap-1 text-accent hover:underline"
        >
          Configuration reference <ExternalLink size={11} />
        </a>
      </div>
    </div>
  );
}
