
# Atlas v3.1 — Architecture

**Status:** authoritative. Supersedes v3 and the SymEx v2 blueprint. Written to be read cold, by a human or a model, with no access to prior conversation.

**What changed from v3.** An external review found three genuine defects — a soundness bug in the invalidation model, a circular memoization key, and no crash-consistency story — plus a class of input corruption that the correctness harness structurally cannot catch. All are fixed below. The same review raised a stratification alarm that is incorrect; §4.2 explains why, and replaces it with the sharper question it was gesturing at. §16 records what was rejected and why, so the same objections do not get re-litigated.

---

## 0. The one-sentence identity

> Atlas maintains a set of derived program facts as a **materialized view over a Go codebase**, incrementally updated as the codebase changes, where every derived fact records the inputs it was derived from — so invalidation is computed rather than hand-written, correctness is a checkable property rather than an argued rule, and "when did this CVE become reachable, and what caused it" is a query rather than a pipeline.

Every component below is a consequence of that sentence. If a proposed addition is not, it does not belong in the project.

**Framing rule, load-bearing.** The README leads with **the engine and its semantics** — least-fixpoint maintenance of reachability under deletion. The differential harness and the measurements are a section in the middle, never the thesis. Reason: three projects in this portfolio (Parallax, SpecGuard, Atlas) can all be skimmed as "harness that differentially compares X to Y." Atlas is the only one that is a *system with maintained state*, where measurement is how it is validated rather than what it produces. That distinction is real and it is invisible unless the framing makes it visible.

---

## 1. The reframe: this is incremental view maintenance

Not an analogy — a literal restatement, in the vocabulary of a database subfield with forty years of theory behind it.

| Atlas concept | IVM concept |
|---|---|
| Files, symbols, type declarations, method sets, implementer sets, dispatch sites, module versions | **Base relations** |
| `implements`, call edges (CHA tier), reachable set, RTA-gated edge view, vulnerable-symbol reachability | **Derived relations (views)** |
| A commit | A **delta** on base relations |
| The incremental update rule | **View maintenance under a delta** |
| The differential correctness harness | The standard IVM criterion: *maintained view == recomputed view* |

**What adopting the frame buys:**

- **CHA vs. RTA-gated** stops being two algorithms and becomes **two views over the same base relations**, one with an additional join against the reachable set. The v2 instinct — never delete CHA facts, expose RTA as an opt-in read path — was already this.
- **Vulnerability impact** stops being a bolted-on module and becomes a **consumer**: a query over the maintained view plus a temporal predicate.
- **Dependency-version re-indexing (v2 §5.5)** stops being a hand-argued rule and becomes **an ordinary delta**. A module version changes → facts sourced from that module have their support withdrawn → the engine re-derives → bidirectional interface recheck happens *because the engine follows recorded dependencies*.

That last point is the diagnostic that the frame is correct. v2 §5.5 required enormous care precisely because it was hand-writing what should have been derived.

---

## 2. Core redesign — three changes

### 2.1 Validity intervals replace per-commit snapshots

**Wrong in v2.** `PRIMARY KEY (repo, commit_hash, ...)` stores a full graph snapshot per commit. A fact surviving 500 commits occupies 500 rows. And "when did this change" requires diffing adjacent snapshots — which is why v2 needed a `reachability_snapshots` table plus a diff pipeline to answer the project's *headline* question. The data model fought the headline claim.

**Replacement.** A fact carries a half-open validity interval `[valid_from, valid_to)` over a linearized commit sequence. One row per fact lifetime.

**Deletes:** per-commit duplication; the `reachability_snapshots` table; v2 §5.4's flip-detection logic; the distinction between "current state" and "history."

**Linearization, and its two honest limits.**

Git history is a DAG; intervals need a total order. Atlas linearizes via a **first-parent walk of one named branch**. Two consequences that must be stated before a reviewer finds them:

1. **Rebase and force-push silently invalidate every interval.** Nothing in a naive design detects history rewriting. Mitigation: store a **linearization fingerprint** — a rolling hash of the first-parent commit-hash chain — and verify it at the start of every index run. Mismatch ⇒ refuse to proceed and require a full re-index. Cheap, and it converts silent corruption into a loud failure.
2. **On merge-based workflows, attribution is PR-granular, not commit-granular.** First-parent walk collapses each merged PR to a single `seq`. Most significant Go repos work this way. The honest claim is: *"commit-granular attribution on linear histories; PR-granular on merge-based ones."* Arguably PR-granularity is what a user wants — but say it rather than let it be discovered.

Multi-branch temporal modeling is explicitly out of scope. It is a real problem; it is not this project's problem.

### 2.2 Invalidation becomes data, not code — with the soundness fix

**Wrong in v2.** Invalidation is hand-written: the Stage 4 rule, the interface-dispatch reverse index, the §5.5 module rule. Three costs:

1. **The differential harness can only catch under-invalidation.** Retract too little → divergence → detected. Retract too much → the graph still matches → **invisible**. Over-invalidation is undetectable by the project's only correctness instrument, on a project whose entire premise is not redoing work.
2. Every new trigger type is a new rule requiring independent argument and validation.
3. The rule cannot be stated as a specification, so the implementation cannot be checked against one.

**Replacement.** Every derived fact records its inputs. Invalidation is derived: withdraw exactly the facts whose recorded inputs changed.

