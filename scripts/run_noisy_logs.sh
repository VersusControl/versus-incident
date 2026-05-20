#!/usr/bin/env bash
# Continuously append fresh noisy log lines on a fixed interval so the agent
# (running in tail mode) has live traffic to chew on. Optionally inject a
# one-shot spike burst to test the spike detector.
#
# Usage:
#   ./scripts/run_noisy_logs.sh                                # live tail to file
#   INTERVAL=10 BATCH=50 ./scripts/run_noisy_logs.sh
#   ./scripts/run_noisy_logs.sh --output local/resource/noisy-app.log
#
#   # target a Loki / Elasticsearch / CloudWatch / Graylog / Splunk backend:
#   ./scripts/run_noisy_logs.sh --target loki
#   ./scripts/run_noisy_logs.sh --target elasticsearch
#   TARGET=cloudwatch CW_LOG_GROUP_NAME=/aws/lambda/foo ./scripts/run_noisy_logs.sh
#   ./scripts/run_noisy_logs.sh --target graylog
#   SPLUNK_HEC_TOKEN=... ./scripts/run_noisy_logs.sh --target splunk
#
#   # inject a single spike burst, then exit:
#   ./scripts/run_noisy_logs.sh --spike db-conn-refused
#   SPIKE=panic SPIKE_BURST=120 ./scripts/run_noisy_logs.sh --target loki
#
# Env vars / flags (live tail):
#   INTERVAL       seconds between batches             (default 5)
#   BATCH          lines per batch                     (default 20)
#   TARGET         file|loki|elasticsearch|cloudwatch|graylog|splunk  (default file)
#   OUTPUT         log file (file target only)         (default local/resource/noisy-app.log)
#   ITER           max iterations, 0 = infinite        (default 0)
#
# Loki target:
#   LOKI_URL       (default http://localhost:3100)
#   LOKI_APP       (default noisy)            ## "app" stream label
#   LOKI_TENANT    (optional)                 ## X-Scope-OrgID
#
# Elasticsearch target:
#   ES_URL         (default http://localhost:9200)
#   ES_INDEX       (default logs-noisy)
#   ES_USER / ES_PASS (optional basic auth)
#
# CloudWatch target:
#   CW_LOG_GROUP_NAME   (required)
#   CW_LOG_STREAM       (default noisy-app)
#   CW_REGION / AWS_REGION
#
# Graylog target (GELF UDP — input must already be configured in Graylog):
#   GRAYLOG_HOST     (default localhost)
#   GRAYLOG_PORT     (default 12201)
#   GRAYLOG_SOURCE   (default noisy)         ## GELF `host` field
#
# Splunk target (HTTP Event Collector):
#   SPLUNK_URL          (default https://localhost:8088)
#   SPLUNK_HEC_TOKEN    (required)
#   SPLUNK_INDEX        (default main)
#   SPLUNK_SOURCETYPE   (default _json)
#
# Spike-mode (disables live tail when set):
#   SPIKE          template name to burst              (e.g. db-conn-refused)
#   SPIKE_BURST    number of lines in the burst        (default 80)
#   SPIKE_CONTEXT  regular noisy lines before burst    (default 0)
#   --list-templates   print available template names and exit
#
# Scenario-mode (curated incident clusters for detect demos):
#   SCENARIO         scenario name (e.g. db-outage, disk-full, tls-expired)
#   SCENARIO_BURST   total lines in the cluster        (default 60)
#   --list-scenarios print available scenarios and exit
#
# Stops cleanly on Ctrl+C.

set -euo pipefail

INTERVAL="${INTERVAL:-5}"
BATCH="${BATCH:-20}"
TARGET="${TARGET:-file}"
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
    --target)   TARGET="$2";   shift 2 ;;
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
      sed -n '2,67p' "$0"; exit 0 ;;
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

