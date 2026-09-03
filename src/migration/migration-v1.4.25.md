# Migrating to v1.4.25

## Strict sibling configuration keys

`tools.yaml` and `agent_sources.yaml` now reject unknown keys instead of
silently ignoring them. This is an intentional breaking validation change:
remove misspelled, obsolete, or unsupported keys before restarting. Environment
references are expanded only after YAML parsing and only within decoded string
scalars, so an expanded value cannot inject additional YAML structure.

For compatibility, list fields still accept either YAML lists or comma-separated
strings. For example, both `endpoint_cidrs: [10.0.0.0/8, 192.168.0.0/16]` and
`endpoint_cidrs: 10.0.0.0/8,192.168.0.0/16` remain valid.