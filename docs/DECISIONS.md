# Decisions

Every non-trivial implementation choice, mapped to the architecture.md
section it implements. Ambiguous points that were NOT resolved here — see
docs/FLAGGED.md instead.

---

## [2026-08-01] Freeze v2 via git tag rather than branch or copy

**Maps to:** architecture.md §9.2, §13 step 2.

**Decision:** Tag current `main` HEAD as `v2-frozen` rather than branching
or copying the code to a new location.

**Why:** §9.2 says "Tag and freeze v2 before starting. It becomes an
independent reference." A tag is the minimal mechanism that satisfies this
— it pins a commit without duplicating the tree or requiring ongoing
maintenance of a parallel branch. `internal/incremental`, `internal/store`
(v2 schema) stay in the working tree, unmodified, as the running "second
oracle" §9.2 and §10.1 require — they are not deleted or refactored, only
frozen at that commit for reference.

---

## [2026-08-01] Poison-input gate implemented directly against go/packages' pkg.Errors

**Maps to:** architecture.md §3.2, §6.

**Decision:** The hard gate ("refuse to index a commit if any loaded
package has errors or incomplete TypesInfo") is implemented directly
against `golang.org/x/tools/go/packages`' `pkg.Errors` and a `pkg.TypesInfo
== nil` check — exactly the mechanism §3.2 specifies. `ParseRepo` in
`internal/parser/parser.go` already loads via `go/packages` with
`NeedTypesInfo` etc.; it previously only logged `pkg.Errors` to stderr and
kept going (see docs/FLAGGED.md's struck-through entry for the correction
of an earlier misread that assumed a hand-rolled frontend). The gate adds
a `CheckPoison(pkgs []*packages.Package) (clean bool, reason, detail
string)` helper consulted before any facts are derived from a commit's
parse result.

**Why:** No approximation needed once the frontend was read correctly —
§6's requirement was already met at the loader level; the gate is the
missing piece, not a frontend rewrite.

---

## [2026-08-01] `commits` table linearization: first-parent walk via `git log --first-parent`

**Maps to:** architecture.md §2.1.

**Decision:** Linearize via `git rev-list --first-parent <branch>`
(oldest-to-newest after reversal), assigning `seq` as the 0-based index in
that walk. Fingerprint is `SHA-256` of the newline-joined ordered list of
full commit hashes.

**Why:** This is exactly what §2.1 specifies — "a first-parent walk of one
named branch" — and matches the rolling-hash mitigation for rebase/force-push
detection it prescribes. `git rev-list --first-parent` is the standard,
correct way to get this walk without hand-rolling DAG traversal.

---

## [2026-08-01] `derivations.input_hash` for INTERFACE kind computed as sorted-implementer-set hash

**Maps to:** architecture.md §2.2 (the soundness fix), §5 schema comment
("INTERFACE: IMPLEMENTER-SET hash — not method set").

**Decision:** `interface_implementers.implementer_set_hash` = SHA-256 over
the sorted, newline-joined list of fully-qualified concrete type names that
satisfy the interface at that point in the commit sequence. Sorted so the
hash is independent of iteration/derivation order, per §2.2's stated
requirement ("stable... independent of iteration order").

**Why:** This is the exact fix architecture.md prescribes for the v3
soundness bug (method-set hash silently missing new implementers in new
files). Using a sorted set (not a multiset, not an ordered list) means
adding then removing an implementer, or two different derivation orders
producing the same final set, converge to the identical hash — required
for the "incrementally updatable... maintained as its own derived relation"
property §2.2 demands.

---

## [2026-08-01] Fact writes require a *sql.Tx, never a bare *sql.DB

**Maps to:** architecture.md §3.1 (crash consistency) applied to §2.1/§2.3's
fact-write paths.

**Decision:** `store.OpenFact`, `store.CloseFactByKey`, `store.CloseFactByID`
all take a `*sql.Tx` as a required parameter — there is no `(*DB) OpenFact(...)`
convenience method that opens its own transaction.

**Why:** §3.1's whole guarantee rests on every fact write happening inside
the same transaction as the `repo_state` watermark update
(`store.ApplyDelta`). If fact-write functions accepted a bare `*sql.DB`,
it would be easy for future code (including future-me, in step 5+) to call
them outside of `ApplyDelta` "just this once" for convenience, silently
reopening the exact hole §3.1 exists to close. Requiring a `*sql.Tx`
argument makes that mistake a compile error instead of a runtime footgun —
the only way to get a `*sql.Tx` in this codebase is from inside an
`ApplyDelta` callback.

## [2026-08-01] Test repo names must include a timestamp, not just t.Name()

**Maps to:** general test hygiene for internal/index and internal/store
integration tests, discovered via a real failure.

**What happened:** `internal/index`'s first end-to-end test used
`"index-e2e:" + t.Name()` as its repo identifier. Running that test alone
passed. Running the full suite afterward failed with a Postgres CHECK
constraint violation (`valid_to > valid_from`) while closing a fact — a
real constraint firing correctly, but for the wrong reason: the earlier
standalone run's data was still in the shared Postgres instance under the
same repo name, so the second run's seq=0 tried to close a fact whose
`valid_from` was 1 (from the PREVIOUS run) with `valid_to=0`. Investigated
via `psql` before changing anything, per the instructions' emphasis on not
papering over failures — confirmed the leftover row's repo name, deleted
it, and only then decided this was a test-isolation bug, not a product bug.

**Decision:** every integration test in this codebase that talks to the
shared Postgres instance must derive its repo identifier from
`t.Name() + time.Now().UnixNano()` (or equivalent), never `t.Name()`
alone — `internal/store`'s tests already did this (`uniqueRepoName`);
`internal/index`'s didn't, and that was the actual bug. Fixed by adding an
equivalent `uniqueIndexTestRepo` helper.

**Why this matters beyond this one fix:** it's a reminder that this test
suite shares real, persistent state (a long-lived Postgres container) across
runs, unlike most Go unit tests — collisions here look like product bugs
(a CHECK constraint really did fire) but are actually a fixture problem.
Any new integration test added later must follow the same convention.

## [2026-08-01] CheckPoison: zero loaded packages is Clean, not poison

**Maps to:** architecture.md section 3.2, correcting an earlier decision
in this same file.

**What happened:** `TestRunIndexer_IndexesRealCommitHistory` builds a real
3-file-history repo whose FIRST commit adds only `go.mod`, no `.go` files
yet (an entirely ordinary way a repo's history starts). Running the real
indexer against it reported that commit as skipped/poison — `CheckPoison`
originally treated `len(pkgs) == 0` as `ReasonModuleUnavailable`. That was
wrong: an empty `go.mod`-only commit legitimately has zero packages under
`"./..."`, with no error and no per-package `Errors`/incomplete-`TypesInfo`
signal at all. Section 3.2's skip rate is supposed to measure real build
failures ("the skip rate is part of every reported result"); counting
ordinary empty-repo commits as skips would inflate it with noise.

**Decision:** `CheckPoison(nil)` / `CheckPoison([])` now returns
`Clean: true`. A genuine module-resolution failure is still caught two
other ways: (a) `packages.Load` returning a non-nil error, which
`LoadPackages` already surfaces separately, upstream of `CheckPoison`
entirely; (b) whatever packages DID load having `Errors` or nil
`TypesInfo`, still checked exactly as before. Only the specific case of
"loaded successfully, matched nothing, no error signal anywhere" flipped
from poison to clean.

**Why recorded as a correction rather than silently fixed:** this reverses
this file's own earlier stated design bullet (buried in the original
"Poison-input gate" decision entry) without a second thought otherwise —
worth being visible that the FIRST version of this gate was too aggressive,
caught by an actual end-to-end test against real history, not by
inspection. `internal/parser/poison_test.go`'s
`TestCheckPoison_NoPackagesLoadedIsCaught` was renamed to
`TestCheckPoison_NoPackagesLoadedIsClean` and its assertion flipped to
match, with the old name and reasoning kept in the doc comment so this
isn't silently rewritten history.

## [2026-08-01] Fixed: cross-package interface satisfaction was never detected at all

**This is a significant one, found via fixture 5, not by inspection.**

`internal/parser`'s interface-implementer detection (`collectPackageInterfacesFromPkg`
/ `collectPackageInterfaces`) only ever scanned the CURRENT package's own
`pkg.Syntax` for interface declarations, then checked every concrete type
in that same package against only those same-package interfaces. A
concrete type in package A implementing an interface declared in package B
was **never detected** — `types.Implements` was simply never called
against it. This isn't a rare edge case: `io.Writer`, `http.Handler`,
`sort.Interface`, `fmt.Stringer`, `error` — cross-package interface
satisfaction is the NORMAL shape of idiomatic Go, arguably more common
than same-package. Every `interface_resolved` edge computed against a
cross-package interface was silently falling back to
`ComputeFacts`'s "no known implementers" raw-descriptor edge instead of
resolving to real concrete targets — undercutting the actual precision of
the whole system for a very common pattern, without any visible error.

**How it was found:** fixture 5 ("move a type across a package boundary")
failed on its very first assertion — the dispatch edge wasn't even present
at seq=0, before anything moved. Investigated rather than adjusting the
test to match: traced it to `collectPackageInterfacesFromPkg` and
confirmed by reading the code (not guessing) that it only ever receives
`pkg.Syntax`, one package's files.

**The old README's claim was wrong about the actual code:** it says
cross-package satisfaction "works when dependencies are available" — that
describes intent, not what the code did. Same category of stale-doc-vs-
real-code gap as the earlier `go/packages` correction this session.

**Fix:** `collectAllInterfaces(pkgs []*packages.Package)` replaces the
per-package collector, building one map keyed by FULLY-QUALIFIED interface
name across every loaded package, computed once in `BuildSymbolTable`
before the per-package extraction loop (not recomputed per package — it's
the same global map for all of them). `extractDefinedSymbols` was also
fixed at the same time to stop prepending the CONCRETE type's own import
path onto the interface name (`importPath+"."+ifaceName"`) — that was
only ever correct by coincidence when interface and implementer shared a
package; the map is now keyed by the interface's OWN qualified name, from
its own declaring package.

**Blast radius:** this is a shared-frontend fix — both v2 (`ParseRepo`)
and v3.1 (`BuildSymbolTable`) read the same `ImplementedInterfaces` field,
so both benefit uniformly. This does NOT touch v2's invalidation logic
(`internal/incremental`, still frozen/untouched) — only fact-extraction
accuracy improved, which is a legitimate shared-frontend correctness fix,
not a v2-specific behavior change. Full test suite reran clean afterward,
confirming no existing fixture (`testdata/fixture`'s `billing.Ledger`
case is same-package, so it never exercised this path either way) broke.
Added `TestCrossPackageInterfaceSatisfaction` as a direct, minimal
regression test right next to the fix, not just relying on fixture 5 to
catch a regression indirectly.

## [2026-08-01] Real incremental DRed (§4.3), and how it was validated before trusting it

**Maps to:** architecture.md section 4.3's actual mechanism — over-delete,
rederive, insert — replacing the full-BFS-recompute reachability
maintenance that was built first (deliberately, as a correctness baseline).

**Why a naive translation of the three phases isn't actually incremental,
and had to be caught before writing the "real" version:** the first
instinct is "phase 2/3 = BFS from (old reachable minus dead) over the new
edges." That is NOT incremental — a multi-source BFS costs the same
O(V+E) as a single-source one, so it would re-touch the entire reachable
region every commit, identical cost to `Reachable`'s full recompute, just
with extra bookkeeping. The actual efficiency claim required a sharper
argument: a node NOT marked dead by phase 1's pessimistic downstream
propagation is provably still reachable via a path that never touched a
removed edge — it can be ACCEPTED without re-derivation, not re-walked.
Phase 2 (rederive) is then a fixpoint bounded by `|dead|` (does a dead node
have some other live incoming edge from something confirmed?), and phase 3
(insert) seeds only from targets of genuinely new edges, not the whole
confirmed set. See `internal/reach/incremental.go`'s doc comment for the
full argument — it's written there because it's the kind of thing you need
to be able to reconstruct on a whiteboard, not just cite.

**Validation, in order, before trusting it for anything:**
1. The section 4.4 fixture (self-supporting cycle, sole external edge
   deleted) — ported directly, passes.
2. A differential fuzz test comparing `IncrementalUpdate` against
   `Reachable` (the already-proven-correct full recompute) across ~650
   random graphs (5 density/entry-count profiles) × ~30 sequential
   mutations each — **~19,500 comparisons**, with the incremental result
   fed FORWARD as the next delta's starting point rather than reset to
   ground truth each time. That last detail matters: resetting to ground
   truth every iteration would only test single-delta correctness and
   could hide errors that compound across a sequence, which is the actual
   production usage pattern.
3. Five hand-picked degenerate cases a fuzzer might rarely hit — including
   a "diamond" (two independent paths to the same node, only one broken)
   that specifically exercises phase 2 recovering a node phase 1
   pessimistically killed via propagation. Manually hand-traced this case
   before trusting the test's own pass/fail, not just relying on the
   assertion.
4. Only after 1-3 passed clean: wired into the real store
   (`MaintainReachabilityIncremental`), and ran a real 5-commit sequence
   of actual Go source (including the fixture 8 scenario, a brand-new
   node appearing, and an interface-dispatch edge shape) through BOTH the
   full-recompute path and the incremental path on separate repo
   namespaces, diffing `atlas.reachable_symbols` after every single
   commit — not just at the end.

**Both `MaintainReachability` (full recompute) and
`MaintainReachabilityIncremental` are kept**, not just the new one — the
full recompute stays as the simple reference implementation and the
target of the differential test, exactly analogous to keeping v2 as a
frozen oracle rather than deleting it once something newer exists.

## [2026-08-01] SIGKILL injection test trial count

**Maps to:** architecture.md §3.1 ("≥500 trials, asserting recovery to a
clean, known seq").

**Decision:** Test harness supports a configurable trial count via env var
(`ATLAS_CRASH_TRIALS`, default lower for fast local iteration), with CI/full
validation runs expected to set it to ≥500 per the spec.

**Platform note:** this environment is Windows. There is no POSIX
`SIGKILL` here; the injection test (`internal/store/crash_test.go`'s
`TestSIGKILLInjection_RecoversToKnownSeq`, helper in
`crash_helper_test.go`) uses `os/exec` + `cmd.Process.Kill()`, which on
Windows calls `TerminateProcess` — an abrupt, no-cleanup termination
equivalent in effect to SIGKILL for this test's purposes (no deferred
`Rollback()` runs, no graceful shutdown; the in-flight Postgres transaction
is only ever resolved by Postgres's own connection-drop handling, exactly
the scenario section 3.1 is testing). If this is ever run on a POSIX CI
runner, the same mechanism works unchanged — `Kill()` sends real `SIGKILL`
there.

**Why:** 500 real SIGKILL trials against a Postgres transaction is slow
(process spawn + DB round trip per trial); defaulting to a smaller number
keeps `go test ./...` fast for routine iteration while preserving the
ability to run the spec's actual required trial count on demand. This is
recorded as a decision (not silently under-testing) — the spec's "≥500" is
honored via an explicit override, not diluted.

**Bounded retry on the post-kill consistency check, added after a real
investigation:** The first full 500-trial run (2026-08-01) failed once, at
trial 203, with "commits row exists but watermark didn't advance." Rather
than loosen the assertion blindly, the actual Postgres state was inspected
directly after the failed run: `commits` had exactly rows for seq 0..99
(100 rows) and `repo_state.last_applied_seq = 99` — i.e. by the time of
manual inspection, the two tables agreed exactly, with no orphaned or
missing row. That rules out a genuine atomicity violation in `ApplyDelta`
(which writes both in one transaction — there is no code path that could
durably commit one without the other) and points at a **transient
visibility race in the test's own read-after-kill check**: a kill can race
a subprocess's own `tx.Commit()` — bytes already handed to the OS network
stack before the process dies still get delivered to and processed by
Postgres, so the commit can land a beat after `cmd.Wait()` returns to the
parent. The test now retries the consistency check up to 5×100ms before
declaring a real violation, and logs (not fails) if a retry resolves it —
this preserves the ability to catch a genuine violation (retries never
resolve a real one, since nothing further would commit) while not treating
ordinary commit-vs-process-exit ordering jitter as a product bug. Two
subsequent full 500-trial runs completed with zero mismatches, including
zero transient ones needing the retry.