# Translate the shell-level TARGET / endpoint env vars into CLI flags for the
# Python generator. The generator also reads the same env vars directly, but
# being explicit avoids surprises (e.g. ES_INDEX silently inheriting from a
# parent shell).
target_args=( --target "$TARGET" )
case "$TARGET" in
  file)
    mkdir -p "$(dirname "$OUTPUT")"
    target_args+=( --output "$OUTPUT" )
    summary_target="file:$OUTPUT"
    ;;
  loki)
    target_args+=( --loki-url "${LOKI_URL:-http://localhost:3100}" \
                   --loki-app "${LOKI_APP:-noisy}" )
    [[ -n "${LOKI_TENANT:-}" ]] && target_args+=( --loki-tenant "$LOKI_TENANT" )
    summary_target="loki:${LOKI_URL:-http://localhost:3100}"
    ;;
  elasticsearch)
    target_args+=( --es-url "${ES_URL:-http://localhost:9200}" \
                   --es-index "${ES_INDEX:-logs-noisy}" )
    [[ -n "${ES_USER:-}" ]] && target_args+=( --es-user "$ES_USER" )
    [[ -n "${ES_PASS:-}" ]] && target_args+=( --es-pass "$ES_PASS" )
    summary_target="elasticsearch:${ES_URL:-http://localhost:9200}/${ES_INDEX:-logs-noisy}"
    ;;
  cloudwatch)
    if [[ -z "${CW_LOG_GROUP_NAME:-}" ]]; then
      echo "TARGET=cloudwatch requires CW_LOG_GROUP_NAME" >&2
      exit 2
    fi
    target_args+=( --cw-log-group "$CW_LOG_GROUP_NAME" \
                   --cw-log-stream "${CW_LOG_STREAM:-noisy-app}" )
    [[ -n "${CW_REGION:-${AWS_REGION:-}}" ]] && \
      target_args+=( --cw-region "${CW_REGION:-${AWS_REGION}}" )
    summary_target="cloudwatch:${CW_LOG_GROUP_NAME}/${CW_LOG_STREAM:-noisy-app}"
    ;;
  graylog)
    target_args+=( --graylog-host "${GRAYLOG_HOST:-localhost}" \
                   --graylog-port "${GRAYLOG_PORT:-12201}" \
                   --graylog-source "${GRAYLOG_SOURCE:-noisy}" )
    summary_target="graylog:gelf://${GRAYLOG_HOST:-localhost}:${GRAYLOG_PORT:-12201}"
    ;;
  splunk)
    if [[ -z "${SPLUNK_HEC_TOKEN:-}" ]]; then
      echo "TARGET=splunk requires SPLUNK_HEC_TOKEN" >&2
      exit 2
    fi
    target_args+=( --splunk-url "${SPLUNK_URL:-https://localhost:8088}" \
                   --splunk-token "$SPLUNK_HEC_TOKEN" \
                   --splunk-index "${SPLUNK_INDEX:-main}" \
                   --splunk-sourcetype "${SPLUNK_SOURCETYPE:-_json}" )
    summary_target="splunk:${SPLUNK_URL:-https://localhost:8088} index=${SPLUNK_INDEX:-main}"
    ;;
  *)
    echo "unknown TARGET: $TARGET" >&2; exit 2 ;;
esac

# Scenario mode: emit a curated cluster of correlated failures so the
# detect-mode AI SRE has rich context for one mini-incident, then exit.
if [[ -n "$SCENARIO" ]]; then
  echo "scenario mode: injecting '$SCENARIO' cluster ($SCENARIO_BURST lines) -> $summary_target"
  python3 "$GEN" \
    --append \
    --start-time now \
    --scenario "$SCENARIO" \
    --scenario-burst "$SCENARIO_BURST" \
    "${target_args[@]}"
  echo "done. Watch the agent log for an 'emit_emitted' verdict on the next tick."
  exit 0
fi

# Spike mode: emit one tight burst of the chosen template, then exit.
# Live-tail loop is skipped entirely so the burst lands in a single tick.
if [[ -n "$SPIKE" ]]; then
  echo "spike mode: injecting $SPIKE_BURST x '$SPIKE' lines (context=$SPIKE_CONTEXT) -> $summary_target"
  python3 "$GEN" \
    --append \
    --start-time now \
    --spike "$SPIKE" \
    --spike-burst "$SPIKE_BURST" \
    --spike-context "$SPIKE_CONTEXT" \
    "${target_args[@]}"
  echo "done. Watch the agent log for a 'SPIKE pattern=...' line on the next tick."
  exit 0
fi

trap 'echo; echo "stopped after $count batch(es)"; exit 0' INT TERM

echo "appending $BATCH line(s) every ${INTERVAL}s -> $summary_target (Ctrl+C to stop)"
count=0
while :; do
  python3 "$GEN" \
    --append \
    --lines "$BATCH" \
    --start-time now \
    --interval-min 0.1 \
    --interval-max 1.0 \
    "${target_args[@]}" \
    >/dev/null
  count=$((count + 1))
  printf '[%s] batch %d (%d lines) -> %s\n' "$(date -u +%H:%M:%SZ)" "$count" "$BATCH" "$summary_target"

  if [[ "$ITER" -gt 0 && "$count" -ge "$ITER" ]]; then
    echo "reached --iter=$ITER, exiting"
    break
  fi
  sleep "$INTERVAL"
done
