# Graylog source

Pulls log messages from [Graylog](https://www.graylog.org/) via the
synchronous `search/universal/absolute` REST endpoint. Sorted by
timestamp, cursor-friendly, no async query lifecycle.

## Minimal config

```yaml
sources:
  - name: prod-graylog
    type: graylog
    enable: true
    graylog:
      address: https://graylog:9000
      api_token: ${GRAYLOG_API_TOKEN}
      query: 'level:(3 OR 4) AND service:api'
      page_size: 500
```

## HTTP Basic auth

```yaml
graylog:
  address: https://graylog:9000
  username: ${GRAYLOG_USERNAME}
  password: ${GRAYLOG_PASSWORD}
  query: '*'
```

## Restrict to a single stream

```yaml
graylog:
  address: https://graylog:9000
  api_token: ${GRAYLOG_API_TOKEN}
  stream_id: 000000000000000000000001
  query: 'level:ERROR'
```

## Full reference

```yaml
graylog:
  address: https://graylog:9000      # REQUIRED. Graylog base URL.

  # Auth — pick one. API token takes priority when both are set.
  api_token: ""                      # sent as Basic <token>:token (Graylog convention)
  username: ""                       # fallback HTTP Basic
  password: ""

  insecure_skip_verify: false        # dev only

  query: '*'                         # Graylog search string. "*" matches all.
  stream_id: ""                      # restrict the search to a single stream
  message_field: message             # field copied into the log message
  severity_field: level              # field copied into severity
  fields:                            # optional server-side projection (faster/smaller)
    - message
    - timestamp
    - source
    - level
  extra_fields:                      # extra fields kept on each signal
    - source
    - service
  page_size: 500                     # max messages per tick (Graylog caps at 150 by default)
```

## Behavior

- **Cursor** — The maximum message timestamp seen on the previous
  tick. Graylog's `from` is **inclusive**, so the source filters the
  response client-side to messages strictly newer than the cursor —
  the boundary message is never read twice.
- **Cold start** — With no cursor yet, the source pulls the last 5
  minutes so the first tick has something to look at instead of
  replaying the full retention.
- **Ordering** — Messages are returned oldest-first.
- **Projection** — Set `fields` to limit what Graylog returns per
  message. Anything in `extra_fields` must also appear in `fields`
  (or leave `fields` empty so the whole document is returned).

## Authentication

Graylog supports two modes — pick one:

- **API token** (recommended) — Create a token under *User → Edit
  tokens* in Graylog. It is sent as HTTP Basic with the token as the
  username and the literal string `token` as the password.
- **HTTP Basic** — Standard Graylog username + password.

## Try it locally

Point the source at your Graylog server, enable it in
`agent_sources.yaml`, and watch the catalog pick up new messages on
the **Patterns** page in the admin UI.

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| `graylog: 401 Unauthorized` | Wrong API token / credentials, or token sent without the literal `token` password. |
| `graylog: 400 Bad Request` | Invalid Graylog query string — test it in the Graylog search UI first. |
| No new messages but logs exist | `query` matches nothing in the current window, or the `stream_id` is wrong. |
| Severity always empty | `severity_field` points at a field that isn't present — check the field name in a sample message. |