#!/usr/bin/env bash
# Continuously append fresh noisy log lines on a fixed interval so the agent
# (running in tail mode) has live traffic to chew on. Optionally inject a
# one-shot spike burst to test the spike detector.
#
# Usage:
#   ./scripts/run_noisy_logs.sh                                # live tail
#   INTERVAL=10 BATCH=50 ./scripts/run_noisy_logs.sh
#   ./scripts/run_noisy_logs.sh --output local/resource/noisy-app.log
#
#   # inject a single spike burst, then exit:
#   ./scripts/run_noisy_logs.sh --spike db-conn-refused
#   SPIKE=panic SPIKE_BURST=120 ./scripts/run_noisy_logs.sh
#
# Env vars / flags (live tail):
#   INTERVAL       seconds between batches             (default 5)
#   BATCH          lines per batch                     (default 20)
#   OUTPUT         log file to append to               (default local/resource/noisy-app.log)
#   ITER           max iterations, 0 = infinite        (default 0)
#
# Env vars / flags (spike mode — disables live tail when set):
#   SPIKE          template name to burst              (e.g. db-conn-refused)
#   SPIKE_BURST    number of lines in the burst        (default 80)
#   SPIKE_CONTEXT  regular noisy lines before burst    (default 0)
#   --list-templates   print available template names and exit
#
# Env vars / flags (scenario mode — curated incident clusters for detect demos):
#   SCENARIO         scenario name (e.g. db-outage, disk-full, tls-expired)
#   SCENARIO_BURST   total lines in the cluster        (default 60)
#   --list-scenarios print available scenarios and exit
#
# Stops cleanly on Ctrl+C.

set -euo pipefail

INTERVAL="${INTERVAL:-5}"
BATCH="${BATCH:-20}"
OUTPUT="${OUTPUT:-local/resource/noisy-app.log}"
ITER="${ITER:-0}"
SPIKE="${SPIKE:-}"
SPIKE_BURST="${SPIKE_BURST:-80}"
SPIKE_CONTEXT="${SPIKE_CONTEXT:-0}"
SCENARIO="${SCENARIO:-}"
SCENARIO_BURST="${SCENARIO_BURST:-60}"
LIST_TEMPLATES=0
LIST_SCENARIOS=0

# Allow flag overrides too
while [[ $# -gt 0 ]]; do
  case "$1" in
    --interval) INTERVAL="$2"; shift 2 ;;
    --batch)    BATCH="$2";    shift 2 ;;
    --output|-o) OUTPUT="$2";  shift 2 ;;
    --iter)     ITER="$2";     shift 2 ;;
    --spike)         SPIKE="$2";         shift 2 ;;
    --spike-burst)   SPIKE_BURST="$2";   shift 2 ;;
    --spike-context) SPIKE_CONTEXT="$2"; shift 2 ;;
    --scenario)       SCENARIO="$2";       shift 2 ;;
    --scenario-burst) SCENARIO_BURST="$2"; shift 2 ;;
    --list-templates) LIST_TEMPLATES=1;  shift ;;
    --list-scenarios) LIST_SCENARIOS=1;  shift ;;
    -h|--help)
      sed -n '2,32p' "$0"; exit 0 ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
done

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GEN="$SCRIPT_DIR/generate_noisy_logs.py"

if [[ ! -f "$GEN" ]]; then
  echo "generator not found: $GEN" >&2
  exit 1
fi

if [[ "$LIST_TEMPLATES" -eq 1 ]]; then
  exec python3 "$GEN" --list-templates
fi
if [[ "$LIST_SCENARIOS" -eq 1 ]]; then
  exec python3 "$GEN" --list-scenarios
fi

mkdir -p "$(dirname "$OUTPUT")"

# Scenario mode: emit a curated cluster of correlated failures so the
# detect-mode AI SRE has rich context for one mini-incident, then exit.
if [[ -n "$SCENARIO" ]]; then
  echo "scenario mode: injecting '$SCENARIO' cluster ($SCENARIO_BURST lines) into $OUTPUT"
  python3 "$GEN" \
    --append \
    --output "$OUTPUT" \
    --start-time now \
    --scenario "$SCENARIO" \
    --scenario-burst "$SCENARIO_BURST"
  echo "done. Watch the agent log for an 'emit_emitted' verdict on the next tick."
  exit 0
fi

# Spike mode: emit one tight burst of the chosen template, then exit.
# Live-tail loop is skipped entirely so the burst lands in a single tick.
if [[ -n "$SPIKE" ]]; then
  echo "spike mode: injecting $SPIKE_BURST x '$SPIKE' lines into $OUTPUT (context=$SPIKE_CONTEXT)"
  python3 "$GEN" \
    --append \
    --output "$OUTPUT" \
    --start-time now \
    --spike "$SPIKE" \
    --spike-burst "$SPIKE_BURST" \
    --spike-context "$SPIKE_CONTEXT"
  echo "done. Watch the agent log for a 'SPIKE pattern=...' line on the next tick."
  exit 0
fi

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
