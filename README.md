# symex — Stage 1: AST Symbol Extractor

Stage 1 of the incremental call graph engine. Parses a Go repository using
`go/parser`, `go/ast`, and `go/types` (standard library only) and produces a
structured symbol table documenting every defined symbol and every call
expression across all `.go` files.

No external parsing dependency. No call graph yet — that is stage 2.

---

## Quick start

```bash
go build ./cmd/symex
./symex -repo /path/to/your/go/repo -pretty | head -100
```

Against the included test fixture:

```bash
./symex -repo testdata/fixture -out fixture_symbols.json
cat fixture_symbols.json | jq '.files[].defined[] | select(.kind == "interface")'
```

Run tests:

```bash
go test ./...
```

---

## Data model

### `RepoSymbolTable`

Top-level output. One `ModulePath` (from `go.mod`) and one `FileSymbolTable`
per parsed `.go` file.

### `FileSymbolTable`

```json
{
  "file_path": "billing/ledger.go",
  "package": "billing",
  "import_path": "github.com/example/fixture/billing",
  "defined": [ ... ],
  "references": [ ... ]
}
```

### `DefinedSymbol`

One entry per declaration in the file. The `kind` field is one of:

| kind        | what it is                              |
|-------------|----------------------------------------|
| `function`  | package-level function                  |
| `method`    | function with a receiver                |
| `interface` | interface type declaration              |
| `type`      | concrete type (struct or alias)         |

Key fields:

- **`qualified_name`** — canonical node key for the call graph: `pkg/path.Func`
  for functions, `pkg/path.(ReceiverType).Method` for methods. Used verbatim
  as node identifiers in stage 2.
- **`lines`** — 1-based inclusive start/end line of the declaration. Used in
  stage 3 (git diff integration) to map changed line ranges to changed symbols.
- **`canonical_hash`** — SHA-256 of the canonicalized function body (see below).
  Empty for `interface` and `type` symbols.
- **`method_set`** — populated only for `interface` symbols; lists method names.
- **`implemented_interfaces`** — populated only for `type` symbols; lists the
  qualified names of interfaces this type satisfies, computed via `go/types`.

### `CallReference`

One entry per call expression found inside a function/method body.

```json
{
  "caller": "github.com/example/fixture/payment.(Processor).ProcessPayment",
  "callee": "github.com/example/fixture/billing.Ledger.Debit",
  "kind": "interface_resolved",
  "line": 42
}
```

The `kind` field is the load-bearing distinction for call graph correctness:

---

## `direct_call` vs `interface_resolved` — the critical distinction

### `direct_call`

The callee is a concrete function or a method on a concrete (non-interface)
receiver. `go/types` resolved the call to exactly one `*types.Func`. The call
graph edge added in stage 2 will be precise.

**How we detect it:** `info.Uses[ident]` returns a `*types.Func` for simple
calls. For method calls, `info.Selections[selectorExpr]` gives a
`*types.Selection`; we then inspect `selection.Recv()` — if the underlying
type is not `*types.Interface`, the receiver is concrete.

**Examples:**
```go
billing.NewSQLLedger(dsn)        // direct_call: one concrete function
proc.ProcessPayment(amount)      // direct_call: proc is *payment.Processor (concrete)
formatAmount(n)                  // direct_call: intra-package concrete function
```

### `interface_resolved`

The callee is a method invoked on a value whose **static type is an interface**.
We cannot determine which concrete implementation will be called without a full
pointer / escape analysis, which is beyond the scope of static AST analysis.

**How we detect it:** `info.Selections[selectorExpr]` gives us a `*types.Selection`;
`selection.Recv()` unwrapped gives a `*types.Interface`. We know the method
name but not the concrete target.

**What happens in stage 2:** The call graph builder will add an edge from the
caller to *every concrete type in the repo that implements this interface*. This
is a known, intentional over-approximation — it is a soundness-over-precision
tradeoff. We may include spurious edges (dead code that happens to implement the
interface), but we will never miss a real one. The provenance tag
`interface_resolved` is preserved on every edge so the reachability engine can
propagate reduced confidence appropriately.

**This is documented here, not as a bug to fix later**, because the alternative
(points-to analysis) is orders of magnitude more complex and was explicitly
descoped from v1.

**Examples:**
```go
// p.ledger is billing.Ledger (interface) — interface_resolved
p.ledger.Debit(amount)
p.ledger.Credit(amount)
p.ledger.Balance()
```

---

## Canonical structural hash

### Purpose

The hash enables the semantic change classifier (stage 4) to distinguish:

