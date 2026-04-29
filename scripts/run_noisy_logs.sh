#!/usr/bin/env bash
# Continuously append fresh noisy log lines on a fixed interval so the agent
# (running in tail mode) has live traffic to chew on.
#
# Usage:
#   local/scripts/run_noisy_logs.sh                     # defaults
#   INTERVAL=10 BATCH=50 ./local/scripts/run_noisy_logs.sh
#   ./local/scripts/run_noisy_logs.sh --output local/resource/noisy-app.log
#
# Env vars / flags:
#   INTERVAL  seconds between batches              (default 5)
#   BATCH     lines per batch                      (default 20)
#   OUTPUT    log file to append to                (default local/resource/noisy-app.log)
#   ITER      max iterations, 0 = infinite         (default 0)
#
# Stops cleanly on Ctrl+C.

set -euo pipefail

INTERVAL="${INTERVAL:-5}"
BATCH="${BATCH:-20}"
OUTPUT="${OUTPUT:-local/resource/noisy-app.log}"
ITER="${ITER:-0}"

# Allow flag overrides too
while [[ $# -gt 0 ]]; do
  case "$1" in
    --interval) INTERVAL="$2"; shift 2 ;;
    --batch)    BATCH="$2";    shift 2 ;;
    --output|-o) OUTPUT="$2";  shift 2 ;;
    --iter)     ITER="$2";     shift 2 ;;
    -h|--help)
      sed -n '2,18p' "$0"; exit 0 ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
done

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GEN="$SCRIPT_DIR/generate_noisy_logs.py"

if [[ ! -f "$GEN" ]]; then
  echo "generator not found: $GEN" >&2
  exit 1
fi

mkdir -p "$(dirname "$OUTPUT")"

trap 'echo; echo "stopped after $count batch(es)"; exit 0' INT TERM

echo "appending $BATCH line(s) every ${INTERVAL}s to $OUTPUT (Ctrl+C to stop)"
count=0
while :; do
  python3 "$GEN" \
    --append \
    --output "$OUTPUT" \
    --lines "$BATCH" \
    --start-time now \
    --interval-min 0.1 \
    --interval-max 1.0 \
    >/dev/null
  count=$((count + 1))
  printf '[%s] batch %d (%d lines) -> %s\n' "$(date -u +%H:%M:%SZ)" "$count" "$BATCH" "$OUTPUT"

  if [[ "$ITER" -gt 0 && "$count" -ge "$ITER" ]]; then
    echo "reached --iter=$ITER, exiting"
    break
  fi
  sleep "$INTERVAL"
done