**⚠️ The soundness bug this had in v3, and its fix.** v3 specified the `INTERFACE` input hash as a **method-set** hash. That is wrong, and it produces silent under-derivation:

> Fact `F = (A.f → B.g, interface_resolved)`, dispatch site in file `X`, derived from `FILE(X)`, `INTERFACE(I, …)`, `TYPE(B, …)`. A commit adds type `C` in a *new* file `Y`, also implementing `I`. The edge `A.f → C.g` must now exist. But `X` is unchanged, `I`'s method set is unchanged, and `B` is unchanged — **nothing invalidates, the edge is never derived, and CVE reachability through `C` is a false negative forever.**

**Fix: the `INTERFACE` input hash is a hash over the interface's *implementer set*, not its method set.** Adding `C` changes the implementer set of `I` → every dispatch site through `I` is withdrawn → re-derivation produces the `C` edge.

**Why this does not require a Datalog engine.** The objection to provenance-based invalidation is that provenance gives you *retract*, not *derive* — you need declarative rule bodies for the engine to know that a delta on `implements` should trigger re-evaluation. But hashing the implementer set converts the derive case into **retract-then-rederive**, which provenance handles natively. No DSL, no subscription registry, no rule language.

**The cost, stated plainly:** this over-invalidates. Any new implementer of `I` anywhere withdraws every dispatch site through `I`, including sites that gain no new edge. That is the correct trade — **under-derivation is a correctness bug; over-invalidation is a number**, and §10.1 is the instrument that measures it. Sound first, precision measured.

**Required property of the implementer-set hash:** stable (independent of iteration order and of unrelated code), cheap to compute, and incrementally updatable — maintained as its own derived relation, not recomputed by scanning the program. If this property cannot be achieved, see §15's kill criterion.

**Explicitly not built: a Datalog DSL or query language.** That re-implements Soufflé. Rules stay ordinary Go functions; the discipline is only that a rule declares its inputs and the engine records provenance.

### 2.3 Backward validation, not content-addressed memoization

**⚠️ v3 specified a broken mechanism.** The cache key was `H(analyzer_version, file_structural_hash, identities+hashes of every resolved type consulted)`. "Types consulted" is known only **after** running the derivation, so the key cannot be computed to decide whether to skip. Genuine circularity. v3 conflated content-addressed memoization with Salsa's early cutoff and picked the one that does not apply here.

**Replacement — two-phase, backward validation.** This is what Salsa actually does: given a previous result, check whether each recorded dependency changed.

1. Look up prior derivations for the unit of work (a file) keyed on `(analyzer_version, file_structural_hash)`.
2. For each recorded input in `derivations`, compare the recorded hash to the current hash.
3. All match ⇒ **hit**, reuse the existing facts, extend their intervals, do no parsing or type-checking. Any mismatch ⇒ **miss**, re-derive.

`derivation_cache` as a separate table becomes largely redundant with `facts` + `derivations` and should only exist if cross-branch content sharing is actually wanted. Default: **delete it**, consistent with §17's goal of a smaller system. If retained, it needs an eviction policy — v3 specified an unbounded, monotonically growing JSONB store, which on a large repo's full history is the dominant storage cost.

**Borrowed from Salsa, deliberately:** durability tiers. Salsa marks crates.io inputs high-durability and workspace inputs low-durability so stable inputs short-circuit validation cheaply. Atlas's main-module vs. dependency-module split is the same distinction: **dependency facts are validated by module version alone**, never by re-hashing every file in the module.

**Reporting note:** cache-hit rate is a *diagnostic*, not a headline metric. It measures how much redundancy exists in git history — it will be high on any repo — not how good the engine is. Report it; do not put it in a resume bullet.

### 2.4 The frontend seam

Everything above — interval store, derivation tracking, delta engine, backward validation, DRed, differential harness — is **language-agnostic**. `go/packages`, `go/types`, CHA, interface method sets are the **Go frontend**.

Draw the seam explicitly. Then **do not build a second frontend.** The seam's value is that it forces you to state what the engine is. *"I designed the frontend boundary and deliberately shipped one implementation"* is a stronger answer than a half-finished second language.

---

## 3. Execution model — single-writer, bounded parallelism

v3 omitted this entirely and then asserted an invariant ("determinism under parallel construction") over a concurrency model that did not exist. Fixed:

- **One writer process** walks the linearized commit sequence and applies deltas **sequentially**. There is no queue, no multi-writer contention, no distributed coordination, no duplicate or out-of-order delivery. Say this out loud when asked — *"there is one writer walking a total order, so the question doesn't arise"* is a good answer, and leaving it ambiguous invites a bad one.
- **Parallelism exists within a delta**, not across deltas: file parsing and per-file fact derivation are goroutine-parallel; the database write for a delta is a single sequential transaction.
- **Determinism is therefore a property of the parse/derive phase**, and is meaningfully testable: the derived fact set must be identical regardless of worker count and scheduling order. This is the corrected form of v3's invariant 6 — it was unenforceable as written because it presumed a model that did not exist; it is enforceable now because the model is stated.
- **`seq` is the only ordering.** `commits.authored_at` is decorative and must never appear in an `ORDER BY` — git author timestamps are attacker-controllable and routinely non-monotonic.

### 3.1 Crash consistency

**Absent in v3, and the cheapest correctness win in the project.**