- **Trivial change**: comment edit, local variable rename, whitespace. Hash
  unchanged → zero blast radius. The function's observable behavior to its
  callers is identical.
- **Logic change**: operator changed, new branch added, callee changed. Hash
  differs → run reachability from this symbol.

### Algorithm

1. **Clone** the function body by round-tripping through `go/printer` and
   `go/parser`. This gives a clean AST with no references to the original
   source positions.
2. **Seed the local renaming table** with parameter and named-result identifiers
   in declaration order (so `func F(a, b int)` seeds `a→_v0, b→_v1`).
3. **Walk the cloned body** and register every locally-declared identifier
   (short-var `:=`, `var` specs, range loop variables) in encounter order,
   assigning positional placeholders `_v0, _v1, …`.
4. **Second walk**: substitute every identifier that has a mapping with its
   placeholder. Package-level and imported names are **not** renamed — a change
   from calling `foo()` to `bar()` will change the hash even if `foo` and `bar`
   happen to have the same structure internally.
5. **Zero all source positions** so the printer output is independent of where
   in the file the function appeared.
6. **Print** the canonicalized AST with `go/printer`.
7. **SHA-256** the printed bytes → hex string.

### Invariants verified by tests

| test | expected outcome |
|------|-----------------|
| `total := x+y` vs `sum := x+y` | equal hashes (pure local rename) |
| function with doc comment vs without | equal hashes (comment stripped) |
| `x + y` vs `x * y` | different hashes (operator change) |
| `return foo(n)` vs `return bar(n)` | different hashes (callee change) |
| `if x < 0 { return 0 }` added | different hashes (control flow) |
| same source hashed twice | equal hashes (determinism) |
| parameter rename `(a,b int)` → `(x,y int)` | equal hashes |
| `return a, b` vs `return b, a` | different hashes (argument order) |

---

## What this stage does NOT do

- **No call graph construction.** Edges are built in stage 2.
- **No incremental update.** The full repo is re-parsed on each invocation. The
  incremental engine (stage 6) is built on top of this output, using the
  `source_file` field to know which edges to retract on a file change.
- **No cross-package interface satisfaction.** `implemented_interfaces` only
  lists interfaces declared in the same package. Cross-package satisfaction
  (e.g. a type in `payment` implementing an interface from `billing`) requires
  the type checker to have access to the compiled `billing` package via the
  importer. This works when dependencies are available; the extractor degrades
  gracefully (logs a warning, continues) when they are not.
- **No test file exclusion.** `_test.go` files are parsed and included. This is
  intentional: test functions call production code, and those edges belong in
  the test mapping (stage 7).

---

## Project layout

```
symex/
├── cmd/symex/main.go               # CLI entrypoint
├── internal/
│   ├── symboltable/types.go        # Data model (pure structs, no I/O)
│   ├── canonicalize/
│   │   ├── hash.go                 # Canonical hash computation
│   │   ├── parse_bridge.go         # go/parser bridge (avoids import cycle)
│   │   └── hash_test.go            # Unit tests for hash invariants
│   ├── parser/
│   │   ├── parser.go               # AST parsing + type checking + extraction
│   │   └── parser_test.go          # Integration test against fixture repo
│   └── modpath/modpath.go          # go.mod reader (no external dep)
├── testdata/fixture/               # Synthetic test repo
│   ├── go.mod
│   ├── billing/ledger.go           # Interface + two concrete implementations
│   ├── payment/processor.go        # Mix of direct + interface calls
│   └── store/store.go              # Cross-package calls + hash invariant cases
└── README.md
```

---

## Interview notes

**Q: Why `go/types` instead of syntactic heuristics for interface detection?**

