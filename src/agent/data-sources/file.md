# File source

Tail a single log file from disk. The cheapest way to test the agent
end-to-end and the source you should use for fixtures or to onboard a
new format before plumbing in a real backend.

## Minimal config

```yaml
# agent_sources.yaml
sources:
  - name: my-app
    type: file
    enable: true
    file:
      path: /var/log/my-app/app.log
```

That's it. The agent tails new lines, parses an optional leading
timestamp, and feeds each line through the regex pre-filter and miner.

## Full reference

```yaml
file:
  path: /var/log/my-app/app.log    # REQUIRED. Globs are NOT supported.
  format: text                     # "text" (default) or "json"
  from_beginning: false            # true = replay whole file on first start
  cursor_path: ""                  # default: <storage.file.data_dir>/cursors/file-<name>.cursor
  max_line_bytes: 65536            # truncate longer lines
  max_lines_per_pull: 1000         # cap signals per tick (paginates backlog)

  # text-mode only
  timestamp_layout: ""             # Go time layout; empty = auto-detect

  # json-mode only
  message_field: message
  timestamp_field: "@timestamp"
  severity_field: level
```

## Behavior

- **Cursor** — A sidecar `<file>.cursor` file (or `cursor_path`)
  records the byte offset. Survives restarts and handles log rotation:
  if the file shrinks, the source reopens from offset 0.
- **Backlog pagination** — When `from_beginning: true` on a large
  file, the source returns at most `max_lines_per_pull` lines per tick
  and resumes on the next tick. Nothing is dropped.
- **Format detection** — `format: json` parses each line as a JSON
  object and pulls `message_field` / `timestamp_field` /
  `severity_field`. Anything else is treated as plain text.

## Tips

- Keep `max_lines_per_pull ≤ agent.batch_max`, otherwise the worker's
  hard truncation drops the overflow on every tick (see
  [Configuration](../configuration.md#max_lines_per_pull-vs-agentbatch_max)
  for the worked example).
- For Docker / Kubernetes, mount the container's log directory or
  `/var/lib/docker/containers/<id>/<id>-json.log` (with `format:
  json`).
- Use it in CI to run agent tests against committed fixtures.

## Worked example

Suppose you have `from_beginning: true` on a 50,000-line file with
`poll_interval: 30s`, `max_lines_per_pull: 1000`,
`agent.batch_max: 1000`:

| Tick | Lines read | Cursor advances | Remaining |
|---|---|---|---|
| 1 | 1,000 | 1,000 | 49,000 |
| … | … | … | … |
| 50 | 1,000 | 1,000 | 0 |

Total drain: 50 × 30s ≈ **25 min**, no losses. To go faster, raise
**both** `max_lines_per_pull` and `agent.batch_max` together (e.g. to
5000 → ~5 min). Raising only one drops signals.