A delta at `seq = N` must close intervals, insert facts, insert derivations, adjust reachability support, and record that `N` was applied. Without a recorded watermark, a crash mid-delta leaves a store that is neither `N−1` nor `N`, and **nothing can detect it** — the harness would catch divergence at some later commit, with no way to know when it was introduced.

- `repo_state(repo, last_applied_seq)`, updated **in the same transaction** as the fact writes.
- On startup, resume from `last_applied_seq + 1`.
- Any cache/derivation write participates in the same transaction, or cached reads are validated against `facts` before use. A cache entry committed without its facts causes silent under-derivation on retry.
- **Test:** SIGKILL injection at randomized points *within* delta application, ≥500 trials, asserting recovery to a clean, known `seq`. Killing only between commits proves nothing; state the injection-point distribution.

### 3.2 Poison input — the failure the harness cannot catch

**The most likely defect to actually bite, and structurally invisible to differential testing.**

`go/packages` does not fail on a broken build. It returns packages with `Errors` populated and incomplete `TypesInfo`. Derive facts from a partially-typed package and you write **wrong facts with valid derivations** — and the differential harness *passes*, because the full rebuild is wrong in exactly the same way. Both sides agree; both are wrong.

A large fraction of historical commits in real Go repos do not build under a current toolchain (toolchain drift, removed dependencies, dead module proxies).

**Hard gate:**
- Before deriving, assert every loaded package has zero `Errors` and complete `TypesInfo`. Any error ⇒ **refuse to index the commit**, record it in a `skipped_commits` table with the reason.
- **The skip rate is part of every reported result**, not a footnote. It bounds N for every measurement in §10.
- Toolchain pinning per historical commit is real work and is the thing most likely to be underestimated. Budget it as a first-class task, not a preliminary.

---

## 4. The hard center: least-fixpoint reachability under deletion

The intellectual core. The one genuinely hard part. **If you build one thing from this document, build this.**

### 4.1 Why deletion is hard

Insertion is monotone and easy. Deletion is not: a symbol is typically reachable through many derivation paths. Delete one constructor call — is the type still reachable? Naive propagation says no and over-deletes.

This is what **DRed (Delete–Rederive)** exists to solve. It appears **twice** in Atlas, because the RTA-gated view sits on top of reachability: a reachability change alters which `interface_resolved` edges surface, which can alter reachability again.

### 4.2 The real question: which fixpoint — and why stratification is a non-issue

Write the program out:

```
reachable(X)      :- entry(X).
reachable(Y)      :- reachable(X), edge_visible(X,Y).
edge_visible(X,Y) :- cha_edge(X,Y), provenance = direct.
edge_visible(X,Y) :- cha_edge(X,Y), provenance = interface_resolved,
                     reachable(constructor_of(type(Y))).
```

**Every rule is positive.** No negation, no aggregation anywhere in the recursive cycle. The program is monotone, so by Knaster–Tarski the least fixpoint exists and is unique, and DRed's published scope explicitly covers general recursion. **Stratification is a constraint about negation inside recursion; there is no negation here, so there is no theorem gap and no need for a formal model to establish one.**

**The sharper question the stratification alarm was gesturing at, and the one you must be able to answer:** the semantics is the **least** fixpoint, and naive support counting maintains **a** fixpoint that is not the least one. That is precisely and exactly what the self-supporting cycle is:

> `A` and `B` call each other; both reachable from `main`. Delete the only edge `main → A`. Under support counting, `A` still has support from `B`, `B` still has support from `A`, neither count reaches zero, and a dead region of the graph stays alive **forever**, with no mechanism that will ever correct it. Every downstream CVE verdict through that region is a permanent false positive.

The greatest fixpoint contains the cycle. The least fixpoint does not. DRed maintains the least fixpoint; support counting does not. That is the entire content of §4.4's fixture, and it is the correct way to state it on a whiteboard.

**The one design rule that keeps stratification a non-issue:** express the RTA gate **positively** — *surface the edge if the target type is reachable*. Never as exclusion-by-negation — *hide edges whose target is not reachable*. Semantically identical; the second form puts negation inside the recursive cycle and *then* the whole stratification question becomes real. Also: never introduce a derived `unreachable` or `dead_code` relation into the cycle. If either rule is ever broken, revisit this section before writing code.

### 4.3 The algorithm, and the one competitor to name

Three phases per delta:

1. **Over-delete.** Withdraw directly-invalidated facts, then transitively withdraw everything whose support depended on them, ignoring alternative derivations. Deliberately pessimistic.
2. **Rederive.** From the surviving fact set, re-derive whatever is still reachable through untouched paths. This is where the cycle problem resolves: a cycle with no surviving external entry re-derives nothing and stays dead.
3. **Insert.** Propagate new facts monotonically.

**Named, not implemented — and one of these you must be able to discuss.**

- **`DRed^c`** (Hu, Motik & Horrocks, 2018): separate recursive and non-recursive derivation counters; deletion propagation stops early at facts with nonzero non-recursive support, and rederivation becomes a one-step counter check.
- **B/F — the backward/forward algorithm** (Motik, Nenov, Piro & Horrocks, AAAI 2015). ⚠️ **This is the one an informed interviewer will ask about, and v3 omitted it.** B/F exists specifically because DRed is inefficient when facts have many alternative derivations — which is *exactly* the call-graph reachability case, where a symbol is typically reachable through many paths. The expected question is verbatim: *"Why DRed and not backward/forward, given your fact density?"*
- **FBF** (Motik et al., ~2019) — the later refinement; reportedly also presents counting that handles recursive rules, which would make v3's "counting is for non-recursive views" phrasing accurate about the 1993 paper but stale as a claim about the literature.

