#!/usr/bin/env bash
# run-demo.sh — end-to-end demo of stage 2 against a real Go repo.
#
# What this does:
#   1. Clones chi (a small, well-structured Go HTTP router) if not already present.
#   2. Starts Postgres via docker-compose (if not already running).
#   3. Builds the symex-graph binary.
#   4. Picks two adjacent commits from chi's history.
#   5. Runs build-and-analyze, printing the full JSON output.
#
# Requirements: git, docker, docker-compose, Go 1.21+
#
# Usage: ./scripts/run-demo.sh [/path/to/target-repo] [base-commit] [head-commit]
#   All args are optional; defaults use chi.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

TARGET_REPO="${1:-/tmp/symex-demo-chi}"
DSN="${SYMEX_DSN:-postgres://symex:symex@localhost:5432/symex?sslmode=disable}"

# ── Step 1: ensure the target repo exists ──────────────────────────────────────
if [ ! -d "$TARGET_REPO/.git" ]; then
  echo "[demo] Cloning go-chi/chi into $TARGET_REPO ..."
  git clone --depth=100 https://github.com/go-chi/chi.git "$TARGET_REPO"
fi

# ── Step 2: pick commits if not supplied ───────────────────────────────────────
BASE_COMMIT="${2:-}"
HEAD_COMMIT="${3:-}"

if [ -z "$BASE_COMMIT" ] || [ -z "$HEAD_COMMIT" ]; then
  echo "[demo] Selecting two adjacent commits from repo history..."
  # Get the last 10 commits and pick two adjacent ones that touch Go files.
  HEAD_COMMIT=$(git -C "$TARGET_REPO" log --oneline --diff-filter=M -- '*.go' \
    | head -1 | awk '{print $1}')
  BASE_COMMIT=$(git -C "$TARGET_REPO" log --oneline --diff-filter=M -- '*.go' \
    | head -2 | tail -1 | awk '{print $1}')

  if [ -z "$HEAD_COMMIT" ] || [ -z "$BASE_COMMIT" ]; then
    echo "[demo] Could not find two commits with Go file changes."
    echo "[demo] Try: $0 /path/to/repo <base-sha> <head-sha>"
    exit 1
  fi
fi

echo "[demo] Base commit: $BASE_COMMIT"
echo "[demo] Head commit: $HEAD_COMMIT"
echo "[demo] Diff preview:"
git -C "$TARGET_REPO" diff --stat "$BASE_COMMIT" "$HEAD_COMMIT" -- '*.go' || true
echo ""

# ── Step 3: start Postgres if needed ──────────────────────────────────────────
if ! pg_isready -h localhost -p 5432 -U symex -d symex -q 2>/dev/null; then
  echo "[demo] Starting Postgres via docker-compose..."
  docker-compose -f "$ROOT/docker-compose.yml" up -d
  echo "[demo] Waiting for Postgres to be ready..."
  for i in $(seq 1 30); do
    pg_isready -h localhost -p 5432 -U symex -d symex -q && break
    sleep 1
  done
fi

# ── Step 4: build the binary ──────────────────────────────────────────────────
echo "[demo] Building symex-graph..."
cd "$ROOT"
go mod tidy
go build -o /tmp/symex-graph ./cmd/symex-graph

# ── Step 5: run build-and-analyze ─────────────────────────────────────────────
echo "[demo] Running build-and-analyze..."
echo ""
/tmp/symex-graph build-and-analyze \
  -repo "$TARGET_REPO" \
  -dsn "$DSN" \
  -base "$BASE_COMMIT" \
  -head "$HEAD_COMMIT" \
  -pretty 2>&1
