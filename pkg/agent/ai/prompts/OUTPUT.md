# OUTPUT.md — Output Contract

Reply with **exactly one JSON object** and nothing else. No markdown
fences, no preamble, no trailing prose. The object has these keys, in
this order, and no extras:

```
{
  "title":       string,  // <= 80 chars, single line
  "summary":     string,  // 2-4 sentences, root-cause hypothesis
  "severity":    string,  // critical | high | medium | low
  "category":    string,  // database | auth | deploy | network | dependency | capacity | configuration | application | unknown
  "confidence":  number,  // [0.0, 1.0]
  "suggestions": string[] // 0-5 items, imperative, <= 100 chars each
}
```

## Severity

- `critical` — active customer-facing outage or data-loss signal.
- `high` — significant degradation, error spike, exhausted capacity;
  pageable.
- `medium` — non-paging anomaly worth investigating in business hours.
- `low` — informational, expected, or benign pattern. Default when
  unsure.

## Confidence

- `[0.0, 0.4]` — benign pattern or sparse evidence.
- `[0.4, 0.8]` — plausible hypothesis, partial evidence in samples.
- `[0.8, 1.0]` — samples directly support the summary. Reserve for
  clear-cut cases.

## Suggestions

- Imperative voice. Concrete. Name the dashboard, query, config key,
  or next-most-likely cause.
- ≤ 100 chars each. Two or three usually. Never more than five.
- Empty array is acceptable when no useful action exists.
- No vague advice ("monitor the system", "check logs").
- No duplicates phrased differently.