**Verification status:** `DRed^c` was confirmed against secondary literature. **B/F and FBF were not verified directly — confirm authors, venue, and the efficiency claim from primary sources before citing either.** Do not repeat a citation you have not opened; that is the exact failure mode this project's discipline exists to prevent.

**The defensible answer to "why DRed":** *"I implemented the baseline mechanism to understand it. B/F is the known optimization for high-derivation-density workloads, which mine is; I did not implement it because I had no measured bottleneck to justify it, and optimizing without one is how you end up with a citation instead of a result."* That answer requires you to actually have the profile from §10.2.

### 4.4 The fixture, written first

A mutually recursive pair reachable through exactly one external edge. Delete that edge. Assert both symbols leave the reachable set.

**Write it before the algorithm, and verify it FAILS against a support-counting implementation before you fix it.** A test that has never failed proves nothing.

---

## 5. Schema

```sql
-- Linearized commit sequence (first-parent walk of one named branch)
CREATE TABLE commits (
    repo        TEXT   NOT NULL,
    seq         BIGINT NOT NULL,   -- the ONLY temporal coordinate
    commit_hash TEXT   NOT NULL,
    parent_hash TEXT,
    authored_at TIMESTAMPTZ,       -- decorative; never used for ordering
    PRIMARY KEY (repo, seq),
    UNIQUE (repo, commit_hash)
);

-- Crash-consistency watermark (§3.1)
CREATE TABLE repo_state (
    repo                    TEXT PRIMARY KEY,
    last_applied_seq        BIGINT NOT NULL,
    linearization_fingerprint TEXT NOT NULL   -- rolling hash of first-parent chain (§2.1)
);

-- Commits deliberately not indexed (§3.2). The skip rate is part of every result.
CREATE TABLE skipped_commits (
    repo TEXT NOT NULL, seq BIGINT NOT NULL,
    reason TEXT NOT NULL,     -- 'type_errors' | 'module_unavailable' | 'toolchain'
    detail TEXT,
    PRIMARY KEY (repo, seq)
);

-- Derived facts with validity intervals
CREATE TABLE facts (
    fact_id               BIGSERIAL PRIMARY KEY,
    repo                  TEXT NOT NULL,
    kind                  TEXT NOT NULL,   -- 'CALL' today; 'IMPLEMENTS','IMPORTS' next
    source_symbol         TEXT NOT NULL,
    target_symbol         TEXT NOT NULL,
    provenance            TEXT NOT NULL,   -- 'direct_call' | 'interface_resolved'
    call_site             TEXT NOT NULL,   -- disambiguates multiple sites (see index note)
    source_file           TEXT NOT NULL,
    source_module         TEXT NOT NULL,
    source_module_version TEXT NOT NULL,
    support_count         INT  NOT NULL DEFAULT 1,  -- surviving derivations (§2.2)
    valid_from            BIGINT NOT NULL,
    valid_to              BIGINT,
    CHECK (valid_to IS NULL OR valid_to > valid_from)
);

-- At most one open interval per logical fact. call_site and provenance are IN the key:
-- omitting them collapses a fact derivable from two distinct call sites into one row.
CREATE UNIQUE INDEX facts_live_uniq
    ON facts (repo, kind, source_symbol, target_symbol, provenance, call_site, source_module)
    WHERE valid_to IS NULL;

CREATE INDEX facts_interval ON facts (repo, kind, valid_from, valid_to);

-- What each fact was derived from (§2.2). Drives both withdrawal and backward validation.
CREATE TABLE derivations (
    fact_id    BIGINT NOT NULL REFERENCES facts(fact_id) ON DELETE CASCADE,
    input_kind TEXT NOT NULL,   -- 'FILE' | 'TYPE' | 'INTERFACE' | 'MODULE'
    input_key  TEXT NOT NULL,
    input_hash TEXT NOT NULL,   -- FILE: structural hash
                                -- TYPE: method-set hash
                                -- INTERFACE: IMPLEMENTER-SET hash  (§2.2 — not method set)
                                -- MODULE: version string
    PRIMARY KEY (fact_id, input_kind, input_key)
);

CREATE INDEX derivations_reverse ON derivations (input_kind, input_key, input_hash);

-- Maintained implementer sets: a first-class derived relation, not recomputed by scanning.
CREATE TABLE interface_implementers (
    repo TEXT NOT NULL, interface_id TEXT NOT NULL,
    implementer_set_hash TEXT NOT NULL,
    valid_from BIGINT NOT NULL, valid_to BIGINT,
    CHECK (valid_to IS NULL OR valid_to > valid_from)
);

CREATE TABLE dependency_versions (
    repo TEXT NOT NULL, module_path TEXT NOT NULL, version TEXT NOT NULL,
    valid_from BIGINT NOT NULL, valid_to BIGINT,
    CHECK (valid_to IS NULL OR valid_to > valid_from)
);

CREATE TABLE vulnerable_symbols (
    osv_id TEXT NOT NULL, module_path TEXT NOT NULL,
    affected_version_range TEXT NOT NULL, vulnerable_symbol TEXT NOT NULL,
    PRIMARY KEY (osv_id, vulnerable_symbol)
);

-- Incrementally maintained reachability, DRed support counts (§4)
CREATE TABLE reachable_symbols (
    repo TEXT NOT NULL, symbol TEXT NOT NULL,
    support_count INT NOT NULL,
    valid_from BIGINT NOT NULL, valid_to BIGINT,
    CHECK (valid_to IS NULL OR valid_to > valid_from)
);
```

