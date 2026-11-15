#!/usr/bin/env bash
# Smoke-test the versus-incident Helm chart by rendering each scenario in
# this directory and asserting on the output.
#
# Each scenario is one <n>-<name>.assert file plus its inputs:
#   tests/<n>-<name>.yaml      values overrides, passed with -f (optional)
#   tests/<n>-<name>.args      extra helm template arguments, one per line,
#                              used as given with no shell splitting; lines
#                              starting with "#" are comments (optional).
#                              Relative paths resolve from this directory.
#   tests/<n>-<name>.assert    line-based assertions:
#                                "<regex>"        → must appear in stdout
#                                "!<regex>"       → must NOT appear
#                                "# EXPECT_FAIL"  → expect helm template
#                                                   to exit non-zero, then
#                                                   subsequent regexes are
#                                                   matched against stderr.
#                                "# NOTES"        → render templates/NOTES.txt
#                                                   (which helm template skips)
#                                                   and match only that text.
#                                "# STORED_SIGN_IN_SECRET: <base64>"
#                                                 → render as an upgrade that
#                                                   finds the chart Secret from
#                                                   the previous release holding
#                                                   this gateway_secret.
#                                "# APP_VERSION: <version>"
#                                                 → render a copy of the chart
#                                                   whose Chart.yaml carries
#                                                   this appVersion.
#
# Every successful render is kept as <render dir>/<n>-<name>.yaml. After the
# scenarios pass, the Go checks that load the rendered config through the real
# config loader read those files (VERSUS_CHART_RENDERS); they never run helm.
#
# Usage: ./helm/versus-incident/tests/run.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHART_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
RELEASE="versus-test"

# The lookup the chart uses to keep the generated sign-in secret across
# upgrades. A STORED_SIGN_IN_SECRET scenario swaps it for the stored Secret; if
# the template changes, this must change with it.
SIGN_IN_SECRET_LOOKUP='lookup "v1" "Secret" .Release.Namespace (printf "%s-secrets" (include "versus-incident.fullname" .))'

if ! command -v helm >/dev/null 2>&1; then
  echo "helm not found; install Helm 3.x" >&2
  exit 2
fi

WORK_DIR="$(mktemp -d)"
RENDER_DIR="$WORK_DIR/renders"
mkdir -p "$RENDER_DIR"
trap 'rm -rf "$WORK_DIR"' EXIT

# chart_copy NAME: a fresh copy of the chart that a scenario may patch.
chart_copy() {
  local copy="$WORK_DIR/charts/$1/versus-incident"
  mkdir -p "$(dirname "$copy")"
  cp -R "$CHART_DIR" "$copy"
  printf '%s\n' "$copy"
}

# with_notes_template CHART: make NOTES.txt renderable by wrapping it in a
# named template and printing it from a template file as a block scalar.
with_notes_template() {
  {
    printf '{{- define "test.notes" -}}\n'
    cat "$1/templates/NOTES.txt"
    printf '\n{{- end -}}\n'
  } >"$1/templates/_test_notes.tpl"
  printf 'notes: |\n{{ include "test.notes" . | indent 2 }}\n' >"$1/templates/test-notes.yaml"
}

# with_stored_sign_in_secret CHART BASE64: make the lookup return the chart
# Secret a previous release stored, holding BASE64 as gateway_secret.
with_stored_sign_in_secret() {
  local helpers="$1/templates/_helpers.tpl" content replacement
  content="$(cat "$helpers")"
  case "$content" in
    *"$SIGN_IN_SECRET_LOOKUP"*) ;;
    *)
      echo "the chart no longer reads the stored sign-in secret with the expected lookup; update SIGN_IN_SECRET_LOOKUP in $0" >&2
      return 1
      ;;
  esac
  replacement="(dict \"data\" (dict \"gateway_secret\" \"$2\"))"
  content=${content/"$SIGN_IN_SECRET_LOOKUP"/$replacement}
  printf '%s\n' "$content" >"$helpers"
}

# with_app_version CHART VERSION: set the appVersion in Chart.yaml.
with_app_version() {
  local chart_file="$1/Chart.yaml"
  sed "s/^appVersion:.*/appVersion: \"$2\"/" "$chart_file" >"$chart_file.new" &&
    mv "$chart_file.new" "$chart_file" &&
    grep -q "^appVersion: \"$2\"$" "$chart_file"
}

pass=0
fail=0
failed_scenarios=()

