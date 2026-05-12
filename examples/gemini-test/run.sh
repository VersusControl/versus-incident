#!/usr/bin/env bash
# End-to-end Gemini detect-mode test for versus-incident.
#
# Prereqs (set in your shell BEFORE running this script):
#   export GEMINI_API_KEY=<your-new-gemini-key>     # do NOT paste in chat
#   export GATEWAY_SECRET=test-secret-1234
#
# What it does:
#   1. Builds the binary
#   2. Boots versus pointing at this example's config (detect mode + Gemini)
#   3. Injects a few error patterns into the file source
#   4. Waits for the agent to call Gemini and emit a finding
#   5. Prints the AI detect log entry (prompt + response + finding)
#
# Stop with Ctrl-C. Cleanup is automatic.

set -euo pipefail

if [ -z "${GEMINI_API_KEY:-}" ]; then
  echo "✗ GEMINI_API_KEY is not set. Export your key first:" >&2
  echo "    export GEMINI_API_KEY=<your-key>" >&2
  exit 1
fi
if [ -z "${GATEWAY_SECRET:-}" ]; then
  echo "ℹ GATEWAY_SECRET not set; using 'test-secret-1234' for this run."
  export GATEWAY_SECRET=test-secret-1234
fi

cd "$(dirname "$0")/../.."        # repo root
REPO_ROOT="$(pwd)"
EXAMPLE_DIR="$REPO_ROOT/examples/gemini-test"

echo "▶ building binary"
go build -o "$EXAMPLE_DIR/run" ./cmd

# Prepare runtime working dir
rm -rf "$EXAMPLE_DIR/data" "$EXAMPLE_DIR/local"
mkdir -p "$EXAMPLE_DIR/data" "$EXAMPLE_DIR/local/resource"
touch "$EXAMPLE_DIR/local/resource/app.log"

# versus loads config from `config/config.yaml` relative to CWD. We
# symlink so we don't have to copy.
rm -rf "$EXAMPLE_DIR/config"
mkdir "$EXAMPLE_DIR/config"
ln -sf "$EXAMPLE_DIR/config.yaml"          "$EXAMPLE_DIR/config/config.yaml"
ln -sf "$EXAMPLE_DIR/agent_sources.yaml"   "$EXAMPLE_DIR/config/agent_sources.yaml"
# Copy default channel templates so the agent doesn't fail to render
# (the templates are referenced but no channel is enabled, so they
# are not actually used in this test).
cp "$REPO_ROOT/config/"*.tmpl "$EXAMPLE_DIR/config/"

cd "$EXAMPLE_DIR"

echo "▶ starting versus on :3000 (detect mode + Gemini)"
./run > server.log 2>&1 &
SERVER_PID=$!
trap "kill $SERVER_PID 2>/dev/null || true; wait 2>/dev/null || true" EXIT

# Wait until healthz answers.
for _ in $(seq 1 30); do
  if curl -sf http://localhost:3000/healthz >/dev/null 2>&1; then
    break
  fi
  sleep 0.5
done
echo "✔ server up"

# Inject distinct error patterns.
echo "▶ injecting test log lines"
ts() { date -u +%Y-%m-%dT%H:%M:%SZ; }
for line in \
  "ERROR [api] database connection refused: dial tcp 10.0.0.1:5432: connect timeout" \
  "ERROR [auth] panic: nil pointer dereference in token validator at handlers/auth.go:142" \
  "ERROR [worker] OOMKilled: container exceeded memory limit 512Mi (used 537Mi)" \
; do
  echo "$(ts) $line" >> local/resource/app.log
done

echo "▶ waiting up to 60s for the agent to call Gemini"
for _ in $(seq 1 30); do
  EVENTS=$(curl -sf -H "X-Gateway-Secret: $GATEWAY_SECRET" \
    http://localhost:3000/api/agent/detect 2>/dev/null | jq 'length // 0')
  if [ "$EVENTS" -gt 0 ]; then
    break
  fi
  sleep 2
done

echo
echo "================ agent status ================"
curl -s -H "X-Gateway-Secret: $GATEWAY_SECRET" http://localhost:3000/api/agent/status | jq '{patterns, sources, ai}'

echo
echo "================ detect log (latest) ================"
curl -s -H "X-Gateway-Secret: $GATEWAY_SECRET" http://localhost:3000/api/agent/detect | \
  jq '.[0] // "no detect events yet — check server.log for AI errors"'

echo
echo "================ server log (last 30 lines) ================"
tail -n 30 server.log

echo
echo "Ctrl-C to stop. Server.log: $EXAMPLE_DIR/server.log"
wait $SERVER_PID
