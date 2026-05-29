# Output

When you have enough signal, reply with a SINGLE JSON object (no prose,
no fences) matching this schema:

```json
{
  "title": "string, ≤120 chars, one-line headline",
  "summary": "string, ≤500 chars, what is happening and why it matters",
  "severity": "low|medium|high|critical",
  "confidence": 0.0,
  "category": "string, optional, e.g. saturation, deploy, auth, dependency",
  "root_cause_hypotheses": [
    {
      "hypothesis": "short statement",
      "confidence": 0.0,
      "rationale": "one sentence pointing to the evidence"
    }
  ],
  "evidence": [
    {
      "source": "tool name or 'snapshot'",
      "summary": "what this evidence shows",
      "detail": "optional verbatim snippet, ≤200 chars"
    }
  ],
  "related_pattern_ids": ["pattern_id_1"],
  "next_steps": ["actionable, imperative, ≤80 chars each, max 5"],
  "suggestions": ["legacy field, leave empty if next_steps used"]
}
```

Rules:
- `severity` MUST be one of the four enum values.
- `confidence` is a float in `[0, 1]`.
- At least one of `root_cause_hypotheses` or `next_steps` MUST be
  non-empty — empty findings are useless.
- Order `root_cause_hypotheses` by descending confidence.
- Cap arrays: `root_cause_hypotheses` ≤ 5, `evidence` ≤ 8,
  `next_steps` ≤ 5.
