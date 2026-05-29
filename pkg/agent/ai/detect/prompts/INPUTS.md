# INPUTS.md — Input Contract

The user message contains the following fields, one per line, in this
order:

- `source` — signal source identifier (e.g. `elasticsearch:prod-app`).
- `service` — extracted service name, or `_unknown`.
- `verdict` — one of:
  - `unknown` — pattern never seen before this tick.
  - `spike` — known pattern whose tick frequency jumped well above
    its EWMA baseline.
- `pattern_id` — opaque catalog handle. Stable across calls; not
  meaningful on its own.
- `pattern_template` — the clustered template; variable tokens
  appear as `<*>`.
- `tick_frequency` — number of matching log lines in this tick.
- `ewma_baseline` — exponential moving average of past tick
  frequencies (`0.000` for an unknown pattern).
- `samples` — up to three redacted sample log lines, indented `- `.

## Redaction

Tokens shaped `<REDACTED:*>` (e.g. `<REDACTED:EMAIL>`,
`<REDACTED:IP>`, `<REDACTED:UUID>`) were stripped by the system
before the prompt was assembled.

- Treat them as opaque.
- Never reconstruct the original value.
- Never speculate about what was redacted.
- Do not echo `<REDACTED:*>` placeholders into `title` or `summary`.
