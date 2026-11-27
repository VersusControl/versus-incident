#!/usr/bin/env bash
# Go test commands, shared by CI (.github/workflows/dev.yml) and
# scripts/ci-local.sh so both run the same packages with the same flags.
#
#   scripts/ci-tests.sh plan <decision-file>
#       Reads a tools/cichanges decision (key=value lines) and prints
#       race_shards=[...]    the shards whose groups changed, as JSON
#       plain_packages=...   changed packages that no running shard covers;
#                            they get a plain (non-race) run
#   scripts/ci-tests.sh check-lists
#       Checks the Kubernetes race shard lists against the test names in the
#       source, without compiling anything.
#   scripts/ci-tests.sh run <shard>
#       Runs one shard under -race.
#   scripts/ci-tests.sh plain <package>...
#       Runs the packages no race shard covers, without -race.
#
# Optional environment for "run" and "plain":
#   CI_COVER_DIR  write the coverage profile to $CI_COVER_DIR/race-<shard>.out
#                 or $CI_COVER_DIR/plain.out
#   CI_TEST_CPU     pass -cpu to go test. CI leaves it unset (the hosted runner
#                   has 4 vCPUs); scripts/ci-local.sh sets 4 to match.
#   CI_TEST_VERBOSE set to 1 to run the Kubernetes shards' test binaries with
#                   -test.v and print their whole output, for test timings and
#                   the list of names a shard ran.
#
# "run" and "plain" compile first and test second, and print how long each
# took ("ci-tests: <shard>: compile 41s", "ci-tests: <shard>: run 188s"), so a
# slow shard shows whether the time went to the build or to the tests. On
# GitHub the two figures also go to the job summary.
#
# pkg/kubernetes is split by test name into three shards. "kubernetes-rbac"
# and "kubernetes-slow" run only the top-level tests listed in
# .github/race-shards/<shard>.txt from the pkg/kubernetes package;
# "kubernetes" skips every listed name and runs everything else under
# pkg/kubernetes, so a new test lands there without a list change. The
# Kubernetes shards build their test binaries with go test -c and run them
# from the package folder, as go test does. "check-lists" makes sure every
# listed name is a top-level test of pkg/kubernetes, appears in one list only,
# and names no subpackage test; CI runs it once, in the job that plans the
# shards.
#
# Every shard runs at most 4 parallel tests, one per vCPU of the hosted runner
# (the go test default there; set here so a laptop with more cores runs the
# same). Under the race detector more parallel tests than CPUs starve each
# other and overrun the wall-clock waits inside the tests.
set -euo pipefail

caller_dir=$PWD
cd "$(dirname "$0")/.."

lists_dir=.github/race-shards
listed_shards=(kubernetes-rbac kubernetes-slow)

