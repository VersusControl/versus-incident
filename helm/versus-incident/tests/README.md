# Versus-Incident chart tests

These are `helm template` smoke tests, not full `helm test`/`bats`
suites. They render the chart against a few representative `values.yaml`
files and grep the output for required strings (and the absence of
unexpected ones). Run from repo root:

```
./helm/versus-incident/tests/run.sh
```

Each scenario is a YAML file in this directory; the runner renders it
and asserts the listed conditions. Add a new scenario by dropping a
`<name>.yaml` here and a matching `<name>.assert` (one regex per line,
prefix `!` to assert absence) sibling.
