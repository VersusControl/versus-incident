#!/usr/bin/env bash
# Continuously rebuilds ui/dist on every source change, using vite's
# built-in watch mode. Pair this with `./run` (the Go server) running
# in another terminal — the embedded FS is read at request time, so
# refreshing the browser picks up the new bundle.
#
# Note: this rebuilds the static bundle only. For a hot-reload dev
# experience (HMR, instant updates without full reload), use the Vite
# dev server instead:
#
#   cd ui && npm run dev      # http://localhost:5173 with /api proxy
#
# Use this script when you want to test the *embedded* path the binary
# will actually ship with.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
UI_DIR="$REPO_ROOT/ui"

cd "$UI_DIR"

if [ ! -d node_modules ]; then
  if [ -f package-lock.json ]; then
    npm ci
  else
    npm install
  fi
fi

echo "▶ vite build --watch (Ctrl-C to stop)"
exec npx vite build --watch --mode production