# Which shard owns a package directory (relative to the module root).
shard_of() {
  case "$1" in
    pkg/kubernetes | pkg/kubernetes/*) echo kubernetes ;;
    pkg/storage | pkg/storage/* | pkg/controllers | pkg/controllers/*) echo storage-controllers ;;
    pkg/aws | pkg/aws/* | pkg/awsauth | pkg/awsauth/* | pkg/finops | pkg/finops/*) echo aws ;;
    *) echo rest ;;
  esac
}

plan() {
  local decision=$1
  case "$decision" in /*) ;; *) decision="$caller_dir/$decision" ;; esac
  on() { grep -qx "$1=true" "$decision"; }
  skip() { echo "::notice::Race shard $1: skipped, no changes in $2" >&2; }
  local shards=() running=" "
  if on go-kubernetes; then
    shards+=(kubernetes-rbac kubernetes-slow kubernetes); running+="kubernetes "
  else
    skip kubernetes-rbac "pkg/kubernetes"; skip kubernetes-slow "pkg/kubernetes"; skip kubernetes "pkg/kubernetes"
  fi
  if on go-storage || on go-controllers; then
    shards+=(storage-controllers); running+="storage-controllers "
  else
    skip storage-controllers "pkg/storage, pkg/controllers, pkg/routes"
  fi
  if on go-aws; then
    shards+=(aws); running+="aws "
  else
    skip aws "pkg/aws, pkg/awsauth, pkg/finops"
  fi
  # pkg/routes runs under -race in the rest shard.
  if on go-agent || on go-controllers || on go-shared; then
    shards+=(rest); running+="rest "
  else
    skip rest "pkg/agent, pkg/core, pkg/services, pkg/controllers, pkg/routes or the shared Go code"
  fi

  local json="" s
  for s in ${shards[@]+"${shards[@]}"}; do json="${json:+$json,}\"$s\""; done
  echo "race_shards=[$json]"

  local go_packages module pkg rel plain=""
  go_packages=$(sed -n 's/^go_packages=//p' "$decision" | tail -n 1)
  if [ -n "$go_packages" ]; then
    module=$(go list -m)
    if [ "$go_packages" = "./..." ]; then go_packages=$(go list ./...); fi
    for pkg in $go_packages; do
      rel=${pkg#"$module"}
      rel=${rel#/}
      case "$running" in
        *" $(shard_of "$rel") "*) ;;
        *) plain="${plain:+$plain }$pkg" ;;
      esac
    done
  fi
  echo "plain_packages=$plain"
}

# One name per line; comments and blank lines dropped.
names_of() {
  if [ -f "$1" ]; then grep -v -e '^[[:space:]]*#' -e '^[[:space:]]*$' "$1" | tr -d ' \t\r' || true; fi
}

# Anchored alternation of a list: ^(A|B|...)$. Names hold only letters,
# digits and underscores, so none needs escaping.
pattern_of() { printf '^(%s)$' "$(printf '%s\n' "$1" | paste -sd '|' -)"; }

all_listed_names() {
  local s
  for s in "${listed_shards[@]}"; do names_of "$lists_dir/$s.txt"; done
}

# The top-level test names that go test would run, read from the source:
# "func TestXxx(t *testing.T)" in the test files go list selects for the
# default build (so files behind a build tag are left out, as go test leaves
# them out). $1 is "root" for pkg/kubernetes itself, "sub" for its
# subpackages.
kubernetes_test_names() {
  local which=$1 pkg file
  # shellcheck disable=SC2016 # a go list template, not a shell expansion
  go list -f '{{$p := .ImportPath}}{{$d := .Dir}}{{range .TestGoFiles}}{{$p}}{{"\t"}}{{$d}}/{{.}}{{"\n"}}{{end}}{{range .XTestGoFiles}}{{$p}}{{"\t"}}{{$d}}/{{.}}{{"\n"}}{{end}}' ./pkg/kubernetes/... |
    while IFS=$'\t' read -r pkg file; do
      [ -n "$file" ] || continue
      case "$which:$pkg" in
        root:*/pkg/kubernetes | sub:*/pkg/kubernetes/*) ;;
        *) continue ;;
      esac
      # go test runs Test, or Test followed by anything but a lower-case
      # letter, taking one *testing.T.
      sed -n -E 's/^func (Test[A-Za-z0-9_]*)[[:space:]]*\([A-Za-z_][A-Za-z0-9_]*[[:space:]]+\*testing\.T\).*/\1/p' "$file" |
        grep -E '^Test($|[^a-z])' || true
    done | sort -u
}