**Query at commit C:** `valid_from <= seq(C) AND (valid_to IS NULL OR valid_to > seq(C))`.

**Attribution — the entire v2 §5.4 pipeline, now one query:** introducing commit is `commits WHERE seq = facts.valid_from`; removing commit is `commits WHERE seq = facts.valid_to`. Cause is read off the delta that moved the interval.

**Referential integrity note:** `valid_from` / `valid_to` should reference `commits(repo, seq)`. Postgres cannot express a composite FK against a nullable column cleanly, so enforce it with an audit query in the harness (§9, I4) rather than pretending a constraint exists.

---

## 6. The Go frontend

**Loading.** `golang.org/x/tools/go/packages` with `NeedSyntax`, `NeedTypes`, `NeedTypesInfo`, `NeedDeps`, `NeedModule`. Not hand-rolled `go/parser` + manual `go/types.Config`. This is what gopls, govulncheck, and go-callvis use, because it correctly resolves modules, build tags, vendored dependencies, and test files — the category of edge case that produced the chi `Selection` type-assertion bug. **Always check `pkg.Errors` (§3.2).**

**Call graph construction — a named position on a documented spectrum.** `golang.org/x/tools/go/callgraph` defines four algorithms by increasing precision and cost: static-only (unsound), CHA, RTA, VTA. A call graph is **sound** if it overapproximates every dynamic calling behavior; one is **more precise** if it is a smaller overapproximation. Two separate measurable properties.

- **CHA is the baseline.** Sound; conservatively computes the whole `implements` relation ahead of time. Known weakness: spurious edges to types that implement an interface but are never instantiated in reachable code.
- **RTA-inspired gating is layered on top, never a replacement.** An `interface_resolved` edge surfaces in the precise tier only if its target concrete type is confirmed reachable. Raw CHA facts stay complete and unmodified; the gated view is a read path. **Expressed positively** (§4.2).
- **VTA is declined, with a stated reason.** vulncheck itself defaults to VTA with CHA as an option, because CHA's spurious-edge problem is real. But VTA and the x/tools RTA both operate on SSA and require whole-program batch construction from complete entrypoints. SSA basic blocks and registers are not designed for per-file incremental reconstruction, which is this project's differentiator. Adopting that IR means rebuilding the validated incremental core on a representation that fights it, to gain precision on a property that is not what makes the project distinctive.

**Do not implement the RTA gate via `go/callgraph/rta`.** Implement it over the fact store using the maintained reachable set.

**Entry-point model.** A stated configuration choice, not a clever solution. Binary → `main.main` and everything reachable. Library → every exported top-level function across all packages, because you do not control the caller. The same choice govulncheck must make and documents.

---

## 7. Deltas the engine handles

All of these are the **same code path**. That is the point.

| Delta | Base relations touched | Response |
|---|---|---|
| File added/modified/deleted | `FILE` | Withdraw facts whose file hash changed; backward-validate, re-derive on miss |
| Type signature change | `TYPE` | Withdraw facts consulting that type identity |
| New/removed implementer of a dispatched interface | `INTERFACE` (implementer-set hash) | Withdraw all dispatch sites through that interface; re-derive (§2.2) |
| Dependency version bump | `MODULE` | Withdraw facts from that module at the old version; re-derive from the module cache |
| Reachability change | derived → derived | DRed (§4) |

**v2 §5.5 as an instance, not a special case.** A dependency bump withdraws every fact whose `MODULE` input hash changed. Because `derivations` records interface identities in both directions, the bidirectional recheck — *do the new version's types still implement interfaces the main repo dispatches through, and do the main repo's interfaces gain or lose implementers* — happens as a consequence of following recorded inputs. New source comes from the local module cache after `go mod download`; no network fetch.

---

## 8. The `IMPLEMENTS` probe — run early, as falsification

v3 called this "the single most convincing artifact this project can produce" and scheduled it last, as a victory lap. **That was backwards.** Its real value is as the **test that falsifies §2.2**.

Add `IMPLEMENTS` as a new derived relation and attempt to write **zero new invalidation code**. If it requires new hand-written invalidation, the architectural claim is false and the derivation model needs work before another line of it is built.

**Run this immediately after derivation tracking exists, before DRed.** It is the highest-information small task in the project. If it passes, ship the diff in the README as the demonstration; if it fails, you have learned that at a cost of hours instead of weeks.

---

## 9. Correctness model

