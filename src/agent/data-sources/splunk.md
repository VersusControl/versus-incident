# Splunk source

Pulls events from Splunk Enterprise / Splunk Cloud via the streaming
`search/v2/jobs/export` REST endpoint. Streams results sorted by
`_time` without holding state on the indexer.

## Minimal config

```yaml
sources:
  - name: prod-splunk
    type: splunk
    enable: true
    splunk:
      address: https://splunk:8089
      token: ${SPLUNK_TOKEN}
      search: 'index=main sourcetype=api level=error'
      page_size: 500
```

## HTTP Basic auth

```yaml
splunk:
  address: https://splunk:8089
  username: ${SPLUNK_USERNAME}
  password: ${SPLUNK_PASSWORD}
  search: 'index=main level=error'
```

## Full reference

```yaml
splunk:
  address: https://splunk:8089       # REQUIRED. Splunk REST base URL (mgmt port, usually 8089).

  # Auth — pick one. Bearer token takes priority when both are set.
  token: ""                          # sent as Authorization: Bearer <token>
  username: ""                       # fallback HTTP Basic
  password: ""

  insecure_skip_verify: false        # dev only

  search: 'index=main level=error'   # REQUIRED. SPL query (auto-prefixed with `search`).
  time_field: _time                  # timestamp field on each event
  message_field: _raw                # field copied into the log message
  severity_field: level              # field copied into severity (empty by default)
  extra_fields:                      # extra fields kept on each signal
    - host
    - source
    - sourcetype
  page_size: 500                     # max events per tick
```

## Behavior

- **Cursor** — The maximum `_time` seen on the previous tick.
  Splunk's `earliest_time` is **inclusive**, so the source drops any
  event whose `_time` is not strictly after the cursor — the boundary
  event is never read twice.
- **Cold start** — With no cursor yet, the source pulls the last 5
  minutes so the first tick has something to look at.
- **Search prefix** — `search` is auto-prefixed with the `search`
  command when it doesn't already start with one, so both
  `index=main error` and `search index=main error` work.
- **Time range** — `earliest_time = cursor`, `latest_time = now`,
  both as sub-second epoch.

## Authentication

Splunk supports two modes — pick one:

- **Bearer token** (recommended) — Create an authentication token in
  Splunk (*Settings → Tokens*). Sent as `Authorization: Bearer
  <token>`.
- **HTTP Basic** — Standard Splunk username + password.

## Try it locally

Point the source at your Splunk management port (usually `8089`),
enable it in `agent_sources.yaml`, and watch the catalog pick up new
events on the **Patterns** page in the admin UI.

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| `splunk: 401 Unauthorized` | Wrong token / credentials, or token feature disabled in Splunk. |
| `splunk: 400 Bad Request` | Invalid SPL — test the search in the Splunk UI first. |
| Connection refused | Wrong port — the REST API uses the management port (`8089`), not the web UI port (`8000`). |
| No new events but logs exist | `search` matches nothing in the current window, or the index/sourcetype name is wrong. |
| Severity always empty | `severity_field` points at a field Splunk doesn't extract — confirm the field name in a sample event. |