# Every top-level test must land in exactly one Kubernetes shard. Here-strings,
# not pipes: grep -q stops reading at its first match, and with pipefail the
# writer's broken pipe would turn a match into a failure.
check_kubernetes_lists() {
  if [ ! -d pkg/kubernetes ]; then
    echo "Kubernetes race shard lists: pkg/kubernetes is not in this branch, nothing to check"
    return 0
  fi
  local s listed
  for s in "${listed_shards[@]}"; do
    if [ -z "$(names_of "$lists_dir/$s.txt")" ]; then
      echo "::error::$lists_dir/$s.txt must list top-level test names, one per line"
      exit 1
    fi
  done
  listed=$(all_listed_names)
  if grep -qv '^Test[A-Za-z0-9_]*$' <<< "$listed"; then
    echo "::error::The files in $lists_dir must list top-level test names only: $(grep -v '^Test[A-Za-z0-9_]*$' <<< "$listed" | paste -sd ',' -)"
    exit 1
  fi
  local twice
  twice=$(sort <<< "$listed" | uniq -d | paste -sd ',' -)
  if [ -n "$twice" ]; then
    echo "::error::Tests listed in more than one Kubernetes race shard: $twice"
    exit 1
  fi
  local root sub name stale="" shadowed=""
  root=$(kubernetes_test_names root)
  sub=$(kubernetes_test_names sub)
  if [ -z "$root" ]; then
    echo "::error::Found no top-level tests in pkg/kubernetes; the race shard list check cannot run"
    exit 1
  fi
  while IFS= read -r name; do
    # A stale name would leave its test in no shard's intent.
    if ! grep -qxF -- "$name" <<< "$root"; then
      stale="${stale:+$stale, }$name"
    fi
    # The remainder shard's skip pattern also applies to subpackages, so a
    # subpackage test with a listed name would run nowhere.
    if grep -qxF -- "$name" <<< "$sub"; then
      shadowed="${shadowed:+$shadowed, }$name"
    fi
  done <<< "$listed"
  if [ -n "$stale" ]; then
    echo "::error::The Kubernetes race shard lists name tests that do not exist in pkg/kubernetes: $stale"
    exit 1
  fi
  if [ -n "$shadowed" ]; then
    echo "::error::The Kubernetes race shard lists name tests that a pkg/kubernetes subpackage also defines: $shadowed"
    exit 1
  fi
  echo "Kubernetes race shard lists: $(wc -l <<< "$listed" | tr -d ' ') names, each a top-level test of pkg/kubernetes in one list only"
}

# Prints a timing line, and adds it to the GitHub job summary when there is one.
report_time() {
  local shard=$1 phase=$2 seconds=$3
  echo "ci-tests: $shard: $phase ${seconds}s"
  if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
    echo "- \`$shard\` $phase: ${seconds}s" >> "$GITHUB_STEP_SUMMARY"
  fi
}

# Runs packages through go test in two steps: a compile that runs no test
# (-exec true starts nothing), then the real run. The second step links the
# test binaries again but reuses every compiled package from the build cache.
two_step_go_test() {
  local label=$1 cover_out=$2
  shift 2
  local flags=() start
  while [ "$#" -gt 0 ] && [ "$1" != -- ]; do flags+=("$1"); shift; done
  shift
  start=$SECONDS
  local built
  if ! built=$(go test "${flags[@]}" -run '^$' -exec true "$@" 2>&1); then
    echo "$built"
    report_time "$label" compile $((SECONDS - start))
    return 1
  fi
  report_time "$label" compile $((SECONDS - start))
  if [ -n "$cover_out" ]; then flags+=("-coverprofile=$cover_out"); fi
  start=$SECONDS
  local rc=0
  go test "${flags[@]}" "$@" || rc=$?
  report_time "$label" run $((SECONDS - start))
  return "$rc"
}

# Runs one test binary from its package folder, as go test does, so tests
# that read files by relative path find them. Like go test with a package
# list, it prints the whole output only when the package fails, and one
# result line either way. A missing binary fails.
run_test_binary() {
  local binary=$1 dir=$2 import_path=$3 cover_out=$4
  shift 4
  if [ ! -x "$binary" ]; then
    echo "::error::Test binary for $import_path is missing: $binary"
    return 1
  fi
  local args=(-test.short -test.count=1 -test.timeout=13m -test.parallel=4 -test.paniconexit0)
  if [ -n "${CI_TEST_CPU:-}" ]; then args+=("-test.cpu=$CI_TEST_CPU"); fi
  if [ -n "$cover_out" ]; then args+=("-test.coverprofile=$cover_out"); fi
  if [ "${CI_TEST_VERBOSE:-}" = 1 ]; then args+=(-test.v); fi
  local start=$SECONDS rc=0 output="$binary.out" coverage
  (cd "$dir" && "$binary" "${args[@]}" "$@") > "$output" 2>&1 || rc=$?
  coverage=$(grep -E '^coverage: ' "$output" | tail -n 1 || true)
  if [ "$rc" -eq 0 ] && [ "${CI_TEST_VERBOSE:-}" = 1 ]; then cat "$output"; fi
  if [ "$rc" -eq 0 ]; then
    printf 'ok  \t%s\t%ss\t%s\n' "$import_path" $((SECONDS - start)) "$coverage"
  else
    cat "$output"
    printf 'FAIL\t%s\t%ss\n' "$import_path" $((SECONDS - start))
  fi
  return "$rc"
}