shopt -s nullglob
for values_file in "$SCRIPT_DIR"/*.yaml; do
  base=$(basename "$values_file" .yaml)
  if [ ! -f "$SCRIPT_DIR/$base.assert" ]; then
    echo "SKIP $base — no .assert file"
  fi
done

for assert_file in "$SCRIPT_DIR"/*.assert; do
  base=$(basename "$assert_file" .assert)
  values_file="$SCRIPT_DIR/$base.yaml"
  args_file="$SCRIPT_DIR/$base.args"

  expect_fail=0
  if grep -q "^# EXPECT_FAIL" "$assert_file"; then
    expect_fail=1
  fi

  printf "==> %s ... " "$base"

  args=()
  if [ -f "$values_file" ]; then
    args+=(-f "$values_file")
  fi
  if [ -f "$args_file" ]; then
    while IFS= read -r arg || [ -n "$arg" ]; do
      case "$arg" in
        ""|\#*) continue ;;
      esac
      args+=("$arg")
    done <"$args_file"
  fi

  chart="$CHART_DIR"
  stored_secret=$(sed -n 's/^# STORED_SIGN_IN_SECRET: *//p' "$assert_file")
  notes=0
  if grep -q "^# NOTES$" "$assert_file"; then
    notes=1
  fi
  app_version=$(sed -n 's/^# APP_VERSION: *//p' "$assert_file")
  if [ -n "$stored_secret" ] || [ -n "$app_version" ] || [ "$notes" -eq 1 ]; then
    chart=$(chart_copy "$base")
    if { [ -n "$stored_secret" ] && ! with_stored_sign_in_secret "$chart" "$stored_secret"; } ||
      { [ -n "$app_version" ] && ! with_app_version "$chart" "$app_version"; }; then
      echo "FAIL (could not patch the chart)"
      fail=$((fail+1))
      failed_scenarios+=("$base")
      continue
    fi
    if [ "$notes" -eq 1 ]; then
      with_notes_template "$chart"
      args+=(--show-only templates/test-notes.yaml)
    fi
  fi

  stdout_file=$(mktemp)
  stderr_file=$(mktemp)
  (cd "$SCRIPT_DIR" && helm template "$RELEASE" "$chart" ${args[@]+"${args[@]}"}) \
    >"$stdout_file" 2>"$stderr_file"
  rc=$?

  if [ "$expect_fail" -eq 1 ] && [ "$rc" -eq 0 ]; then
    echo "FAIL (expected helm template to fail, got rc=0)"
    fail=$((fail+1))
    failed_scenarios+=("$base")
    rm -f "$stdout_file" "$stderr_file"
    continue
  fi
  if [ "$expect_fail" -eq 0 ] && [ "$rc" -ne 0 ]; then
    echo "FAIL (helm template rc=$rc)"
    sed 's/^/  | /' "$stderr_file"
    fail=$((fail+1))
    failed_scenarios+=("$base")
    rm -f "$stdout_file" "$stderr_file"
    continue
  fi

  haystack="$stdout_file"
  if [ "$expect_fail" -eq 1 ]; then
    haystack="$stderr_file"
  else
    cp "$stdout_file" "$RENDER_DIR/$base.yaml"
  fi

  scenario_failed=0
  while IFS= read -r line; do
    case "$line" in
      ""|\#*) continue ;;
    esac
    negate=0
    pat="$line"
    case "$line" in
      !*) negate=1; pat="${line:1}" ;;
    esac
    if grep -E -q -- "$pat" "$haystack"; then
      if [ "$negate" -eq 1 ]; then
        echo
        echo "  FAIL: pattern present but should be absent: $pat"
        scenario_failed=1
      fi
    else
      if [ "$negate" -eq 0 ]; then
        echo
        echo "  FAIL: pattern missing: $pat"
        scenario_failed=1
      fi
    fi
  done < "$assert_file"

  if [ "$scenario_failed" -eq 0 ]; then
    echo "ok"
    pass=$((pass+1))
  else
    fail=$((fail+1))
    failed_scenarios+=("$base")
  fi

  rm -f "$stdout_file" "$stderr_file"
done

echo
echo "==> $pass passed, $fail failed"
if [ "$fail" -ne 0 ]; then
  printf '   failed: %s\n' "${failed_scenarios[@]}"
  exit 1
fi

# Chart <-> application checks that need Go: the rendered config files are
# loaded through the real config loader, their key set is compared with
# pkg/config/default_config.yaml, and the rendered reader role is compared with
# what the Kubernetes connector reads. They read the renders above.
if command -v go >/dev/null 2>&1; then
  echo
  echo "==> rendered chart through the Go config loader and RBAC checks (go test)"
  if ! (cd "$CHART_DIR/../.." && VERSUS_CHART_RENDERS="$RENDER_DIR" go test ./pkg/config/ ./pkg/kubernetes/ -run 'TestHelm|ReaderRole|SecretGrant' -count=1); then
    exit 1
  fi
else
  echo
  echo "the rendered-chart checks need Go; install it so drift is actually checked" >&2
  exit 2
fi

exit 0