| # | Invariant | Enforcement |
|---|---|---|
| I1 | Facts live at sampled `seq` == full rebuild at that `seq` | Harness; **state the sampling scheme and rate** — every-100th-commit hides bugs that self-heal |
| I2 | At most one open interval per logical fact | `facts_live_uniq`, with `provenance` + `call_site` in the key |
| I3 | `valid_to IS NULL OR valid_to > valid_from` | `CHECK` constraint |
| I4 | Every interval endpoint references a real `commits(repo, seq)` | Audit query in the harness (composite-FK limitation, §5) |
| I5 | A fact is live iff ≥1 derivation has all recorded inputs at recorded hashes | Per-fact `support_count` + backward validation (§2.2, §2.3) |
| I6 | `reachable` == BFS over live facts from entry points | Full recount at sampled commits |
| I7 | **An SCC with no surviving external entry edge is not reachable** | §4.4 fixture — the most important test in the project |
| I8 | `support_count` == count of surviving derivations | Audit recount query |
| I9 | After crash, the store is at a known fully-applied `seq` | `repo_state`, single transaction, SIGKILL injection (§3.1) |
| I10 | **Withdrawal is sound: withdrawn set ⊇ actually-differing set** | Asserted **separately and directly**. Critical: §10.1's ratio below 1.0 would otherwise read as "efficient" when it means "unsound" |
| I11 | Derived fact set identical regardless of worker count | Testable now that §3 defines the model |
| I12 | Backward-validation hit ⇒ facts identical to recomputation | Periodic forced recompute, compare |
| I13 | `analyzer_version` change invalidates all cached derivations | Make it a **build-time hash of the analyzer package**, not a hand-bumped constant |
| I14 | A symbol is unreachable only after checking every entry point | Assert the entry-point set was enumerated, not short-circuited |
| I15 | No fact carries a `source_module_version` not live at that `seq` | Cheap audit query; catches a whole class of dependency-delta bugs |
| I16 | Linearization is stable across re-index | Fingerprint check (§2.1) |
| I17 | No commit with package `Errors` was indexed | Gate + `skipped_commits` (§3.2) |

### 9.1 Fixtures before generators

v3 listed eight adversarial edit classes and framed a generator as the deliverable. Reverse the order:

**Hand-write all eight as fixtures first.** They are the oracle-bearing cases, and they cost hours:

1. add a method to a dispatched interface
2. remove a method from a dispatched interface
3. **add an implementer of an already-dispatched interface** (this is the §2.2 bug — it must fail before the fix)
4. remove the only implementer of a dispatched interface
5. move a type across a package boundary
6. change a signature without changing a body, and the converse
7. rename a symbol and reuse the old name for a different symbol in the same commit
8. **delete the sole external edge into a mutually recursive component** (I7)

**Build a property-based generator only if the fixtures pass and budget remains.** Generating Go that still type-checks after moving a type across package boundaries is a small program-transformation engine — one of the two largest engineering items in the project, and its marginal value over the eight fixtures is smaller than it appears. Critically: **do not accept "generate, and skip if it doesn't compile."** A generator with that escape hatch produces only trivial edits and proves nothing.

### 9.2 Keep the v2 engine as a second oracle

Tag and freeze v2 before starting. It becomes an independent reference — and it is the **baseline for §10.1, available only while it still runs.** Do not delete it.

---

## 10. Measurements

Findings, not features. But per §0's framing rule, they live *after* the engine in the README, not before it.

### 10.1 Invalidation precision

Per commit: **facts withdrawn ÷ facts that actually differed under full rebuild**, reported at p50/p90/p99.

**Two requirements, both non-negotiable.**

- **Assert I10 separately.** A ratio below 1.0 is a *soundness failure*, not efficiency. Without the directional assertion, the metric silently rewards being wrong.
- **The claim is narrow, and must be written narrowly.** *"Hand-written invalidation rules over-invalidate"* generalizes from a population of one implementation — yours. The only defensible form is:

> Measured that my own rule-based v2 engine over-invalidated by K× at p90 versus derivation-tracked maintenance over the same commit sequence.

That is a smaller claim and it is honest. It is the same discipline Parallax already enforces; enforce it here.

### 10.2 Incremental vs. full, including the crossover

Wall-clock and facts-touched as a function of change shape, over real history. **Report where incremental loses** — above some change size, invalidation plus re-derivation costs more than a full rebuild. What predicts the crossover (files touched? fan-in of changed symbols? interface methods touched?) is the actual result.

Methodology: cold vs. warm OS page cache stated; repeat count and variance, not a single run; hardware described honestly.

**This is also the profile that licenses the "why DRed and not B/F" answer** (§4.3). Without it, that answer is unsupported.

**Prior-art caveat, mandatory.** Szabó, Erdweg & Bergmann (PLDI 2021) develop an empirical methodology for estimating whether a computation is inherently incompatible with incrementalization, and find high-impact changes are rare. Cite it; state the differences (persistent cross-process store, commit-granular VCS deltas rather than IDE keystrokes, call-graph rather than lattice-based analyses); do **not** claim novelty for the observation that most commits are incrementalizable.

### 10.3 govulncheck differential — the only external verifier in the portfolio

Per `(commit, CVE)` verdict comparison; agreement rate; taxonomy of disagreements, each classified as (a) predicted CHA over-approximation, (b) entry-point model difference, (c) `reflect` invisibility, (d) a real bug in mine.

**Commit the predicted taxonomy to git BEFORE running the comparison.** That single step is the difference between a measurement and a result, and it is the same pre-registration discipline Parallax uses.

**Three caveats that must be stated unprompted, or the first question kills the bullet:**

