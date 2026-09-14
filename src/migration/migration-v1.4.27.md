# Migration to v1.4.27

## SigNoz readers require verified HTTPS

This is a **breaking security compatibility change** for existing SigNoz reader
configurations. All SigNoz source types attach a mandatory query API key and now
reject plain HTTP, `insecure_skip_verify: true`, URL userinfo, redirects, and API
keys shorter than 8 bytes. The transport ignores ambient `HTTP_PROXY` and
`HTTPS_PROXY` settings, requires TLS 1.2 or newer, and revalidates resolved
destination addresses when connecting.

Update every `signoz`, `signoz_metrics`, and `signoz_traces` source to the final
verified HTTPS origin. Do not configure an address that redirects.

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
denied unless `allow_loopback: true` is explicitly set for verified-TLS local
testing. Metadata, link-local, unspecified, and multicast destinations are
always blocked.

Self-hosted installations that previously exposed only HTTP must add TLS
termination with a certificate trusted by the Versus runtime before upgrading.
The bundled Compose examples now require `SIGNOZ_READ_ADDRESS` to name that
final HTTPS origin; their internal HTTP address is used only by the bootstrap
container before the query key is handed to Versus.

The reader address is the SigNoz Query Service URL that serves read APIs, not an
OTLP ingest/export endpoint such as ports 4317 or 4318. Telemetry exporters can
continue sending data to their separately configured collector endpoint; this
change applies to credentialed Versus readers.

An incompatible source fails during startup instead of sending a request. The
configuration error identifies the violated policy, for example:

```text
api key requires verified HTTPS
api key requires verified HTTPS; insecure_skip_verify cannot be enabled
api key must be at least 8 bytes
address host is not permitted
```

After changing the origin, a `3xx` response reports `configure the final
address`; replace the configured URL with the redirect target. A TLS handshake
or unknown-authority error means the Versus runtime does not trust the endpoint
certificate. Install the issuing CA in the runtime trust store or use a
publicly trusted certificate. `allow_private_networks` and `allow_loopback` do
not waive TLS verification.

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
