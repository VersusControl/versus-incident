# Security Policy

Versus Incident handles incident data, webhook secrets, and on-call routing
keys. We take security reports seriously and aim to acknowledge every report
quickly.

## Supported versions

Security fixes are issued against the latest minor release. Older minor
versions are not patched.

| Version | Supported |
|---------|-----------|
| latest `1.x` | ✅ |
| older `1.x` | ❌ — please upgrade |
| `0.x` | ❌ |

## Reporting a vulnerability

**Please do not open a public GitHub issue for security reports.**

Use one of the following private channels:

1. **GitHub Security Advisory** (preferred):
   https://github.com/VersusControl/versus-incident/security/advisories/new
2. **Email:** `supports@devopsvn.tech` with the subject line
   `[SECURITY] versus-incident: <short summary>`.

When reporting, please include:

- Affected version(s) and deployment mode (Docker, Helm, source build)
- Reproduction steps or proof-of-concept
- Impact (information disclosure, RCE, privilege escalation, etc.)
- Any suggested fix or mitigation

You should expect:

- **Acknowledgement** within 3 business days.
- **Initial triage and severity assessment** within 7 business days.
- **Fix or mitigation plan** depending on severity:
  - Critical / High → patch release as soon as a fix is verified.
  - Medium → next scheduled release.
  - Low → tracked publicly once a fix is available.
- A **CVE / GHSA advisory** for any vulnerability that affects published
  releases.

We are happy to credit reporters in the advisory unless you prefer to remain
anonymous.

## Out of scope

- Reports against unsupported versions (please upgrade and re-test).
- Issues that require an attacker to already control the host running
  Versus Incident, the Redis instance, or the configured AI provider.
- Findings that depend on misconfiguration explicitly documented as
  development-only (e.g. `redis.insecure_skip_verify: true`).
- Denial-of-service via unbounded request volume to the public
  `/api/incidents` endpoint when no rate-limiting/proxy is deployed in
  front. Operators are expected to terminate TLS and rate-limit at the
  edge.

## Hardening recommendations for operators

- Run Versus Incident behind an authenticated reverse proxy. The
  `/api/incidents` endpoint is intentionally unauthenticated so monitoring
  tools can post directly; restrict it at the network layer.
- Set a strong root-level `gateway_secret` if any admin endpoints
  (`/api/admin/*` or `/api/agent/*`) are exposed. Empty `gateway_secret`
  means the admin routes are not registered at all — never set it to an
  empty string and assume the endpoints are protected by something else.
- Use TLS to Redis in production. Do **not** set
  `redis.insecure_skip_verify: true` outside development.
- Provide all tokens, webhook URLs, and routing keys via environment
  variables — never commit them to YAML.
- Enable redaction (`agent.redaction.enable: true`, default) before
  pointing the agent at production logs.