1. **Not apples-to-apples on the database.** govulncheck consumes the Go vulnerability database, not raw OSV. Normalize, or your "disagreements" are database differences wearing an algorithm costume.
2. **N is small and selected.** govulncheck requires a buildable module; most historical commits don't build (§3.2). Report the exclusion rate as part of the result.
3. **govulncheck is not ground truth.** This measures *agreement*, not accuracy. It uses VTA, which is more precise than CHA; systematic disagreement in the direction of your over-approximation is the expected outcome, not a defect.

Why it is load-bearing anyway: every other claim in your entire portfolio is your harness agreeing with your rebuild. Your diagnosed career bottleneck is exactly this — solo repositories with no external verification. This is the closest thing to third-party validation available without a merged upstream PR. It also permanently retires the "why not just use govulncheck?" question, because the answer becomes *"I validate against it."*

---

## 11. Additions — deferred, and honestly labeled

Both are **packaging**, not evidence. Neither carries interview signal on its own. Ship them last or not at all.

- **§Service with a latency budget.** Commit lands → facts updated → queries served, with p50/p99 under a defined load. **No latency number is publishable without** a stated load model, stated hardware (including whether the DB is on the same box), and a repeat count. One run on a laptop is not a number.
- **§PR-time verdict.** A GitHub Action commenting *this change makes CVE-X reachable via this path*. Its one non-packaging argument is that it demonstrates the engine is actually incremental — but §10.2 already *measures* that. It adds a user, not evidence.

---

## 12. Non-goals

- A Datalog DSL or query language — re-implements Soufflé.
- A second language frontend; an LSP integration.
- Multi-writer or distributed execution. §3 is single-writer **by design**, not by omission.
- `DRed^c`, B/F, or FBF implementations — name them, decline them, and only revisit with a measured bottleneck (§10.2).
- Taint/dataflow analysis. Atlas answers "can this path execute," never "can attacker data reach this sink." State the distinction unprompted; it is correct scoping, not a gap.
- Multi-service / network topology. No data model exists; do not build a fake one.
- Speculative patch impact ("what would bumping X to Y fix").
- A web UI or a second Grafana dashboard.
- Multi-branch temporal modeling.
- Benchmarking across many repositories to compensate for methodology. Three repos with a stated selection rationale and a documented crossover beat twenty with a bar chart.
- Swapping Postgres for something faster. The bottleneck is `go/packages` type-checking; assume nothing else without a profile.
- Any new **kind** of fact before §8's probe passes.

**Known limitations to state proactively:** `reflect` calls are invisible to static analysis — the same blind spot govulncheck has. CHA over-approximates; the RTA gate narrows but does not eliminate it. Single-branch temporal model. PR-granular attribution on merge-based workflows.

---

## 13. Build order

Each step ends with the harness green. Do not start N+1 until N passes.

1. **Prior-art reading.** Timeboxed. Verify B/F and FBF from primary sources (§4.3); verify Glean, Kythe, Soufflé incremental before writing any comparative sentence. Before code.
2. **Freeze v2.** Tag it. It is oracle #2 and the §10.1 baseline.
3. **Crash consistency + poison-input gate.** `repo_state`, single-transaction deltas, SIGKILL injection, `skipped_commits`. Cheapest correctness-per-effort in the project, and §3.2 caps everything downstream.
4. **Interval store.** `facts` with `[valid_from, valid_to)`, `commits`, linearization fingerprint. Drop `reachability_snapshots`. Harness passes plus I1, I3, I4, I16.
5. **Derivation tracking, with implementer-set hashing.** Replace hand-written rules. `interface_implementers` as a maintained relation.
6. **⚠️ The `IMPLEMENTS` probe (§8).** Falsifies or confirms §2.2 before anything expensive is built on it. If it needs new invalidation code, stop and fix the model.
7. **Measure §10.1 against the frozen v2 engine.** Timing-critical — this is only available while v2 still runs.
8. **The eight fixtures (§9.1).** Fixture 3 must fail before step 5's fix and pass after; fixture 8 must fail before step 9.
9. **DRed reachability (§4).** Write the §4.4 fixture first, verify it fails against support counting, then build. Resolve the LFP argument on paper first — thirty minutes, not a formal model.
10. **Backward validation (§2.3).** Report cache-hit rate as a diagnostic.
11. **Determinism assertion (I11).**
12. **Performance characterization + crossover (§10.2).** With the PLDI 2021 caveat in the README.
13. **govulncheck differential (§10.3).** Predicted taxonomy committed first.
14. *Optional, budget permitting:* property-based generator (§9.1).
15. *Packaging:* service, PR action (§11).

Steps 3–9 are the project. Steps 12–13 are the findings. Everything after 13 is packaging.

---

## 14. Resume framing

Write a clause only when its step is green.

> **Atlas — Incremental Program Analysis Engine** | Go, PostgreSQL, go/packages
>
> - Implemented Delete–Rederive incremental maintenance of call-graph reachability over a persistent Postgres fact store, maintaining the least fixpoint under edge deletion including self-supporting recursive cycles that naive support counting retains indefinitely; validated by differential equivalence against full rebuilds at N sampled commits across M repositories and by SIGKILL fault injection at randomized points within delta application over 500 trials with zero unrecoverable states.
> - Replaced hand-written invalidation rules with derivation-tracked withdrawal, where each derived fact records the file hashes, type identities, interface implementer sets, and module versions it was derived from; adding two new fact relations subsequently required zero new invalidation code (diff in README).
> - Cross-validated CVE reachability verdicts against Google's govulncheck across N (commit, CVE) pairs, achieving Z% verdict agreement with every disagreement classified against a taxonomy committed to git before the comparison was run; residual disagreements are attributable to CHA over-approximation and entry-point model differences, both predicted by the design.

