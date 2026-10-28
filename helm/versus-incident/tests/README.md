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

## Config drift

`run.sh` finishes by running the chart/app drift tests in
`pkg/config/helm_chart_drift_test.go`:

* `TestHelmChartCoversDefaultConfig` renders every scenario branch and
  fails when the chart cannot express a key the binary supports — the
  chart-side analogue of `TestDefaultConfigMatchesSample`. Keys that are
  deliberately not chart-exposed are listed, with a reason, in
  `appKeysNotExposedByChart`; keys the chart renders that are absent from
  the baseline are listed in `chartKeysNotInAppConfig`.
* `TestHelmChartRendersLoadableConfig` writes the rendered `config.yaml`,
  `agent_sources.yaml` and `tools.yaml` into a temp directory and loads
  them through the real config loader, so a block whose shape disagrees
  with the Go structs fails here instead of crash-looping the pod.

`20-config-coverage.yaml` is the maximal scenario the drift test relies
on — do not trim it down; a key removed from it is a key the test can no
longer prove the chart can express.