# Concatenates coverage profiles into one, keeping the first mode line.
merge_profiles() {
  local out=$1 first=true p
  shift
  : > "$out"
  for p in "$@"; do
    [ -f "$p" ] || continue
    if [ "$first" = true ]; then cat "$p" >> "$out"; first=false; else tail -n +2 "$p" >> "$out"; fi
  done
}

run_kubernetes_shard() {
  local shard=$1
  if [ ! -d pkg/kubernetes ]; then
    echo "::notice::Race shard $shard: skipped, pkg/kubernetes is not in this branch"
    return 0
  fi
  local build=(-race)
  if [ -n "${CI_COVER_DIR:-}" ]; then
    mkdir -p "$CI_COVER_DIR"
    build+=(-cover -covermode=atomic)
  fi
  local filter=() targets
  case "$shard" in
    kubernetes)
      targets=./pkg/kubernetes/...
      local listed
      listed=$(all_listed_names)
      if [ -n "$listed" ]; then filter=("-test.skip=$(pattern_of "$listed")"); fi
      ;;
    *)
      targets=./pkg/kubernetes
      local names
      names=$(names_of "$lists_dir/$shard.txt")
      if [ -z "$names" ]; then
        echo "::error::$lists_dir/$shard.txt lists no tests"
        exit 1
      fi
      filter=("-test.run=$(pattern_of "$names")")
      ;;
  esac

  local bin_dir
  bin_dir=$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/ci-tests-bin.XXXXXX")
  # shellcheck disable=SC2064 # expand bin_dir now
  trap "rm -rf '$bin_dir'" EXIT

  local start=$SECONDS
  go test -c "${build[@]}" -o "$bin_dir/" "$targets"
  report_time "$shard" compile $((SECONDS - start))

  # One line per package with tests: import path, folder, binary name.
  local packages
  packages=$(go list -f '{{if or .TestGoFiles .XTestGoFiles}}{{.ImportPath}}{{"\t"}}{{.Dir}}{{end}}' "$targets" | grep . || true)
  local main_line sub_lines
  main_line=$(grep -E $'/pkg/kubernetes\t' <<< "$packages" || true)
  sub_lines=$(grep -vE $'/pkg/kubernetes\t' <<< "$packages" || true)
  if [ -z "$main_line" ]; then
    echo "::error::pkg/kubernetes has no test files"
    exit 1
  fi

  local cover_main="" sub_log="$bin_dir/subpackages.log" sub_pid="" rc=0 profiles=()
  if [ -n "${CI_COVER_DIR:-}" ]; then cover_main="$bin_dir/kubernetes.cover"; profiles+=("$cover_main"); fi
  start=$SECONDS
  # The subpackages are small; they run one after another next to the main
  # package, as go test would run them alongside it.
  if [ -n "$sub_lines" ]; then
    (
      sub_rc=0
      while IFS=$'\t' read -r import_path dir; do
        cover=""
        if [ -n "${CI_COVER_DIR:-}" ]; then cover="$bin_dir/$(basename "$dir").cover"; fi
        run_test_binary "$bin_dir/$(basename "$dir").test" "$dir" "$import_path" "$cover" "${filter[@]}" || sub_rc=1
      done <<< "$sub_lines"
      exit "$sub_rc"
    ) > "$sub_log" 2>&1 &
    sub_pid=$!
  fi
  local import_path dir
  IFS=$'\t' read -r import_path dir <<< "$main_line"
  run_test_binary "$bin_dir/kubernetes.test" "$dir" "$import_path" "$cover_main" "${filter[@]}" || rc=1
  if [ -n "$sub_pid" ]; then
    wait "$sub_pid" || rc=1
    cat "$sub_log"
    while IFS=$'\t' read -r import_path dir; do
      if [ -n "${CI_COVER_DIR:-}" ]; then profiles+=("$bin_dir/$(basename "$dir").cover"); fi
    done <<< "$sub_lines"
  fi
  report_time "$shard" run $((SECONDS - start))
  if [ -n "${CI_COVER_DIR:-}" ]; then merge_profiles "$CI_COVER_DIR/race-$shard.out" "${profiles[@]}"; fi
  return "$rc"
}

