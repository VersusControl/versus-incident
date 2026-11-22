#!/usr/bin/env bash
# Runs the Go checks CI would run for your change, on this machine, with the
# same commands and flags: tools/cichanges picks the groups from the files
# changed since you forked from the base branch (committed, uncommitted and
# untracked), scripts/ci-tests.sh picks the race shards and the plain
# packages and runs them. Tests run with -cpu 4 so a laptop with more cores
# behaves like the 4-vCPU CI runner.
#
#   scripts/ci-local.sh [--base <rev>] [--all] [--shard <name>]... [--plan]
#
#   --base <rev>    branch or commit to compare with (default: main)
#   --all           run everything, as a push to main with code changes does
#   --shard <name>  run only these race shards (repeatable); skips the plain run
#   --plan          print what would run and stop
#
# Each test command is capped at 15 minutes when `timeout` (or `gtimeout`) is
# installed. Coverage profiles land in a temporary folder printed at the end.
set -euo pipefail

cd "$(dirname "$0")/.."

base=main
all=false
plan_only=false
only=()
while [ "$#" -gt 0 ]; do
  case "$1" in
    --base) base=${2:?--base needs a value}; shift 2 ;;
    --all) all=true; shift ;;
    --shard) only+=("${2:?--shard needs a value}"); shift 2 ;;
    --plan) plan_only=true; shift ;;
    -h | --help) sed -n '2,17p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "unknown argument: $1 (see --help)" >&2; exit 2 ;;
  esac
done

capped=()
if command -v timeout > /dev/null; then
  capped=(timeout 15m)
elif command -v gtimeout > /dev/null; then
  capped=(gtimeout 15m)
fi

work=$(mktemp -d "${TMPDIR:-/tmp}/ci-local.XXXXXX")
mkdir -p ui/dist
[ -e ui/dist/.gitkeep ] || touch ui/dist/.gitkeep

fork=$(git merge-base "$base" HEAD)
decision="$work/decision.out"
if [ "$all" = true ]; then
  go run ./tools/cichanges --config .github/ci-groups.json --all > "$decision"
else
  {
    git diff --name-only --no-renames "$fork" --
    git ls-files --others --exclude-standard
  } | sort -u > "$work/files.txt"
  echo "ci-local: $(wc -l < "$work/files.txt" | tr -d ' ') changed file(s) since $base ($fork)"
  go run ./tools/cichanges --config .github/ci-groups.json --files "$work/files.txt" > "$decision"
fi
bash scripts/ci-tests.sh plan "$decision" > "$work/plan.out"

shards_json=$(sed -n 's/^race_shards=//p' "$work/plan.out")
plain=$(sed -n 's/^plain_packages=//p' "$work/plan.out")
go_on=$(sed -n 's/^go=//p' "$decision")
shards=()
for s in $(tr -d '[]"' <<< "$shards_json" | tr ',' ' '); do shards+=("$s"); done
if [ "${#only[@]}" -gt 0 ]; then
  shards=("${only[@]}")
  plain=""
fi

echo "ci-local: groups on: $(grep '=true$' "$decision" | cut -d= -f1 | paste -sd ' ' -)"
if [ "${#shards[@]}" -gt 0 ]; then echo "ci-local: race shards: ${shards[*]}"; else echo "ci-local: race shards: none"; fi
echo "ci-local: plain packages: ${plain:-none}"
if [ "$plan_only" = true ]; then exit 0; fi

failed=()
step() {
  local name=$1
  shift
  local start=$SECONDS
  echo "== $name"
  if "$@"; then
    echo "== $name: pass ($((SECONDS - start))s)"
  else
    echo "== $name: FAIL ($((SECONDS - start))s)"
    failed+=("$name")
  fi
}

export CI_TEST_CPU=4 CI_COVER_DIR="$work/coverage"

if [ "$go_on" = true ] && [ "${#only[@]}" -eq 0 ]; then
  # shellcheck disable=SC2016 # the inner shell expands $out
  step gofmt bash -c 'out=$(gofmt -l .); [ -z "$out" ] || { echo "$out"; exit 1; }'
  step "go vet" go vet ./...
  step "test lint" go run ./tools/testlint --base "$fork"
  if [ -n "$plain" ]; then
    # shellcheck disable=SC2086
    step "plain tests" ${capped[@]+"${capped[@]}"} bash scripts/ci-tests.sh plain $plain
  fi
fi
for s in ${shards[@]+"${shards[@]}"}; do
  step "race $s" ${capped[@]+"${capped[@]}"} bash scripts/ci-tests.sh run "$s"
done

echo "ci-local: coverage profiles in $CI_COVER_DIR"
if [ "${#failed[@]}" -gt 0 ]; then
  echo "ci-local: failed: ${failed[*]}"
  exit 1
fi
echo "ci-local: every step passed"
