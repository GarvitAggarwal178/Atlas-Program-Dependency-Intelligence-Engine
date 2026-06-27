#!/usr/bin/env bash
# scripts/run-harness.sh — run the differential correctness harness.
#
# Clones go-chi/chi (small, well-structured, active interface usage),
# builds symex-harness, and runs 25 commits through the differential test.
#
# Usage: ./scripts/run-harness.sh [/path/to/repo] [num-commits]
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
REPO="${1:-/tmp/symex-harness-chi}"
COMMITS="${2:-25}"
DSN="${SYMEX_DSN:-postgres://symex:symex@localhost:5434/symex?sslmode=disable}"

# Clone if not present
if [ ! -d "$REPO/.git" ]; then
  echo "[harness] Cloning go-chi/chi → $REPO"
  git clone --depth=200 https://github.com/go-chi/chi.git "$REPO"
fi

# Start Postgres if needed
if ! pg_isready -h localhost -p 5434 -U symex -d symex -q 2>/dev/null; then
  echo "[harness] Starting Postgres..."
  docker-compose -f "$ROOT/docker-compose.yml" up -d
  for i in $(seq 1 30); do pg_isready -h localhost -p 5434 -U symex -d symex -q && break; sleep 1; done
fi

# Build
cd "$ROOT"
go mod tidy
go build -o /tmp/symex-harness ./cmd/symex-harness

echo "[harness] Running $COMMITS commits against $REPO"
echo ""

/tmp/symex-harness \
  -repo "$REPO" \
  -dsn "$DSN" \
  -commits "$COMMITS" \
  -out /tmp/harness-results.json

echo ""
echo "[harness] Full JSON results: /tmp/harness-results.json"
