# Migration to v1.4.27

## SigNoz reader transport compatibility

SigNoz `signoz`, `signoz_metrics`, and `signoz_traces` readers accept final,
non-redirecting HTTP and HTTPS Query Service origins. This is not a breaking
configuration change for existing HTTP or `insecure_skip_verify` deployments.
Verified HTTPS is recommended for production. HTTP sends the mandatory query
API key in plaintext and should be used only on a trusted network.

For HTTPS, certificate verification is enabled by default and TLS 1.2 is the
minimum. Set `insecure_skip_verify: true` only when the risk of an unverified
HTTPS endpoint is explicitly accepted. The same flag is accepted and ignored
for HTTP because no TLS handshake occurs.

```yaml
signoz:
  address: https://signoz.example.internal
  api_key: ${SIGNOZ_API_KEY}
  allow_private_networks: true
  insecure_skip_verify: false
```

For Enterprise metric and trace sources, put the same fields under `options:`.
Set `allow_private_networks: true` only when the final origin intentionally
resolves to a trusted RFC1918 or IPv6 unique-local address. Loopback remains
denied unless `allow_loopback: true` is explicitly set for local testing. This
destination policy is independent of HTTP or HTTPS and certificate verification.
Metadata, link-local, unspecified, and multicast destinations are always blocked.

Self-hosted installations that expose only HTTP should add TLS
termination with a certificate trusted by the Versus runtime before production
use, but they do not need to do so merely to upgrade. The bundled Compose
examples use `http://signoz:8080` on their explicitly trusted private Docker
network. Override `SIGNOZ_READ_ADDRESS` with a verified HTTPS origin outside
that local topology.

The reader address is the SigNoz Query Service URL that serves read APIs, not an
OTLP ingest/export endpoint such as ports 4317 or 4318. Telemetry exporters can
continue sending data to their separately configured collector endpoint; this
change applies to credentialed Versus readers.

An invalid source fails during startup instead of sending a request. The
configuration error identifies the violated policy, for example:

```text
api key must be at least 8 bytes
address host is not permitted
```

After changing the origin, a `3xx` response reports `configure the final
address`; replace the configured URL with the redirect target. A TLS handshake
or unknown-authority error means the Versus runtime does not trust the endpoint
certificate. Install the issuing CA in the runtime trust store, use a publicly
trusted certificate, or explicitly accept the risk with
`insecure_skip_verify: true`. `allow_private_networks` and `allow_loopback`
control destinations independently and do not change TLS verification.

Successful shared-service and tool responses are scrubbed for the exact
configured API key before they return data. The `type: signoz` ingestion path
applies the same scrub before returning a signal, even though it bypasses the
shared tool service. This covers `Message`, `Severity`, nested `Fields` and
`Raw` map keys and values, arrays, and the row ID or fallback material used for
de-duplication. Operators may therefore see `[REDACTED:SIGNOZ_API_KEY]` in
place of the configured key, including when the key was the row ID.

Query construction, polling windows, row and byte bounds, timeouts, cursors,
retries, pagination, and overlap handling retain their existing semantics.
Secret-bearing content and de-duplication identifiers are intentionally
sanitized before persistence, model input, or admin output; ingestion output is
therefore not byte-for-byte unchanged.
