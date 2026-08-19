# Migrating to v1.4.22

This release hardens the AWS SNS endpoint and fixes alert attribution. Most
deployments need **no manual steps** — but if you receive alerts over **SNS**,
there are two changes you must act on, and a third if you install with **Helm**.

Everything else in this release is a behavior fix that applies itself on upgrade.

## Upgrading

```bash
# Docker
docker pull ghcr.io/versuscontrol/versus-incident:v1.4.22

# Helm
helm repo update
helm upgrade versus-incident oci://ghcr.io/versuscontrol/charts/versus-incident \
  --version 1.4.22
```

Restart the service to apply the changes.

For any issues with the upgrade, please
[open an issue](https://github.com/VersusControl/versus-incident/issues) on
GitHub.

> **Coming from OSS to Enterprise?** If you are also switching the community
> binary for the Enterprise one, read
> [Migrating OSS data to Enterprise](./migration-oss-to-enterprise.md) — your
> existing data stays filed under the org `default` until you move it.

## Must Act

### 1. The `/sns` endpoint now verifies AWS signatures

`/sns` is unauthenticated by design — anyone on the internet can POST to it. As
of v1.4.22 it **validates the AWS SNS message signature** (SignatureVersion 1
and 2) against an Amazon-served certificate **before it reads any field** of the
body. A body that does not verify is rejected with `400` and never becomes an
incident.

**There is no opt-out.** A toggle to disable verification would restore the
vulnerability it fixes: the endpoint previously followed the `SubscribeURL`
supplied *in the request body*, so an attacker could point it at an internal
service or the cloud instance metadata endpoint and have Versus fetch it — a
server-side request forgery.

| What changes | Impact |
|---|---|
| Hand-crafted or `curl` POSTs to `/sns` | Now rejected with `400`. `/sns` is for **real AWS SNS traffic only**. |
| Real SNS notifications and subscription confirmations | Unchanged — they carry a valid signature and pass. |
| `POST /api/incidents` (the generic webhook) | **Unaffected.** This is where hand-crafted and test payloads belong. |

If you had scripts or synthetic checks poking `/sns`, point them at
[`/api/incidents`](../webhook/getting-started.md) instead.

### 2. `queue.sns.topic_arn` is now required when SNS is enabled

If `queue.sns.enable` is `true` and `topic_arn` is empty, **the server refuses to
start**:

```
queue.sns.enable is set but queue.sns.topic_arn is empty: set SNS_TOPIC_ARN to the topic this endpoint serves
```

Why this is mandatory: a valid AWS signature only proves that **some** SNS topic
sent the message — not **yours**. Anyone with an AWS account can have Amazon sign
a message. Without a topic pin, an attacker can subscribe your endpoint to
**their** topic and then publish to you with genuine AWS signatures. The ARN is
what binds the endpoint to your topic; messages from any other topic are refused.

```yaml
queue:
  enable: true
  sns:
    enable: true
    https_endpoint_subscription_path: /sns
    topic_arn: ${SNS_TOPIC_ARN} # Required when sns.enable is true
```

Set the value through the environment variable:

```bash
docker run -d \
  -e SNS_ENABLE=true \
  -e SNS_TOPIC_ARN=arn:aws:sns:us-east-1:111122223333:my-topic \
  ...
```

See [`SNS_TOPIC_ARN`](../configuration/configuration.md) in the configuration
reference.

### 3. Helm users: SNS/SQS config now actually reaches the server

The chart previously rendered the `sns:` and `sqs:` blocks **under `alert:`**,
while the server reads inbound queue sources from a **top-level `queue:` block**.
The values were parsed as unknown keys and silently dropped — which means
**`/sns` never mounted on a Helm install**, no matter what you set. This release
fixes the rendering so the blocks land under `queue:` where the server reads them.

The **values keys are unchanged** (`alert.sns.*`, `alert.sqs.*`), so **no values
file edits are needed** for the fix itself:

```yaml
alert:
  sns:
    enable: true
    httpsEndpointSubscriptionPath: /sns
    topicArn: "arn:aws:sns:us-east-1:111122223333:my-topic"  # now required when enable=true
  sqs:
    enable: false
```

Two things to do:

- **Re-verify SNS if you believed it was working via Helm.** It was not. After
  the upgrade the endpoint mounts for the first time — confirm your subscription
  is confirmed and alerts arrive.
- **Set `alert.sns.topicArn`.** It is required by the server (see above).

> **Known gap:** the chart does **not** validate `topicArn` at install time.
> Enabling SNS without it installs cleanly, and the pod then **CrashLoops** on
> the server-side check with the error shown in section 2. Set `topicArn`
> whenever you set `enable: true`.

## Who Needs to Act

| Your setup | What to do |
|---|---|
| **No SNS** (webhook, SQS, or agent only) | **Nothing.** Upgrade and restart. |
| **SNS enabled, Docker/Compose/Kubernetes** | Set **`SNS_TOPIC_ARN`** before restarting, or the server will not start. Stop sending hand-crafted payloads to `/sns`. |
| **SNS enabled, Helm** | Set **`alert.sns.topicArn`**, upgrade, then **re-verify** that SNS delivery works — it was silently broken before this release. |
| **Anything poking `/sns` with `curl`** | Point it at **`POST /api/incidents`** instead. |
