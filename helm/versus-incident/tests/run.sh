#!/usr/bin/env bash
# Smoke-test the versus-incident Helm chart by rendering each scenario in
# this directory and asserting on the output.
#
# Each scenario:
#   tests/<n>-<name>.yaml      values overrides
#   tests/<n>-<name>.assert    line-based assertions:
#                                "<regex>"        → must appear in stdout
#                                "!<regex>"       → must NOT appear
#                                "# EXPECT_FAIL"  → expect helm template
#                                                   to exit non-zero, then
#                                                   subsequent regexes are
#                                                   matched against stderr.
#
# Usage: ./helm/versus-incident/tests/run.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHART_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"

if ! command -v helm >/dev/null 2>&1; then
  echo "helm not found; install Helm 3.x" >&2
  exit 2
fi

# Pull subchart deps once so `helm template` works offline.
if [ ! -f "$CHART_DIR/Chart.lock" ] || [ ! -d "$CHART_DIR/charts" ] || [ -z "$(ls -A "$CHART_DIR/charts" 2>/dev/null)" ]; then
  echo "==> helm dependency build"
  helm dependency build "$CHART_DIR" >/dev/null
fi

pass=0
fail=0
failed_scenarios=()

shopt -s nullglob
for values_file in "$SCRIPT_DIR"/*.yaml; do
  base=$(basename "$values_file" .yaml)
  assert_file="$SCRIPT_DIR/$base.assert"
  if [ ! -f "$assert_file" ]; then
    echo "SKIP $base — no .assert file"
    continue
  fi

  expect_fail=0
  if grep -q "^# EXPECT_FAIL" "$assert_file"; then
    expect_fail=1
  fi

  printf "==> %s ... " "$base"

  stdout_file=$(mktemp)
  stderr_file=$(mktemp)
  helm template versus-test "$CHART_DIR" -f "$values_file" \
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
exit 0
