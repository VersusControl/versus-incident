import { useQuery } from "@tanstack/react-query";
import { ChannelIcon } from "@/components/ChannelIcon";
import { RetryableError } from "@/components/RetryableError";
import { SkCard } from "@/components/Skeleton";
import {
  EnablePill,
  KV,
  KVGrid,
  ReadOnlyCard,
  SecretField,
  SubCard,
} from "@/components/settings/ReadOnlyValues";
import { api, type ConfigField } from "@/lib/api";

export type IncidentsConfigPart = "server" | "channels" | "queues" | "oncall";

// Read-only view of the live server / alert / queue / on-call configuration,
// one part per Settings section. Secret-bearing fields (tokens, webhook URLs,
// SMTP password, etc.) render only as configured / not set — their values are
// never sent to the browser.
export function IncidentsConfigSection({ part }: { part: IncidentsConfigPart }) {
  const cfg = useQuery({
    queryKey: ["config-incidents"],
    queryFn: api.getIncidentsConfig,
  });

  if (cfg.isLoading) {
    return <SkCard lines={4} />;
  }

  if (cfg.isError) {
    return (
      <RetryableError
        error={cfg.error}
        onRetry={() => cfg.refetch()}
        retrying={cfg.isRefetching}
        context="Couldn't load incidents configuration"
      />
    );
  }

  if (!cfg.data) return null;
  const data = cfg.data;

  if (part === "server") {
    return (
      <ReadOnlyCard>
        <KVGrid>
          <KV k="Name" v={data.name || "—"} />
          <KV k="Listen" v={`${data.host}:${data.port}`} />
          <KV k="Public host" v={data.public_host || "—"} />
          <KV k="Storage type" v={data.storage.type || "file"} />
          <KV k="Max incidents" v={String(data.storage.file.max_incidents || 0)} />
        </KVGrid>
      </ReadOnlyCard>
    );
  }

  if (part === "channels") {
    return (
      <div className="space-y-3">
        <p className="text-2xs text-ink-400">
          Debug body: <code>{String(data.alert.debug_body)}</code>
        </p>
        {data.alert.channels.map((ch) => (
          <ChannelCard key={ch.id} channel={ch} />
        ))}
      </div>
    );
  }

  if (part === "queues") {
    return (
      <div className="space-y-3">
        <p className="text-2xs text-ink-400">
          Top-level enable: <code>{String(data.queue.enable)}</code>
        </p>
        {data.queue.providers.length === 0 ? (
          <p className="text-xs text-ink-400">No queue listeners are configured.</p>
        ) : (
          data.queue.providers.map((p) => <ProviderRow key={p.id} provider={p} />)
        )}
      </div>
    );
  }

  const oncall = data.oncall;
  return (
    <ReadOnlyCard title="Escalation" aside={<EnablePill enabled={oncall.enable} />}>
      <KVGrid>
        <KV k="Provider" v={oncall.provider || "—"} />
        <KV k="Wait minutes" v={String(oncall.wait_minutes)} />
        <KV k="Initialized only" v={String(oncall.initialized_only)} />
      </KVGrid>

      <SubCard title="AWS Incident Manager">
        <KVGrid>
          <SecretField
            k="Response plan ARN"
            configured={!!oncall.aws_incident_manager.response_plan_arn}
          />
          <KV
            k="Other plan keys"
            v={listOrDash(oncall.aws_incident_manager.other_response_plan_keys)}
          />
        </KVGrid>
      </SubCard>

      <SubCard title="PagerDuty">
        <KVGrid>
          <SecretField k="Routing key" configured={!!oncall.pagerduty.routing_key} />
          <KV k="Other routing keys" v={listOrDash(oncall.pagerduty.other_routing_keys)} />
        </KVGrid>
      </SubCard>

      <SubCard title="ServiceNow">
        <KVGrid>
          <SecretField k="Instance URL" configured={!!oncall.servicenow.instance_url} />
          <SecretField k="Username" configured={!!oncall.servicenow.username} />
          <KV k="Table" v={oncall.servicenow.table || "incident"} />
          <KV k="Other instance keys" v={listOrDash(oncall.servicenow.other_instance_keys)} />
        </KVGrid>
      </SubCard>

      <SubCard title="incident.io">
        <KVGrid>
          <SecretField k="API key" configured={!!oncall.incident_io.api_key} />
          <SecretField
            k="Alert source config ID"
            configured={!!oncall.incident_io.alert_source_config_id}
          />
          <KV
            k="Other alert source keys"
            v={listOrDash(oncall.incident_io.other_alert_source_config_keys)}
          />
        </KVGrid>
      </SubCard>
    </ReadOnlyCard>
  );
}

function listOrDash(values: string[]): string {
  return values.length === 0 ? "—" : values.join(", ");
}


function ChannelCard({
  channel,
}: {
  channel: { id: string; name: string; enable: boolean; fields: ConfigField[] };
}) {
  return (
    <div className="rounded-md border border-ink-600 bg-surface">
      <div className="flex items-center justify-between border-b border-ink-600 px-3 py-2">
        <div className="flex items-center gap-2">
          <ChannelIcon id={channel.id} />
          <div className="text-sm font-medium text-ink-50">{channel.name}</div>
        </div>
        <EnablePill enabled={channel.enable} />
      </div>
      <div className="px-3 py-2">
        <KVGrid>
          {channel.fields
            .filter((f) => f.label !== "Template")
            .map((f) => (
              <FieldRow key={f.label} field={f} />
            ))}
        </KVGrid>
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
    <div className="rounded-md border border-ink-600 bg-surface">
      <div className="flex items-center justify-between border-b border-ink-600 px-3 py-2">
        <div className="text-sm font-medium text-ink-50">{provider.name}</div>
        <EnablePill enabled={provider.enable} />
      </div>
      <div className="px-3 py-2">
        <KVGrid>
          {provider.fields.map((f) => (
            <FieldRow key={f.label} field={f} />
          ))}
        </KVGrid>
      </div>
    </div>
  );
}

function FieldRow({ field }: { field: ConfigField }) {
  if (field.secret) {
    return <SecretField k={field.label} configured={field.value === "set"} />;
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
