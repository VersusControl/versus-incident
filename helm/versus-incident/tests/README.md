# Versus-Incident chart tests

These are `helm template` smoke tests, not full `helm test`/`bats`
suites. They render the chart against a few representative `values.yaml`
files and grep the output for required strings (and the absence of
unexpected ones). Run from repo root:

```
./helm/versus-incident/tests/run.sh
```

Each scenario is a `<name>.assert` file in this directory with its inputs:
`<name>.yaml` (values, passed with `-f`) and/or `<name>.args` (extra
`helm template` arguments, one per line, used as given; relative paths
resolve from this directory, so shared values live in `values/`). The
`.assert` file holds one regex per line, prefix `!` to assert absence.
Directives in the `.assert` file:

* `# EXPECT_FAIL` — `helm template` must fail; the regexes match stderr.
* `# NOTES` — render `templates/NOTES.txt`, which `helm template` skips,
  and match only the notes.
* `# STORED_SIGN_IN_SECRET: <base64>` — render an upgrade that finds the
  chart Secret of the previous release with this `gateway_secret`.
* `# APP_VERSION: <version>` — render a copy of the chart with this
  `appVersion`.

Every successful render is kept as `<render dir>/<name>.yaml` for the Go
checks below. The release name is `versus-test`.

## Go checks on the renders

`go test` never runs `helm`. `run.sh` finishes by running the Go checks that
need a rendered chart with `VERSUS_CHART_RENDERS` pointing at its renders;
without it they skip:

* `TestHelmChartCoversDefaultConfig` (`pkg/config`) reads every scenario
  branch and fails when the chart cannot express a key the binary supports —
  the chart-side analogue of `TestDefaultConfigMatchesSample`. Keys that are
  deliberately not chart-exposed are listed, with a reason, in
  `appKeysNotExposedByChart`; keys the chart renders that are absent from
  the baseline are listed in `chartKeysNotInAppConfig`.
* `TestHelmChartRendersLoadableConfig` and the other `TestHelm*` render tests
  write the rendered `config.yaml`, `agent_sources.yaml` and `tools.yaml`
  into a temp directory and load them through the real config loader, so a
  block whose shape disagrees with the Go structs fails here instead of
  crash-looping the pod.
* The reader role tests (`pkg/kubernetes`) compare the rendered
  ClusterRole with what the Kubernetes connector reads.

The committed one-file manifests in `examples/kubernetes/` are checked
against a fresh render by `scripts/render_kubernetes_manifest.sh --check` in
the chart CI job.

`20-config-coverage.yaml` is the maximal scenario the drift test relies
on — do not trim it down; a key removed from it is a key the test can no
longer prove the chart can express.