run_shard() {
  local shard=$1
  export GORACE="${GORACE:-halt_on_error=1}"
  case "$shard" in
    kubernetes-rbac | kubernetes-slow | kubernetes)
      run_kubernetes_shard "$shard"
      return
      ;;
  esac

  local flags=(-short -race -count=1 -timeout 13m -parallel 4) cover_out=""
  if [ -n "${CI_TEST_CPU:-}" ]; then flags+=(-cpu "$CI_TEST_CPU"); fi
  if [ -n "${CI_COVER_DIR:-}" ]; then
    mkdir -p "$CI_COVER_DIR"
    flags+=(-covermode=atomic)
    cover_out="$CI_COVER_DIR/race-$shard.out"
  fi

  local dirs=""
  case "$shard" in
    storage-controllers) dirs="pkg/storage pkg/controllers" ;;
    aws) dirs="pkg/aws pkg/awsauth pkg/finops" ;;
    rest) ;;
    *)
      echo "::error::Unknown race shard: $shard"
      exit 1
      ;;
  esac

  local packages
  if [ "$shard" = rest ]; then
    local module pkg rel
    module=$(go list -m)
    packages=""
    for pkg in $(go list ./...); do
      rel=${pkg#"$module"}
      rel=${rel#/}
      if [ "$(shard_of "$rel")" = rest ]; then packages="${packages:+$packages }$pkg"; fi
    done
    if [ -z "$packages" ]; then
      echo "::error::Race shard rest selected no packages"
      exit 1
    fi
  else
    local patterns="" missing="" dir
    for dir in $dirs; do
      if [ -d "$dir" ]; then
        patterns="$patterns ./$dir/..."
      else
        missing="${missing:+$missing, }$dir"
      fi
    done
    if [ -z "$patterns" ]; then
      echo "::notice::Race shard $shard: skipped, its packages ($missing) are not in this branch"
      return 0
    fi
    if [ -n "$missing" ]; then
      echo "Race shard $shard: $missing not in this branch, testing the rest"
    fi
    # shellcheck disable=SC2086
    packages=$(go list $patterns)
  fi
  # Word splitting of the package list is intended.
  # shellcheck disable=SC2086
  two_step_go_test "$shard" "$cover_out" "${flags[@]}" -- $packages
}

run_plain() {
  local flags=(-short -count=1 -timeout 13m -parallel 4) cover_out=""
  if [ -n "${CI_TEST_CPU:-}" ]; then flags+=(-cpu "$CI_TEST_CPU"); fi
  if [ -n "${CI_COVER_DIR:-}" ]; then
    mkdir -p "$CI_COVER_DIR"
    flags+=(-covermode=atomic)
    cover_out="$CI_COVER_DIR/plain.out"
  fi
  two_step_go_test plain "$cover_out" "${flags[@]}" -- "$@"
}

case "${1:-}" in
  plain)
    shift
    if [ "$#" -eq 0 ]; then echo "usage: scripts/ci-tests.sh plain <package>..." >&2; exit 2; fi
    run_plain "$@"
    ;;
  plan) plan "${2:?usage: scripts/ci-tests.sh plan <decision-file>}" ;;
  check-lists) check_kubernetes_lists ;;
  run) run_shard "${2:?usage: scripts/ci-tests.sh run <shard>}" ;;
  *)
    echo "usage: scripts/ci-tests.sh plan <decision-file> | check-lists | run <shard> | plain <package>..." >&2
    exit 2
    ;;
esac
