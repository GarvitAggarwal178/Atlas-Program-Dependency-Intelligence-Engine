# Atlas (formerly "symex")

## Purpose

Atlas maintains a set of derived program facts as a materialized view over a
Go codebase, incrementally updated as the codebase changes, where every
derived fact records the inputs it was derived from — so invalidation is
computed rather than hand-written, correctness is a checkable property rather
than an argued rule, and "when did this CVE become reachable, and what
caused it" is a query rather than a pipeline.

Full design authority: [`architecture.md`](architecture.md) (Atlas v3.1).
This file is a map of the territory, not a restatement of the doc — when in
doubt, architecture.md wins.

## Current status

The repo currently contains **v2** ("symex"): a working, tested, but
architecturally superseded engine — per-commit graph snapshots with
hand-written invalidation. It is being rebuilt to the Atlas v3.1 spec in
architecture.md's build order (§13). See [`docs/PROGRESS.md`](docs/PROGRESS.md)
for what has actually been completed, and
[`docs/FLAGGED.md`](docs/FLAGGED.md) for open questions blocking further
work.

**Do not treat this section as authoritative for "what works today" — read
PROGRESS.md, which is updated per session. This section describes the
overall shape, not the current build-order checkpoint.**

## Architecture summary (see architecture.md for the real thing)

Atlas is incremental view maintenance (§1) over a Go codebase:

- **Base relations**: files, symbols, type declarations, interfaces, module
  versions.
- **Derived relations**: call edges (CHA + RTA-gated), reachability, every
  fact recording its own derivation inputs.
- **A commit** is a delta on base relations; the engine derives view updates
  from deltas rather than hand-writing invalidation rules per trigger type.
- **The hard center** (§4): least-fixpoint reachability maintenance under
  deletion via Delete-Rederive (DRed) — support counting alone is unsound
  under self-supporting cycles.
- **Single-writer, bounded parallelism** (§3): one process walks a
  linearized (first-parent) commit sequence sequentially; parallelism exists
  only within a single delta's parse/derive phase.

## Directory layout

```
architecture.md          — Atlas v3.1 design doc, single source of truth
CLAUDE.md                — this file
docs/
  PROGRESS.md             — session log
  DECISIONS.md            — non-trivial implementation choices, mapped to arch sections
  FLAGGED.md              — ambiguities/gaps not resolved by guessing
  CHANGELOG.md            — one line per commit
cmd/
  symex/                  — stage-1 CLI: AST symbol extraction only
  symex-graph/            — end-to-end CLI: parse → build graph → diff → reachability
  symex-harness/          — differential harness CLI (incremental vs. full rebuild)
internal/
  symboltable/            — pure data model for stage-1 output (no I/O)
  canonicalize/           — structural hash for trivial-change detection
  parser/                 — go/packages-based extraction (already matches §6); poison-input gate (§3.2) added on top
  callgraph/              — CHA-style edge builder + interface expansion
  differ/                 — git diff → changed line ranges → changed symbols
  classifier/             — trivial / signature_change / logic_change classification
  incremental/            — v2's hand-written 4-step invalidation rule + differential harness
                            (frozen oracle once tagged — see docs/DECISIONS.md)
  store/                  — Postgres layer; v2 snapshot schema (facts/symbols/interface_dispatch_sites/type_ifaces)
                            plus, as they land, the v3.1 interval/derivation schema (§5)
testdata/fixture/        — synthetic Go repo used by parser/callgraph unit tests
chi/                      — untracked local clone of go-chi/chi, used as a real-world
                            harness fixture. Not project code. See docs/FLAGGED.md.
```

## Build / run / test

```bash
# Install deps, verify build, run non-DB tests
./scripts/bootstrap.sh

# Bring up Postgres (DSN: postgres://symex:symex@localhost:5434/symex?sslmode=disable)
docker-compose up -d

# Run the differential harness against a real repo
./scripts/run-harness.sh [/path/to/repo] [num-commits]

# Full test suite (requires Postgres for internal/store, internal/incremental)
go test ./...
```

## Coding conventions

- Standard library `go/*` packages preferred over hand-rolled parsing where
  possible; architecture.md §6 mandates migrating the frontend to
  `golang.org/x/tools/go/packages` — this has not happened yet (see
  docs/FLAGGED.md and docs/PROGRESS.md for status).
- Every table added under the v3.1 schema must map to a named section in
  architecture.md §5. Do not add a table without one.
- Every derived fact type added after the IMPLEMENTS probe (§8) requires
  the probe's falsification test to keep passing — a new fact kind that
  needs hand-written invalidation code means the model is broken, not that
  the new fact kind is special-cased.
- No Datalog DSL, no second language frontend, no multi-writer/distributed
  execution, no taint analysis — architecture.md §12 non-goals are binding.
