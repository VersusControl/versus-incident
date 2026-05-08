#!/usr/bin/env bash
# Builds the React/Vite UI into ui/dist so the Go binary can embed it
# via //go:embed. Safe to re-run; uses `npm ci` when package-lock.json
# is present and node_modules is missing/stale.
#
# Usage:
#   ./scripts/build_ui.sh                 # build UI only
#   ./scripts/build_ui.sh --with-go       # also rebuild the Go binary
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
UI_DIR="$REPO_ROOT/ui"

cd "$UI_DIR"

if [ ! -d node_modules ]; then
  if [ -f package-lock.json ]; then
    echo "▶ npm ci"
    npm ci
  else
    echo "▶ npm install"
    npm install
  fi
fi

echo "▶ npm run build"
npm run build

# Preserve the .gitkeep placeholder so the embed package still compiles
# from a fresh checkout even after `git clean`.
touch "$UI_DIR/dist/.gitkeep"

echo "✔ UI built: $UI_DIR/dist"

if [ "${1:-}" = "--with-go" ]; then
  cd "$REPO_ROOT"
  echo "▶ go build -o run ./cmd"
  go build -o run ./cmd
  echo "✔ Binary: $REPO_ROOT/run"
fi
