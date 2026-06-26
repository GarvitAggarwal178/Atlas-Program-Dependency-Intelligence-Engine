#!/usr/bin/env bash
# bootstrap.sh — install dependencies and verify the project compiles.
# Run this once after cloning, before running tests or the demo.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

echo "[bootstrap] Running go mod tidy..."
go mod tidy

echo "[bootstrap] Verifying build..."
go build ./...

echo "[bootstrap] Running tests (no Postgres required)..."
go test ./internal/canonicalize/... ./internal/differ/... ./internal/reachability/... ./internal/callgraph/...

echo ""
echo "[bootstrap] OK — all packages compile and non-DB tests pass."
echo ""
echo "To run the full end-to-end demo against a real repo:"
echo "  ./scripts/run-demo.sh"
echo ""
echo "To run with a specific repo and commits:"
echo "  ./scripts/run-demo.sh /path/to/repo <base-commit> <head-commit>"
