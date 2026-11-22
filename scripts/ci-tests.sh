#!/usr/bin/env bash
# Go test commands, shared by CI (.github/workflows/dev.yml) and
# scripts/ci-local.sh so both run the same packages with the same flags.
#
#   scripts/ci-tests.sh plan <decision-file>
#       Reads a tools/cichanges decision (key=value lines) and prints
#       race_shards=[...]    the shards whose groups changed, as JSON
#       plain_packages=...   changed packages that no running shard covers;
#                            they get a plain (non-race) run
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
#
# pkg/kubernetes is split by test name into three shards. "kubernetes-rbac"
# and "kubernetes-slow" run only the top-level tests listed in
# .github/race-shards/<shard>.txt from the pkg/kubernetes package;
# "kubernetes" skips every listed name and runs everything else under
# pkg/kubernetes, so a new test lands there without a list change. The listed
# shards first check that every name is a top-level test of pkg/kubernetes,
# appears in one list only, and names no subpackage test.
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

# Every top-level test must land in exactly one Kubernetes shard. Here-strings,
# not pipes: grep -q stops reading at its first match, and with pipefail the
# writer's broken pipe would turn a match into a failure.
check_kubernetes_lists() {
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
  # go test -list prints each package's test names, then its result line
  # ("ok", "?" or "FAIL" followed by the import path).
  local listing root sub name stale="" shadowed=""
  listing=$(go test -short -count=1 -list '.*' ./pkg/kubernetes/...)
  root=$(awk '/^Test/ { n[++c] = $0; next } /^(ok|\?|FAIL)[ \t]/ { if ($2 ~ /\/pkg\/kubernetes$/) for (i = 1; i <= c; i++) print n[i]; c = 0 }' <<< "$listing")
  sub=$(awk '/^Test/ { n[++c] = $0; next } /^(ok|\?|FAIL)[ \t]/ { if ($2 !~ /\/pkg\/kubernetes$/) for (i = 1; i <= c; i++) print n[i]; c = 0 }' <<< "$listing")
  while IFS= read -r name; do
    # A stale name would leave its test in no shard's intent.
    if ! grep -qxF -- "$name" <<< "$root"; then
      stale="${stale:+$stale, }$name"
    fi
    # The remainder shard's -skip also applies to subpackages, so a subpackage
    # test with a listed name would run nowhere.
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
}

run_shard() {
  local shard=$1
  export GORACE="${GORACE:-halt_on_error=1}"
  local flags=(-short -race -count=1 -timeout 13m -parallel 4)
  if [ -n "${CI_TEST_CPU:-}" ]; then flags+=(-cpu "$CI_TEST_CPU"); fi
  if [ -n "${CI_COVER_DIR:-}" ]; then
    mkdir -p "$CI_COVER_DIR"
    flags+=(-covermode=atomic "-coverprofile=$CI_COVER_DIR/race-$shard.out")
  fi

  local dirs=""
  case "$shard" in
    kubernetes-rbac | kubernetes-slow)
      if [ ! -d pkg/kubernetes ]; then
        echo "::notice::Race shard $shard: skipped, pkg/kubernetes is not in this branch"
        return 0
      fi
      check_kubernetes_lists
      go test "${flags[@]}" -run "$(pattern_of "$(names_of "$lists_dir/$shard.txt")")" ./pkg/kubernetes
      return 0
      ;;
    kubernetes)
      dirs="pkg/kubernetes"
      local listed
      listed=$(all_listed_names)
      if [ -n "$listed" ]; then flags+=(-skip "$(pattern_of "$listed")"); fi
      ;;
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
  go test "${flags[@]}" $packages
}

run_plain() {
  local flags=(-short -count=1 -timeout 13m -parallel 4)
  if [ -n "${CI_TEST_CPU:-}" ]; then flags+=(-cpu "$CI_TEST_CPU"); fi
  if [ -n "${CI_COVER_DIR:-}" ]; then
    mkdir -p "$CI_COVER_DIR"
    flags+=(-covermode=atomic "-coverprofile=$CI_COVER_DIR/plain.out")
  fi
  go test "${flags[@]}" "$@"
}

case "${1:-}" in
  plain)
    shift
    if [ "$#" -eq 0 ]; then echo "usage: scripts/ci-tests.sh plain <package>..." >&2; exit 2; fi
    run_plain "$@"
    ;;
  plan) plan "${2:?usage: scripts/ci-tests.sh plan <decision-file>}" ;;
  run) run_shard "${2:?usage: scripts/ci-tests.sh run <shard>}" ;;
  *)
    echo "usage: scripts/ci-tests.sh plan <decision-file> | run <shard> | plain <package>..." >&2
    exit 2
    ;;
esac
