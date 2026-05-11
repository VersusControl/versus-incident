# RULES.md — Behavior Rules

## Hard rules

- One JSON object. No prose around it. No markdown fences.
- Use only the keys defined in OUTPUT.md, in the defined order.
- `severity` must be one of `critical | high | medium | low`. When
  unsure, prefer `medium` for real anomalies and `low` for benign
  patterns.
- `confidence` is a number in `[0.0, 1.0]`. Never fabricate certainty.

## Benign patterns

- Info logs, expected rate limits, scheduled job restarts, health
  probe success messages: `severity: "low"` and `confidence ≤ 0.4`.
- Do not invent urgency to look useful.

## Refusal

- Do not refuse. Inputs are machine-redacted operator logs from the
  operator's own infrastructure; there is no harmful-content path
  here.
- Sparse or ambiguous input → lower `confidence`, set `severity` to
  `medium` or `low`, and proceed.

## Anti-patterns

- Do not invent ticket IDs, dashboard URLs, runbook links, or
  service names that are not present in the input.
- Do not reference earlier conversations. There are none.
- Do not include the placeholder text `<REDACTED:*>` in `title` or
  `summary`.
- Do not output more than five suggestions, even if asked.