**Excluded, deliberately:**
- **Cache-hit rate.** Measures redundancy in git history, not engine quality. *"What would a bad cache-hit rate look like?"* exposes it instantly.
- **Any unqualified "hand-written rules over-invalidate by K×."** Population of one. Use §10.1's narrow form or nothing.
- **Any p99 latency number** without load model, hardware, and repeat count.

**Skills displayed, descending rarity for an undergraduate:** least-fixpoint view maintenance with DRed-correct deletion · temporal data modeling with validity intervals · crash-consistent transactional state machines · static analysis with a defended position on the CHA/RTA/VTA spectrum · differential testing with an external oracle · deterministic parallel derivation · Postgres schema design under a real access pattern.

**The question this document exists to let you answer well:** *"Walk me through what happens when someone bumps a dependency."* One mechanism, no special cases: a delta on base relations, withdrawal by changed input hash, bidirectional interface recheck as a consequence of recorded derivations, re-derivation from the module cache, DRed for the reachability fallout, harness proves equality with a full rebuild.

---

## 15. Kill criteria

**Per component:**

- **Implementer-set hashing (§2.2)** — if a stable, cheap, incrementally-updatable implementer-set hash proves unachievable, abandon the computed-invalidation claim entirely, keep hand-written rules, and delete §2.2 and §8 from the README. **A hand-written rule you can defend beats a derived one you cannot.**
- **`IMPLEMENTS` probe (§8)** — if it requires new invalidation code, the architecture claim is false. Fix the model or drop the claim; do not build DRed on top of a falsified foundation.
- **govulncheck differential (§10.3)** — if fewer than ~30% of sampled historical commits build under a pinned toolchain, kill it. The surviving N is too selected to support a percentage.
- **Property-based generator (§9.1)** — if it only produces edits the eight fixtures already cover, kill it. It has become an engineering exercise with no oracle value.
- **Interval store (§2.1)** — if the fingerprint reveals target repos rebase frequently, demote temporal attribution from headline to "attribution on linear-history repos."
- **Service / PR action (§11)** — kill on any budget pressure. They are packaging.

**Whole project:**

- **Kill if, four weeks in, you cannot reconstruct the DRed rederive termination argument and the LFP-vs-arbitrary-fixpoint distinction on a whiteboard, cold.** That is the RAGWatch test. Failing it means the project is producing artifacts you cannot defend, which is a liability regardless of how good they are.
- **Kill if Parallax or SpecGuard slips by more than two weeks because of Atlas.** Both are ahead of it on the stated bottleneck (external verification, upstream contribution). Atlas's only claim to that ground is §10.3, and agreement with a tool is not a merged PR.
- **Kill on portfolio redundancy** if, writing the three README abstracts side by side, all three read as "harness that differentially compares X to Y." §0's framing rule exists to prevent this; if it fails in practice, the marginal signal from Atlas is near zero regardless of execution quality.

---

## 16. Rejected review findings, and why

Recorded so these do not get re-litigated.

**Rejected: "the DRed + RTA-gate program may be non-stratified; build a bounded formal model (~20 h) to check."**
Stratification constrains **negation** inside recursion. The program (§4.2) is entirely positive — no negation, no aggregation in the cycle — therefore monotone, therefore the least fixpoint exists and is unique, and DRed's published scope covers general recursion. There is no theorem gap to formally verify. The associated kill criterion ("stop if unresolved after 6 h") would have killed the project over a non-problem. **Replaced with:** the LFP-vs-arbitrary-fixpoint framing (§4.2), which is the real content the alarm was gesturing at, plus one design rule — express the gate positively, never introduce a negated derived relation into the cycle.

**Rejected: "computed invalidation requires Datalog's declarative rule bodies; provenance gives retract-only."**
Partly right, wrong conclusion. Hashing the implementer set converts the derive case into retract-then-rederive, which provenance handles natively (§2.2). No DSL, no rule registry. The over-invalidation this introduces is the correct trade and is measurable.

**Rejected: TLA+ over the engine.** One writer, one database, nothing to model-check. The formal-methods budget belongs on the exactly-once engine, where genuine concurrency exists.

**Rejected: the authorship critique.** Whether a design document was drafted with model assistance is not the test. The test is §15's whiteboard criterion. Apply that; ignore register analysis.

**Rejected: hour estimates as stated.** With agentic tooling the mechanical fraction (schema migration, query rewrites, harness plumbing, service, action) compresses hard; the conceptual fraction (§2.2's hash design, §4's rederive termination, §10's methodology) compresses **not at all**, because it is the part that must be defensible under drill and therefore cannot be delegated. Assume the *ratio* shifts — conceptual work goes from ~40% of the effort to ~80% — not that the total collapses.

---

## 17. What makes this good rather than merely bigger

v3.1 does roughly what v2 does, minus one table, minus one pipeline, minus a class of hand-written code — with one idea underneath it, one genuinely hard algorithm at its center, one measured claim about the world, and one external verifier.

Every proposed addition is tested against §0. If it does not follow from that sentence, it is scope, not architecture.