Syntactic heuristics (e.g. "if the receiver type name starts with `I` it's
probably an interface") break on every real codebase. `go/types` gives us the
actual static type of every expression, including the receiver of every method
call. The distinction between `direct_call` and `interface_resolved` is
load-bearing for call graph precision: getting it wrong either inflates the
blast radius (false `interface_resolved`) or silently misses real dependencies
(`direct_call` when it should be `interface_resolved`). We pay the cost of
type-checking up front to get this right.

**Q: Why is interface dispatch over-approximated instead of precisely resolved?**

Precise resolution requires a whole-program points-to analysis (e.g. Andersen's
algorithm). That is a multi-pass fixpoint computation over the entire call graph
— a circular dependency on what we're computing. For v1, over-approximation
with `interface_resolved` provenance tags is the correct trade: it is sound
(no missed edges), the spurious edges are labeled so downstream stages can
report reduced confidence, and the validation harness (Gremlins, stage 9)
will measure how often those spurious edges actually matter.

**Q: Why SHA-256 the printer output rather than a custom AST walk?**

`go/printer` normalizes whitespace and formatting deterministically. The only
things that survive into the printed output are: operators, control flow
keywords, literal values, called function names, and the structural shape of
the AST. Exactly the things that should change the hash. A custom walk would
need to replicate that normalization and is more likely to miss an edge case.

---

## Stage 2: Call Graph Builder, Git Diff Integration, Reachability Engine

### New packages

```
internal/callgraph/   — graph builder + in-memory graph loader
internal/store/       — Postgres layer (schema DDL, insert, query)
internal/differ/      — git diff parser → changed symbol set
internal/reachability/— BFS with weakest-link provenance
cmd/symex-graph/      — end-to-end CLI
```

### Running end-to-end

```bash
# 1. Bring up Postgres
docker-compose up -d

# 2. Install deps and build
./scripts/bootstrap.sh

# 3. Full demo against chi (auto-clones, picks two adjacent commits)
./scripts/run-demo.sh

# 4. Or specify your own repo and commits
symex-graph build-and-analyze \
  -repo /path/to/your/repo \
  -dsn "postgres://symex:symex@localhost/symex?sslmode=disable" \
  -base <sha-before> \
  -head <sha-after>
```

### Output format

```json
{
  "repo": "/path/to/repo",
  "base_commit": "abc12345...",
  "head_commit": "def67890...",
  "graph_stats": { "total_edges": 412 },
  "diff": {
    "files_changed": 2,
    "changed_files": ["mux.go", "middleware/logger.go"]
  },
  "changed_symbol_set": [
    { "symbol": "github.com/go-chi/chi/v5.(Mux).Handle", "file": "mux.go" }
  ],
  "reachable_set": [
    {
      "symbol": "github.com/go-chi/chi/v5.(Mux).ServeHTTP",
      "path": [
        "github.com/go-chi/chi/v5.(Mux).Handle",
        "github.com/go-chi/chi/v5.(Mux).ServeHTTP"
      ],
      "provenance": "direct_call",
      "depth": 1
    },
    {
      "symbol": "github.com/go-chi/chi/v5.(Mux).routeHTTP",
      "path": [
        "github.com/go-chi/chi/v5.(Mux).Handle",
        "github.com/go-chi/chi/v5.(Mux).ServeHTTP",
        "github.com/go-chi/chi/v5.(Mux).routeHTTP"
      ],
      "provenance": "interface_resolved",
      "depth": 2
    }
  ],
  "summary": {
    "total_reachable": 18,
    "direct_path_count": 11,
    "interface_resolved_path_count": 7
  }
}
```

---

## Why path confidence degrades to the weakest link, not an average

This is the most frequently challenged design decision. Here is the precise answer.

### The claim

If a path from changed symbol A to symbol D is:

```
A --direct_call--> B --interface_resolved--> C --direct_call--> D
```

then D's confidence is `interface_resolved`, even though two of the three edges are `direct_call`.

### Why averaging is wrong

An average would say: "2 out of 3 edges are certain, so this path is 66% confident."
But confidence here is not a frequency — it is a statement about a logical chain.

The path claims: "if A changes, A might affect D, via B and C."
That claim is only as strong as its weakest step. Specifically:

- A→B is certain: go/types resolved this to exactly one concrete function.
- B→C is uncertain: B calls through an interface, and we don't know at static analysis time which concrete type will be dispatched. We added the edge B→C because C is *one possible* implementation, not the only one.
- C→D is certain: another concrete call.

The overall claim "A affects D" rests on the uncertain middle step. If B→C is an over-approximation — if, at runtime, B never actually dispatches to C — then the entire chain A→B→C→D never executes and D is not actually affected. No amount of certainty in A→B or C→D changes that.

This is the conjunction rule from probability: P(A and B) ≤ min(P(A), P(B)). The probability of a chain of events is bounded by the probability of its least likely link.

### Why this matters for test selection

Symbols reached via `direct_call`-only paths go in the **"must run"** tier: we are certain they are reachable from the changed code.

Symbols reached via any `interface_resolved` edge go in the **"consider running"** tier: we added those edges as an over-approximation. Running those tests is safer than not running them, but we are honest that the justification is less certain.

**Averaging would blur this distinction.** A path with 10 direct edges and 1 interface edge would get 91% "confidence" and might be rounded up to "must run". But that single interface edge means the whole chain is uncertain — it belongs in "consider running."

### The alternative would require points-to analysis

The only way to make interface dispatch precise is a whole-program points-to (alias) analysis — a fixed-point computation that tracks which concrete types flow into which variables. That is significantly more complex and was explicitly out of scope for v1. The weakest-link rule is the correct conservative behavior given the soundness-over-precision choice made in stage 1.

---

## Postgres schema

```sql
-- Every call-graph edge. Primary key makes re-runs idempotent.
CREATE TABLE call_edges (
    repo            TEXT NOT NULL,
    commit_hash     TEXT NOT NULL,
    source_symbol   TEXT NOT NULL,
    target_symbol   TEXT NOT NULL,
    provenance      TEXT NOT NULL,   -- 'direct_call' | 'interface_resolved'
    source_file     TEXT NOT NULL,
    PRIMARY KEY (repo, commit_hash, source_symbol, target_symbol)
);

-- Symbol line spans: used by the differ to map changed lines → changed symbols.
CREATE TABLE symbols ( ... );

-- Reverse index for interface dispatch: used by the incremental engine (stage 3).
CREATE TABLE interface_dispatch_sites ( ... );
```

Full DDL: `internal/store/store.go` → `Schema` constant.

---

## Correctness invariants

1. **Incremental == full rebuild** (stage 3, not yet built): the BFS result from
   an incrementally-updated graph must be identical to the result from a full
   rebuild at the same commit.
2. **No symbol is reachable from itself through zero-length paths**: the frontier
   symbols are seeded into `visited` before BFS starts, so they cannot be
   re-enqueued. Self-loops produce a single BFS step that sees the target already
   in `visited` and skips it.
3. **Every edge has exactly one provenance tag**: enforced by the store schema
   (`NOT NULL`) and by the builder (always set from `CallReference.Kind`).
4. **`must_run` never includes an `interface_resolved`-only path**: enforced by
   the weakest-link rule — any `interface_resolved` edge on any path degrades
   the whole path, so the symbol ends up counted in `InterfaceResolvedCount`.

---

## Stage 3: Semantic Change Classifier

### New package: `internal/classifier/`

Classifies each symbol in the changed set as `trivial`, `signature_change`, or `logic_change`. Trivial symbols are excluded from the reachability frontier — zero blast radius.

### Classification algorithm

```
ClassifySymbol(fetcher, baseCommit, headCommit, filePath, symbolName):
  1. Fetch file source at baseCommit and headCommit via git show <sha>:<path>
  2. Parse both versions; locate the FuncDecl by (funcName, receiverType)
  3. Compute canonical hash of each body (comments stripped, locals alpha-renamed)
  4. Extract signature string of each: types-only, no parameter names
     e.g. "(int, string) error" — not "(amount int, label string) error"
  5. Classify:
     - beforeSig != afterSig  → signature_change (callers affected regardless of body)
     - beforeHash == afterHash → trivial (no structural change)
     - else                   → logic_change
```

### Why canonicalization prevents false positives

Without alpha-renaming, `total := x+y` and `sum := x+y` produce different printed ASTs and would incorrectly classify as `logic_change`. After renaming both locals to `_v0`, the bodies are structurally identical and hash to the same value.

**The critical test case — logic change disguised as rename:**

```go
// Before: total := x + y; return total
// After:  sum   := x * y; return sum  ← rename AND operator change
```

After canonicalization:
- Before: `_v2 := _v0 + _v1; return _v2`
- After:  `_v2 := _v0 * _v1; return _v2`

The operator difference survives → `logic_change`. A naive implementation that stops at "both have the same local count" would misclassify this as trivial.

### Test cases (all pass)

| Test | Input | Expected | Why |
|------|-------|----------|-----|
| `TestPureRenameIsTrivial` | `total` → `sum`, same operator | trivial | alpha-rename makes identical |
| `TestPureCommentEditIsTrivial` | doc comment added | trivial | comments stripped before hash |
| `TestParameterTypeChangeIsSignatureChange` | `int` → `int64` | signature_change | signature string differs |
| `TestLogicChangeDisguisedAsRename` | rename + `+` → `*` | logic_change | operator survives canonicalization |
| + 11 table-driven cases | various | per-case | covering whitespace, return type, callee change, etc. |

### Wiring into the pipeline

```
diff → changed symbol set (raw)
           ↓
     ClassifyAll()
           ↓
  classifications map + filtered frontier
  (trivial excluded from frontier)
           ↓
     BFS reachability
  (starts from filtered frontier only)
```

---

## Stage 4: Incremental Update Engine

### The correctness problem (why naive is wrong)

```
File A:  proc.ledger.Debit()   ← interface_resolved call through Ledger
File B:  type SQLLedger struct  ← implements Ledger
```

Naive incremental: B changes → retract B's edges, reparse B, reinsert.
Result: A's interface_resolved edges still point to the old implementors.
If a new type `CacheLedger` in B now implements Ledger, A's edges are WRONG.
A never changed, so a naive implementation never touches A.

### The 4-step rule

When file F changes from commit A to commit B:

**Step 1 — Retract F's data:**
```sql
DELETE FROM call_edges WHERE source_file = F AND commit_hash = B
DELETE FROM symbols WHERE file_path = F AND commit_hash = B
DELETE FROM interface_dispatch_sites WHERE call_site_file = F AND commit_hash = B
DELETE FROM type_ifaces WHERE source_file = F AND commit_hash = B
```

**Step 2 — Find affected interfaces (the hard part):**
Load `type_ifaces` for F at the BASE commit (before retraction). Compare with F's NEW types from the re-parsed symbol table. Any interface whose implementer set changed → find every call site dispatching through it in `interface_dispatch_sites` (ANY file). Retract those call sites' `interface_resolved` edges.

**Step 3 — Reinsert F's new data:**
Parse F at headCommit. Insert new edges, symbols, dispatch_sites (with `method_name`), type_ifaces.

**Step 4 — Recompute cross-file edges:**
For each call site from step 2, re-expand using the updated implementer index. Insert the corrected `interface_resolved` edges.

### Schema additions

```sql
-- Persists which interfaces each type implements, per commit.
-- Needed to detect implementer-set changes without re-parsing the base commit.
CREATE TABLE type_ifaces (
    repo TEXT, commit_hash TEXT, type_name TEXT, interface_name TEXT, source_file TEXT,
    PRIMARY KEY (repo, commit_hash, type_name, interface_name)
);

-- Added method_name to dispatch sites.
-- Needed in step 4 to re-expand without re-parsing the call-site's file.
ALTER TABLE interface_dispatch_sites ADD COLUMN method_name TEXT;
```

### Differential test harness

```bash
symex-harness \
  -repo /path/to/local/clone \
  -dsn "postgres://symex:symex@localhost/symex?sslmode=disable" \
  -commits 25 \
  -out harness_results.json
```

For each of the 25 commits:
1. Applies incremental update from previous commit's graph
2. Runs full rebuild from scratch
3. Loads both edge sets and diffs them with `store.DiffEdgeSets`
4. Reports PASS/FAIL + exact mismatched edges on failure

**Expected output (passing run):**
```
[1/24] abc12345..def67890
  changed files: [mux.go]
  PASS  incr=412 edges, full=412 edges, incr_time=180ms full_time=2.1s speedup=11.7x

[2/24] def67890..89abcdef
  changed files: [middleware/logger.go]
  PASS  incr=419 edges, full=419 edges, incr_time=95ms full_time=2.0s speedup=21.1x
...
════════════════════════════════════════
Differential Harness Results
Commits tested: 24
PASS: 24  FAIL: 0  ERROR: 0
Avg speedup (full/incr): 14.3x
════════════════════════════════════════
```

**On failure, exact diff is printed:**
```
[8/24] ...
  FAIL  incr=415 edges, full=417 edges
  Only in FULL BUILD (missing from incremental):
    [interface_resolved] pkg.Caller → pkg.(NewType).Method
    [interface_resolved] pkg.OtherCaller → pkg.(NewType).Method
```

This tells you exactly which edges the incremental engine missed and why: a new type started implementing an interface whose call sites in other files weren't recomputed (step 2 failed).

### New packages

| Package | Role |
|---|---|
| `internal/incremental/engine.go` | 4-step incremental update rule |
| `internal/incremental/harness.go` | Differential correctness harness |
| `internal/store/incremental.go` | File-level retraction, CopyGraphToCommit, DiffEdgeSets |
| `internal/store/schema_v2.go` | type_ifaces table, DispatchSiteV2, SchemaV2 DDL |
| `cmd/symex-harness/main.go` | Harness CLI |

### Running the harness

```bash
# Start Postgres
docker-compose up -d

# Bootstrap
./scripts/bootstrap.sh

# Run harness against chi (recommended: small, active, good interface usage)
git clone https://github.com/go-chi/chi.git /tmp/chi-harness
symex-harness -repo /tmp/chi-harness -dsn "postgres://symex:symex@localhost/symex?sslmode=disable" -commits 25

# Run against a mid-sized repo (caddy, ~15k lines, heavier interface use)
git clone https://github.com/caddyserver/caddy.git /tmp/caddy-harness
symex-harness -repo /tmp/caddy-harness -dsn "..." -commits 20
```
