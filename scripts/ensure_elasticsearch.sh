#!/usr/bin/env bash
# Ensure a local Elasticsearch is reachable. If not, start one in Docker.
#
# Usage:
#   scripts/ensure_elasticsearch.sh                  # defaults
#   ES_VERSION=8.13.4 ES_PORT=9200 ./scripts/ensure_elasticsearch.sh
#
# Env vars / flags:
#   ES_HOST       URL to probe                  (default http://localhost:9200)
#   ES_PORT       host port to publish          (default 9200)
#   ES_VERSION    image tag                     (default 8.13.4)
#   ES_NAME       container name                (default versus-es)
#   ES_PASSWORD   elastic user password         (default changeme; only used by 8.x security off)
#   WAIT_SECS     max seconds to wait for ES    (default 90)
#
# Notes:
#   - Single-node, security DISABLED (xpack.security.enabled=false) for local dev.
#   - Idempotent: re-running detects an already-running container or live ES.
#   - Pair with scripts/run_makelogs.sh to push events into it.

set -euo pipefail

ES_HOST="${ES_HOST:-http://localhost:9200}"
ES_PORT="${ES_PORT:-9200}"
ES_VERSION="${ES_VERSION:-8.13.4}"
ES_NAME="${ES_NAME:-versus-es}"
ES_PASSWORD="${ES_PASSWORD:-changeme}"
WAIT_SECS="${WAIT_SECS:-90}"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --host)     ES_HOST="$2";     shift 2 ;;
    --port)     ES_PORT="$2";     shift 2 ;;
    --version)  ES_VERSION="$2";  shift 2 ;;
    --name)     ES_NAME="$2";     shift 2 ;;
    --wait)     WAIT_SECS="$2";   shift 2 ;;
    -h|--help)  sed -n '2,18p' "$0"; exit 0 ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
done

probe_es() {
  curl -fsS --max-time 3 "$ES_HOST" >/dev/null 2>&1
}

wait_for_es() {
  local deadline=$(( SECONDS + WAIT_SECS ))
  while (( SECONDS < deadline )); do
    if probe_es; then
      return 0
    fi
    sleep 2
  done
  return 1
}

if probe_es; then
  echo "[ok] elasticsearch already reachable at $ES_HOST"
  exit 0
fi

if ! command -v docker >/dev/null 2>&1; then
  echo "docker not found. Install Docker Desktop (https://docs.docker.com/get-docker/) and retry." >&2
  exit 1
fi

if ! docker info >/dev/null 2>&1; then
  echo "docker daemon is not running. Start Docker Desktop and retry." >&2
  exit 1
fi

# Reuse existing container if present
if docker ps -a --format '{{.Names}}' | grep -qx "$ES_NAME"; then
  state="$(docker inspect -f '{{.State.Status}}' "$ES_NAME")"
  if [[ "$state" != "running" ]]; then
    echo "[start] container $ES_NAME exists ($state) — starting"
    docker start "$ES_NAME" >/dev/null
  else
    echo "[ok] container $ES_NAME already running"
  fi
else
  echo "[run] launching elasticsearch:$ES_VERSION as $ES_NAME on :$ES_PORT (security disabled)"
  docker run -d \
    --name "$ES_NAME" \
    -p "${ES_PORT}:9200" \
    -e "discovery.type=single-node" \
    -e "xpack.security.enabled=false" \
    -e "ES_JAVA_OPTS=-Xms512m -Xmx512m" \
    -e "ELASTIC_PASSWORD=$ES_PASSWORD" \
    "docker.elastic.co/elasticsearch/elasticsearch:$ES_VERSION" \
    >/dev/null
fi

echo "[wait] for $ES_HOST (timeout ${WAIT_SECS}s)"
if wait_for_es; then
  echo "[ok] elasticsearch is up at $ES_HOST"
  curl -s "$ES_HOST" | head -20 || true
else
  echo "[fail] elasticsearch did not become reachable within ${WAIT_SECS}s" >&2
  echo "       inspect logs: docker logs $ES_NAME" >&2
  exit 1
fi
