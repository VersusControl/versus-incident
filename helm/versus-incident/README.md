# Versus Incident Helm Chart

This Helm chart deploys Versus Incident, a robust incident management tool that supports alerting across multiple channels with easy custom messaging and on-call integrations.

The agent's read-only Kubernetes connector supports native in-cluster, token,
client-certificate, safe kubeconfig, EKS IAM, AKS Entra, and closed GKE
service-account, file-workload-identity, or metadata-workload-identity
authentication. It does not require or execute cloud CLIs or auth plugins. See
[Kubernetes Connector](https://docs.versusincident.com/#/agent/connectors/kubernetes)
for complete values and cloud prerequisites.

## Requirements

- Kubernetes 1.19+
- Helm 3.8.0+ (installs from the OCI registry)
- PV provisioner support in the underlying infrastructure (if persistence is required for Redis)

## Installing the Chart

You can install the Versus Incident Helm chart using OCI registry:

```bash
helm install versus-incident oci://ghcr.io/versuscontrol/charts/versus-incident
```

### Install with Custom Values

```bash
# Install with custom configuration from a values file
helm install \
  versus-incident \
  oci://ghcr.io/versuscontrol/charts/versus-incident \
  -f values.yaml
```

### Upgrading an Existing Installation

```bash
# Upgrade an existing installation with the latest version
helm upgrade \
  versus-incident \
  oci://ghcr.io/versuscontrol/charts/versus-incident

# Upgrade with custom values
helm upgrade \
  versus-incident \
  oci://ghcr.io/versuscontrol/charts/versus-incident \
  -f values.yaml
```

## Configuration

### High Availability / multi-instance (enterprise)

> HA runs the **enterprise** image with a `LICENSE_KEY`. The OSS single-instance
> path (`ha.enabled: false`, the default) is unchanged.

Setting `ha.enabled: true` switches the workload from a **Deployment** to a
**StatefulSet** so each replica gets a stable pod name. The binary reads its
instance index from the `POD_NAME` trailing ordinal (Downward API) and
`INSTANCE_COUNT` from `replicaCount`, then partitions signal sources and the SLO
scheduler job by hash-ownership — so multiple replicas never double-page. All
replicas share **one Postgres** (derived automatically — the binary refuses the
`file` backend with more than one replica) and converge their managed secrets
(session key, AI master key, admin-token hash) through it, so a session cookie or
the break-glass admin token minted on one replica validates on all.

The chart renders, alongside the StatefulSet: a **headless Service** (stable
per-pod DNS), a **PodDisruptionBudget**, a **NetworkPolicy**, **pod
anti-affinity** to spread replicas across nodes, a hardened **securityContext**
(non-root, read-only root filesystem, dropped capabilities), and **readiness +
liveness** probes against `/healthz` for graceful rollout.

```bash
helm upgrade --install versus \
  oci://ghcr.io/versuscontrol/charts/versus-incident \
  -f values-ha.yaml \
  --set enterprise.licenseKey="$LICENSE_KEY" \
  --set storage.postgres.dsn="postgres://versus:PASS@my-rds:5432/versus?sslmode=require" \
  --set gatewaySecret="$(openssl rand -hex 32)"
```

For production / GitOps, reference existing Secrets instead of inline values:

```bash
  --set enterprise.existingSecret=versus-license      # key: license_key
  --set storage.postgres.dsnExistingSecret=versus-pg  # key: postgres_dsn
```

See [`values-ha.yaml`](values-ha.yaml) for the full HA value set, and the raw
(non-Helm) manifests and a Docker Compose walkthrough under
`versus-enterprise/examples/high-availability/`.

| Key | Default | Description |
|---|---|---|
| `ha.enabled` | `false` | Render the StatefulSet HA path (enterprise). |
| `replicaCount` | `1` (`3` in `values-ha.yaml`) | Replicas; becomes `INSTANCE_COUNT`. |
| `ha.podManagementPolicy` | `Parallel` | StatefulSet pod start/stop ordering. |
| `ha.updateStrategy.type` | `RollingUpdate` | StatefulSet update strategy. |
| `ha.antiAffinity.enabled` / `.hard` | `true` / `false` | Spread replicas across nodes (soft by default). |
| `ha.podDisruptionBudget.enabled` | `true` | Keep the LB backed during node drains. |
| `ha.networkPolicy.enabled` | `true` | Default-deny ingress; egress allow-all (narrow in prod). |
| `enterprise.licenseKey` / `.existingSecret` | `""` | Enterprise license (required for HA). |
| `storage.postgres.dsn` / `.dsnExistingSecret` | `""` | Shared Postgres DSN (required for postgres). |

### Quick Start Example

```yaml
# values.yaml
replicaCount: 1

# Proxy configuration (for networks that block messaging services)
proxy:
  url: "http://proxy.example.com:8080"
  username: "proxy-user"     # optional
  password: "proxy-pass"     # optional

alert:
  slack:
    enable: true
    token: "xoxb-your-slack-token"
    channelId: "C12345"
    messageProperties:
      buttonText: "Acknowledge Alert"
      buttonStyle: "primary"
  
  telegram:
    enable: false
    botToken: "your-telegram-bot-token"
    chatId: "your-chat-id"
    useProxy: false 
  
  viber:
    enable: false
    botToken: "your-viber-token"
    channelId: "your-channel-id"
    useProxy: false
  
  lark:
    enable: false
    webhookUrl: "your-lark-webhook-url"
    useProxy: false
  
  email:
    enable: false
  
  msteams:
    enable: false
```

### Important Parameters

| Parameter | Description | Default |
|-----------|-------------|---------|
| `replicaCount` | Number of replicas. More than one needs `storage.type=postgres`; the install fails on the file backend. | `1` |
| `config.publicHost` | Canonical external HTTP(S) origin for links, secure cookies, and exact-origin CSRF behind rewriting or TLS-terminating proxies | `""` |
| `gatewaySecret` | Sign-in secret for the UI and `/api/admin/*`, `/api/agent/*`. Empty generates a random 32-byte secret on first install and keeps it on upgrades; the install notes print how to read it. | `""` |
| `gatewaySecretExistingSecret` / `gatewaySecretExistingSecretKey` | Read the sign-in secret from a Secret you create; the app refuses to start until it exists. | `""` / `gateway_secret` |
| `connectors.kubernetes.enabled` | Kubernetes connector on or off. On by default with `auth.mode: in_cluster`. | `true` |
| `kubernetesReaderRBAC.enabled` | Empty creates the read-only reader ClusterRole when the connector uses `in_cluster`. `false` with `in_cluster` fails the install unless `external: true`. | empty |
| `kubernetesReaderRBAC.external` | You bind the connector identity to your own role; the chart creates none. | `false` |
| `kubernetesReaderRBAC.optionalKinds` | Read Gateway API, Argo CD Applications, Argo Rollouts and Flux Kustomizations/HelmReleases. | `true` |
| `kubernetesReaderRBAC.secrets` | List Secrets so the Helm tab can show Helm releases. This also exposes Secret values to the connector identity. | `false` |
| `storage.type` | Storage backend (`file` or `postgres`). HA derives `postgres`. | `"file"` |
| `storage.postgres.dsn` | Postgres DSN (stored in chart Secret) when `storage.type=postgres` | `""` |
| `storage.persistence.enabled` | Mount a PVC at the fixed `/app/data` path so agent state survives restarts | `false` |
| `storage.persistence.size` | PVC request size | `"1Gi"` |
| `agent.enable` | Enable the AI SRE Agent (must run with `replicaCount: 1`) | `false` |
| `agent.mode` | `training`, `shadow`, or `detect` | `"training"` |
| `agent.batchMax` | Max signals pulled into one tick | `5000` |
| `agent.signalMaxBytes` | Per-signal byte cap above which a message is truncated | `65536` |
| `agent.servicePatterns` | Ordered regexes used to discover the service name from a message | see `values.yaml` |
| `agent.redaction.enable` | Scrub each message before it is mined, stored, or sent to an LLM | `true` |
| `agent.redaction.redactIps` | Also mask IP addresses | `false` |
| `agent.redaction.extraPatterns` | Extra operator-supplied redaction regexes | see `values.yaml` |
| `agent.catalog.persistInterval` | How often the pattern catalog is flushed to storage | `"30s"` |
| `agent.catalog.autoPromoteAfter` | Sightings before a learned pattern becomes "known" | `100` |
| `agent.catalog.spikeZ` | Z-score above the learned baseline at which a known pattern re-fires | `3.0` |
| `agent.catalog.spikeAbsCeiling` | Match count that always surfaces regardless of z-score (`0` disables) | `0` |
| `agent.catalog.spikeSustainTicks` | Consecutive spiking ticks required before firing | `1` |
| `agent.catalog.spikeMinFrequency` | Absolute noise floor before a spike can fire | `5` |
| `agent.catalog.spikeMinBaselineCount` | Sightings required before the z-score is trusted | `20` |
| `agent.miner.similarityThreshold` | Log-template miner similarity threshold | `0.4` |
| `agent.miner.treeDepth` | Miner parse-tree depth | `4` |
| `agent.miner.maxChildren` | Miner max children per node | `100` |
| `agent.regex.defaultPattern` | Catch-all prefilter applied to log messages | `"(?i).*error.*"` |
| `agent.regex.rules` | Named patterns that always qualify a message | see `values.yaml` |
| `agent.ai.enable` | Enable LLM analysis in detect mode | `false` |
| `agent.ai.provider` | Model backend: `openai`, `deepseek`, `qwen`, `ollama`, `claude`, `gemini` | `"openai"` |
| `agent.ai.apiKey` | API key for the selected provider (stored in chart Secret) | `""` |
| `agent.ai.model` | Model identifier | `"gpt-4o-mini"` |
| `agent.ai.temperature` | Randomness control. Set `-1` to omit the field for beta-limited / reasoning models that reject explicit temperature values. | `0.2` |
| `agent.ai.maxCallsPerHour` | Per-hour AI call rate limit (`0` = unlimited) | `60` |
| `agent.ai.analyze.model` | Override model for the on-demand analyze agent (empty inherits `agent.ai.model`) | `""` |
| `agent.tools.toolTimeout` | Per-tool dispatch timeout for analyze tools | `"20s"` |
| `agent.tools.parallelTools` | Run multiple tool calls in one model turn concurrently | `false` |
| `agent.tools.recentChanges.git.auth.token` | Git HTTPS token/PAT (stored in chart Secret, injected as `GIT_AUTH_TOKEN`) | `""` |
| `agent.tools.recentChanges.git.repos` | Remote git repos read by the `recent_changes` tool (empty = unregistered) | `[]` |
| `agent.tools.describeDependencies.services` | Service-dependency graph for the `describe_dependencies` tool (empty = unregistered) | `[]` |
| `agent.tools.findRunbook.embeddingModel` | Embedding model for the `find_runbook` runbook-RAG tool (empty = unregistered) | `""` |
| `agent.sources` | Inline list of signal sources (snake_case keys) | `[]` |
| `extraEnv` | Extra container env vars — use these to supply `${VAR}` credentials referenced from `agent.sources` | `[]` |
| `extraEnvFrom` | Bulk-import env from existing Secrets/ConfigMaps | `[]` |
| `proxy.url` | Outbound HTTP/HTTPS/SOCKS5 proxy used by channels with `useProxy: true` | `""` |
| `proxy.username` | Proxy username (stored in chart Secret) | `""` |
| `proxy.password` | Proxy password (stored in chart Secret) | `""` |
| `alert.slack.enable` | Enable Slack notifications | `false` |
| `alert.slack.token` | Slack bot token | `""` |
| `alert.slack.channelId` | Slack channel ID | `""` |
| `alert.telegram.enable` | Enable Telegram notifications | `false` |
| `alert.telegram.useProxy` | Route Telegram API calls through `proxy.*` | `false` |
| `alert.email.enable` | Enable email notifications | `false` |
| `alert.msteams.enable` | Enable Microsoft Teams notifications | `false` |
| `alert.lark.enable` | Enable Lark notifications | `false` |
| `alert.lark.useProxy` | Route Lark webhook calls through `proxy.*` | `false` |
| `alert.viber.enable` | Enable Viber notifications | `false` |
| `alert.viber.apiType` | Viber API type ("channel" or "bot") | `"channel"` |
| `alert.viber.useProxy` | Route Viber API calls through `proxy.*` | `false` |
| `queue.debugBody` | Log the raw body of every message pulled off the inbound queue | `true` |
| `oncall.enable` | Enable on-call functionality | `false` |
| `oncall.provider` | On-call provider ("aws_incident_manager" or "pagerduty") | `"aws_incident_manager"` |
| `redis.enabled` | Enable bundled Redis (required for on-call) | `false` |

The standalone `query_metrics` and `query_traces` tools are retired. For metric
and trace reads, use a licensed Enterprise image with the `intelligence`
entitlement and enabled `prometheus` / `traces` sources in `agent.sources`.
These sources contribute `discover_metrics` / `read_metric_series` and
`discover_trace_fields` / `read_trace_spans` to Chat and Analyze, respectively.
There is no automatic migration: remove old `agent.tools.queryMetrics` and
`agent.tools.queryTraces` values and move backend addresses and credentials to
the corresponding source `options` (snake_case keys). Reference credentials as
`${VAR}` in `agent.sources` and provide them via `extraEnv` / `extraEnvFrom`;
source definitions are stored in a ConfigMap, not a Secret. OSS has no metric
or trace read tools.

> The SNS/SQS inbound toggles live under `alert.sns` / `alert.sqs` for backward
> compatibility; the chart maps them into the binary's top-level `queue` block
> when it renders `config.yaml`.
>
> Every credential in the table above is written to the chart Secret and reaches
> the process as an environment variable that the config files expand via
> `${VAR}`. No credential is rendered into the ConfigMap.

## Notification Channel Configuration

All six channels (`slack`, `telegram`, `viber`, `email`, `msteams`, `lark`)
support `alert.<channel>.kubernetesIssueTemplatePath`. It renders the
application's `kubernetes_issue_template_path` for Kubernetes issue alerts
only. The default empty value uses the shipped
`config/kubernetes_issue_<channel>.tmpl`; a custom path must exist inside every
pod, for example through a custom image or an operator-managed volume mount.
Setting a path does not enable the channel or per-cluster issue alerts.

```yaml
alert:
  slack:
    kubernetesIssueTemplatePath: "/app/custom/kubernetes_issue_slack.tmpl"
```

### Slack

```yaml
alert:
  slack:
    enable: true
    token: "xoxb-your-slack-token"
    channelId: "C12345"
    messageProperties:
      buttonText: "Acknowledge Alert"
      buttonStyle: "primary" # "primary" (blue), "danger" (red), or empty (default gray)
      disableButton: false
```

### Telegram

```yaml
alert:
  telegram:
    enable: true
    botToken: "your-telegram-bot-token"
    chatId: "your-telegram-chat-id"
```

### Email

```yaml
alert:
  email:
    enable: true
    smtpHost: "smtp.example.com"
    smtpPort: 587
    username: "your-email@example.com"
    password: "your-password"
    to: "alerts@example.com"
    subject: "Incident Alert"
```

### Microsoft Teams

```yaml
alert:
  msteams:
    enable: true
    powerAutomateUrl: "your-power-automate-flow-url"
    otherPowerUrls:
      dev: "dev-team-power-automate-url"
      ops: "ops-team-power-automate-url"
```

### Lark

```yaml
alert:
  lark:
    enable: true
    webhookUrl: "your-lark-webhook-url"
    otherWebhookUrls:
      dev: "dev-team-webhook-url"
      prod: "prod-team-webhook-url"
```

### Viber

Viber supports two types of API integrations:

- **Channel API** (default): Send messages to Viber channels for team notifications  
- **Bot API**: Send messages to individual users for personal notifications

**Recommended Configuration (Channel API):**
```yaml
alert:
  viber:
    enable: true
    botToken: "your-viber-channel-token"  # Token for channel or bot
    apiType: "channel"  # Default: "channel" (or "bot" for individual messaging)
    channelId: "your-viber-channel-id"  # Required for Channel API
    templatePath: "/app/config/viber_message.tmpl"
```

**Alternative Configuration (Bot API):**
```yaml
alert:
  viber:
    enable: true
    botToken: "your-viber-bot-token"
    apiType: "bot"  # For individual user notifications
    userId: "your-viber-user-id"  # Required for Bot API
    templatePath: "/app/config/viber_message.tmpl"
```

**When to use each API type:**

- **Channel API** ✅ Better for incident management, team notifications, easier setup
- **Bot API** ⚠️ Limited to individual users, requires user interaction first

## On-Call Configurations

### AWS Incident Manager

```yaml
oncall:
  enable: true
  waitMinutes: 3
  provider: "aws_incident_manager"
  
  awsIncidentManager:
    responsePlanArn: "arn:aws:ssm-incidents::111122223333:response-plan/YourPlan"
    otherResponsePlanArns:
      prod: "arn:aws:ssm-incidents::111122223333:response-plan/ProdPlan"
      dev: "arn:aws:ssm-incidents::111122223333:response-plan/DevPlan"

redis:
  enabled: true
```

### PagerDuty

```yaml
oncall:
  enable: true
  waitMinutes: 5
  provider: "pagerduty"
  
  pagerduty:
    routingKey: "your-pagerduty-routing-key"
    otherRoutingKeys:
      infra: "infrastructure-team-routing-key"
      app: "application-team-routing-key"
      db: "database-team-routing-key"

redis:
  enabled: true
```

## Redis Configuration

Versus uses Redis **only for on-call** (`oncall.enable=true` or `oncall.initializedOnly=true`). The bundled Redis is deployed whenever `redis.enabled=true`; with on-call off it runs unused, so leave it off. With `redis.enabled=false` and on-call off, no Redis is deployed or contacted.

The chart supports exactly two modes — pick one:

### Mode 1 — Bundled Redis (recommended for getting started)

The chart runs one Redis pod from the official `redis` image with a Service named `<fullname>-redis` on port 6379, and the application connects to it automatically. The Versus pod waits for Redis to answer before it starts.

- **Password:** generated on the first install and kept on every upgrade, in the Secret `<fullname>-redis` (key `redis-password`). Set `redis.auth.password` with `--set-string`, or point `redis.auth.existingSecret` (and `redis.auth.existingSecretPasswordKey`) at a Secret you create. Tools that render the chart without cluster access (`helm template`, many GitOps setups) cannot read the kept password and generate a new one on every render; set `redis.auth.password` or `redis.auth.existingSecret` there. Whenever the chart-managed password changes, the Redis pod and the Versus pods restart together. With `redis.auth.existingSecret` the chart cannot see the password, only the Secret name and key: after you rotate the password in your own Secret, restart both yourself with `kubectl -n <namespace> rollout restart deploy/<fullname>-redis`, then `kubectl -n <namespace> rollout restart deploy/<fullname>` (`statefulset/<fullname>` with `ha.enabled`).
- **Data:** a 1Gi PersistentVolumeClaim `<fullname>-redis-data` by default (`redis.persistence.enabled`, `size`, `storageClassName`, `existingClaim`).
- **Image:** `redis.image.repository` and `redis.image.tag` (pinned to an exact release).

```yaml
oncall:
  enable: true

redis:
  enabled: true

# externalRedis.* is IGNORED in this mode — leave it untouched.
```

### Mode 2 — External Redis (production / shared Redis)

Disable the bundled Redis and point the chart at an existing Redis instance.

```yaml
oncall:
  enable: true

redis:
  enabled: false              # do NOT deploy bundled Redis

externalRedis:
  host: "redis.my-namespace.svc.cluster.local"  # required
  port: 6379
  password: "your-redis-password"
  db: 0
  insecureSkipVerify: false
  # Connection tuning. Defaults are deliberately generous so transient
  # DNS / network slowness in Kubernetes does not surface as
  # "context deadline exceeded". Tune downward if you want faster failure.
  connectionTimeout: 30
  readTimeout: 30
  writeTimeout: 30
  maxRetries: 3
  minRetryBackoff: 8
  maxRetryBackoff: 512
```

### Common pitfall — "Redis connection failed: context deadline exceeded"

Setting `externalRedis.host` while leaving `redis.enabled: true` (the default in some older examples) causes the application to talk to the bundled Redis and silently ignore your `externalRedis.*` values. Since chart `1.3.8` the chart fails fast with a clear error if both are set at the same time. If you hit this on an older chart version:

1. Decide which mode you want.
2. For Mode 2: set `redis.enabled: false` AND set `externalRedis.host`.
3. Verify connectivity from the pod: `kubectl exec -it <pod> -- nc -vz <externalRedis.host> 6379`.
4. If you see timeouts, raise `externalRedis.connectionTimeout` / `readTimeout` / `writeTimeout` (now defaulted to 30s).


### Upgrading from the Bitnami Redis

Charts before this one deployed the Bitnami Redis subchart. The bundled Redis is now part of this chart, so an upgrade with `redis.enabled=true` needs four things:

- **Values:** `helm upgrade --reuse-values` keeps the old chart's defaults, including `redis.architecture` and `redis.master`, and the upgrade stops with a message naming them. Move `redis.master.persistence.*` to `redis.persistence.*` in your values file and drop `redis.architecture`, then upgrade with only `-f <values>` (no reuse flag). `--reset-then-reuse-values` (Helm 3.14+) drops the old chart defaults but keeps values you set yourself in the release: if `redis.master` or `redis.architecture` came from your own values, add `--set redis.master=null --set redis.architecture=null`.
- **Image:** the bundled Redis runs the official `redis` image with this chart's tag. If your old release values set `redis.image` (for example `redis.image.repository=bitnamilegacy/redis`, often with `global.security.allowInsecureImages`), the upgrade stops with a message naming `redis.image`. Remove `redis.image` from your values file and, with `--reset-then-reuse-values`, add `--set redis.image=null`. Add it only in that case: when the old values never set `redis.image`, the flag removes the chart's own image too and the upgrade fails with `redis.image.tag is required`. `global.security.*` is not read by this chart and can stay.

  ```bash
  helm upgrade <release> <chart> --namespace <namespace> --reset-then-reuse-values -f <values> \
    --set redis.master=null --set redis.architecture=null
  # Only if your old values set redis.image:
  #   add --set redis.image=null
  ```
- **Data:** the new Redis starts empty on a new volume, `<fullname>-redis-data`. Pending on-call escalations held in the old Redis are lost; incidents and history are not stored in Redis.
- **Clean-up:** the old volume is no longer used. Delete it when you no longer need it: `kubectl -n <namespace> delete pvc redis-data-<release>-redis-master-0`.

## Custom Alert Templates

You can provide custom templates for each notification channel:

```yaml
templates:
  slack: |
    *Critical Error in {{.ServiceName}}*
    ----------
    Error Details:
    ```
    {{.Logs}}
    ```
    ----------
    Owner <@{{.UserID}}> please investigate

  telegram: |
    🚨 <b>Critical Error Detected!</b> 🚨
    📌 <b>Service:</b> {{.ServiceName}}
    ⚠️ <b>Error Details:</b>
    {{.Logs}}
```

## AWS Integrations

Versus Incident can receive alerts from aws sns systems:

### AWS SNS

```yaml
alert:
  sns:
    enable: true
    httpsEndpointSubscriptionPath: "/sns"
    topicArn: "arn:aws:sns:us-east-1:111122223333:my-topic"  # required when enable=true
```

`topicArn` pins the endpoint to your topic; the server refuses to start without
it.

## Ingress Configuration

The Helm chart supports configuring an Ingress resource for external access:

```yaml
ingress:
  enabled: true
  className: "nginx"  # Specify your ingress controller class
  annotations:
    kubernetes.io/ingress.class: nginx
    kubernetes.io/tls-acme: "true"
    # Add any other annotations needed
  hosts:
    - host: versus-incident.example.com
      paths:
        - path: /
          pathType: Prefix
  tls:
    - secretName: versus-incident-tls
      hosts:
        - versus-incident.example.com
```

When enabling Ingress, set `config.publicHost` to the exact browser-visible
HTTP(S) origin. It owns external links, secure-cookie derivation, and exact-origin
CSRF checks; forwarded host and protocol headers are not trusted:

```yaml
config:
  publicHost: "https://versus-incident.example.com"
```

## Scaling and Replicas

This chart does NOT ship a HorizontalPodAutoscaler. The AI SRE Agent worker is
a single writer to the pattern catalog and detect log, so the agent must run
with `replicaCount: 1` — horizontal scaling would produce racing writes (the
chart fails fast if `agent.enable=true` with more than one replica). The file
storage backend is single-replica too: more than one replica needs
`storage.type=postgres`. The number of pods is fixed by `replicaCount` and is
not autoscaled.

```yaml
replicaCount: 1   # required when agent.enable=true
```

## Uninstalling the Chart

To uninstall/delete the `versus-incident` deployment:

```bash
helm uninstall versus-incident
```

## Additional Resources

- [Versus Incident Documentation](https://github.com/versuscontrol/versus-incident)
- [Template Syntax Guide](https://docs.versusincident.com/#/webhook/template-syntax)
- [Configuration Reference](https://docs.versusincident.com/#/configuration/configuration)