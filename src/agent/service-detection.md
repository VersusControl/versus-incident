# Service Detection

## Concept

Every log line the agent reads gets a **service** name: the app or component
that wrote it, such as `checkout-api`. The agent reads that name out of the log
text with a list of patterns, `agent.service_patterns`. The first pattern that
matches wins.

The service name is what lets the agent say "there's a spike in
`checkout-api`" instead of "there's a spike somewhere". Incidents, the
[Service Heatmap](./service-heatmap.md) and the new-service grace all work per
service. A line that matches no pattern lands in `_unknown`.

> A service here is just a label taken from the log line. It does not have to
> match anything in Kubernetes or a service mesh.

## Example

Your logs come in three shapes:

| Log line | Detected service |
|---|---|
| `level=error service_name=checkout-api msg="acme-pay timeout"` | `checkout-api` |
| `{"service":"orders","level":"error","msg":"db timeout"}` | `orders` |
| `2026-06-30 12:00:01.123 payments-api [main] WARN retrying` | `payments-api` |

The shipped patterns already understand all three. When `checkout-api` starts
throwing a new timeout, the incident title, the Slack message and the heatmap
all say `checkout-api`.

A fourth app writes `ERROR [1210] cache miss` with no service field. Nothing
matches it, so its lines land in `_unknown` until you add a pattern for it.

## What you see in the UI

Open **Agent Learning → Services** to see every service the agent has found.

![Agent Learning → Services: 14 discovered services with first seen time, origin, status and grace; checkout-api is Auto and tracked, payments-api is Manual and tracked, orders is Auto and in grace with 59 seconds left, and _unknown is marked fallback; Add service and Clear all services buttons at the top](../docs/images/sre-services-ui-dark.png)

| Column | What it shows |
|---|---|
| **First seen** | When the agent first saw the service. |
| **Origin** | **Auto** when found in the logs, **Manual** when you added it. |
| **Status** | **in grace** while a new service is still being learned quietly, **tracked** once it can raise incidents. |
| **Grace** | Time left in the grace window. |

Select services to **End grace** early or **Restart grace**. **Add service**
creates one by hand; services you added can be renamed or deleted.
_Enterprise_ adds **Ignore learning** for a service you never want the agent
to learn from. On **Agent Learning → Logs** you can select patterns and use
**Assign to service** when a line was attributed to the wrong service.

## Turn it on

Service detection is on by default with patterns for common log shapes:
`service_name=`, `service=` / `svc=` / `app=` / `component=`, JSON
`"service":`, Spring Boot and Logback console lines, syslog `name[pid]:`, and
bracketed names. Log levels, numbers and common JVM thread names are never taken
as a service.

To match your own layout, put your pattern **first**. Each pattern needs exactly
one capture group `( … )` around the service name; a pattern without one is
skipped at startup with `missing capture group` in the log.

```yaml
agent:
  service_patterns:
    - '\b(checkout-api|orders|payments-api)\b'                         # your exact names first
    - '(?i)\b(?:service|svc|app|component)\s*=\s*"?([A-Za-z0-9._-]+)' # key=value fallback
```

Setting `service_patterns` replaces the whole default list, so keep the
defaults you still want as fallbacks. The full default list is in the shipped
[`config/config.yaml`](https://github.com/VersusControl/versus-incident/blob/main/config/config.yaml).

### Set it via environment variable

You can also set the list with `AGENT_SERVICE_PATTERNS`, one regex per item,
separated by commas. A regex that contains a comma must go in YAML instead.

| Tip | Why |
|---|---|
| Specific patterns first | The first match wins, so a loose pattern can grab a thread or logger name before yours is tried. |
| Prefer `\b` over `.*` | `\b(checkout-api)\b` is tighter and faster than `.*(checkout-api).*`. |
| Test with a real line | Use a Go (RE2) regex tester and check that group 1 is the service name. |

To turn detection off, set `service_patterns: []`; every line then lands in
`_unknown`.

## Reference

- [Configuration](./configuration.md): `agent.service_patterns`,
  `AGENT_SERVICE_PATTERNS` and `agent.new_service_grace`.
- [Regex](./regex.md): the separate rules that decide which lines are learned.
- [Service Heatmap](./service-heatmap.md): per-service health built on these
  names.
