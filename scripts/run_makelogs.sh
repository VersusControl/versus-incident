#!/usr/bin/env bash
# Push synthetic HTTP-traffic logs into Elasticsearch using elastic/makelogs.
# Useful for exercising the agent's `elasticsearch` SignalSource end-to-end.
#
# Docs: https://github.com/elastic/makelogs
#
# Usage:
#   scripts/run_makelogs.sh                       # defaults
#   COUNT=50000 DAYS=1 ./scripts/run_makelogs.sh
#   ./scripts/run_makelogs.sh --host https://es.local:9200 --auth user:pass --insecure
#
# Env vars / flags (flags win):
#   ES_HOST        elasticsearch URL              (default http://localhost:9200)
#   ES_AUTH        user:password (optional)
#   COUNT          events to push                 (default 10000)
#   DAYS           days of historical data        (default 1)
#   INDEX_PREFIX   index name prefix              (default logstash-)
#   INDEX_INTERVAL daily | monthly | yearly | <N> (default daily)
#                  daily produces logstash-YYYY.MM.DD; a number bucketizes
#                  by event count (makelogs' raw default).
#   RESET          "true" to drop existing indices first
#   INSECURE       "true" to skip TLS verify
#   INTERVAL       seconds between runs (loop mode); 0 = run once (default 0)
#   ITER           max loop iterations, 0 = infinite (default 0)
#   NO_ENSURE_ES   "true" to skip the docker auto-start probe (default false)
#
# Stops cleanly on Ctrl+C in loop mode.

set -euo pipefail

ES_HOST="${ES_HOST:-http://localhost:9200}"
ES_AUTH="${ES_AUTH:-}"
COUNT="${COUNT:-10000}"
DAYS="${DAYS:-1}"
INDEX_PREFIX="${INDEX_PREFIX:-logstash-}"
INDEX_INTERVAL="${INDEX_INTERVAL:-daily}"
RESET="${RESET:-false}"
INSECURE="${INSECURE:-false}"
INTERVAL="${INTERVAL:-0}"
ITER="${ITER:-0}"
NO_ENSURE_ES="${NO_ENSURE_ES:-false}"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --host)         ES_HOST="$2";       shift 2 ;;
    --auth)         ES_AUTH="$2";       shift 2 ;;
    --count|-c)     COUNT="$2";         shift 2 ;;
    --days|-d)      DAYS="$2";          shift 2 ;;
    --index-prefix) INDEX_PREFIX="$2";  shift 2 ;;
    --index-interval) INDEX_INTERVAL="$2"; shift 2 ;;
    --reset)        RESET="true";       shift   ;;
    --insecure)     INSECURE="true";    shift   ;;
    --interval)     INTERVAL="$2";      shift 2 ;;
    --iter)         ITER="$2";          shift 2 ;;
    --no-ensure-es) NO_ENSURE_ES="true"; shift  ;;
    -h|--help)      sed -n '2,24p' "$0"; exit 0 ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
done

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Make sure ES is reachable (auto-start a docker container if not)
if [[ "$NO_ENSURE_ES" != "true" ]]; then
  ENSURE="$SCRIPT_DIR/ensure_elasticsearch.sh"
  if [[ -x "$ENSURE" ]]; then
    ES_HOST="$ES_HOST" "$ENSURE"
  else
    echo "[warn] $ENSURE not found or not executable; skipping ES auto-start" >&2
  fi
fi

# Locate makelogs (npm global, npx fallback)
if command -v makelogs >/dev/null 2>&1; then
  MAKELOGS=(makelogs)
elif command -v npx >/dev/null 2>&1; then
  MAKELOGS=(npx --yes -p @elastic/makelogs makelogs)
else
  cat >&2 <<'EOF'
makelogs not found. Install it with one of:
  npm install -g @elastic/makelogs
  # or rely on npx (Node.js 16+):
  npx -p @elastic/makelogs makelogs --help
EOF
  exit 1
fi

build_args() {
  ARGS=(--host "$ES_HOST" --count "$COUNT" --days "$DAYS" \
    --indexPrefix "$INDEX_PREFIX" --indexInterval "$INDEX_INTERVAL")
  [[ -n "$ES_AUTH" ]] && ARGS+=(--auth "$ES_AUTH")
  [[ "$RESET" == "true" ]]    && ARGS+=(--reset)
  [[ "$INSECURE" == "true" ]] && ARGS+=(--insecure)
  return 0
}

run_once() {
  build_args
  echo "[$(date -u +%H:%M:%SZ)] makelogs -> $ES_HOST (count=$COUNT days=$DAYS prefix=$INDEX_PREFIX interval=$INDEX_INTERVAL)"
  if [[ "$RESET" == "true" ]]; then
    # --reset already auto-confirms the "replace existing indices?" prompt
    "${MAKELOGS[@]}" "${ARGS[@]}"
  else
    # Auto-answer "no" once so makelogs keeps existing indices and appends.
    # Without this, makelogs blocks on an interactive prompt every run.
    # A single "n\n" is enough; using `yes` floods stdin and makelogs echoes
    # every line back.
    "${MAKELOGS[@]}" "${ARGS[@]}" <<< "n"
  fi
}

if [[ "$INTERVAL" -le 0 ]]; then
  run_once
  exit $?
fi

trap 'echo; echo "stopped after $count run(s)"; exit 0' INT TERM

# --reset only makes sense on the first iteration in loop mode
ORIGINAL_RESET="$RESET"
count=0
while :; do
  run_once
  count=$((count + 1))
  RESET="false"  # never reset on subsequent loops

  if [[ "$ITER" -gt 0 && "$count" -ge "$ITER" ]]; then
    echo "reached --iter=$ITER, exiting"
    break
  fi
  echo "sleeping ${INTERVAL}s before next batch (Ctrl+C to stop)"
  sleep "$INTERVAL"
done

# silence shellcheck about unused var
: "$ORIGINAL_RESET"
