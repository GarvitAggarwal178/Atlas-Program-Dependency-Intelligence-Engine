# Progress

Running log, one entry per work session. What was done and current status.
Honest reporting: failures and broken states are recorded here, not papered
over.

---

## Session 2026-08-01

**Context:** Kicked off executing architecture.md (Atlas v3.1) against the
existing repo, which contains v2 ("symex") — a working but architecturally
superseded engine. Instructions: treat architecture.md as sole source of
truth, no deviation without flagging, run unattended, commit atomically
under the user's own git identity (no Claude co-author trailer).

**Done:**
- Read architecture.md end to end (all 17 sections + rejected-findings log).
- Read the full existing codebase (all of `internal/*`, `cmd/*`, README,
  go.mod, docker-compose.yml, scripts) and built the Step 0 diff (posted to
  chat, not duplicated here — see conversation or reconstruct from
  docs/DECISIONS.md + docs/FLAGGED.md, which are the durable record).
- Created docs scaffolding: this file, docs/DECISIONS.md, docs/FLAGGED.md,
  docs/CHANGELOG.md, CLAUDE.md.
- Verified git identity was already correct (GarvitAggarwal178 /
  garvitaggarwal178@gmail.com) — no config change needed.

**Status at end of session:** (updated as work proceeds — see task list
below for what's in flight.)

**Build-order progress (architecture.md §13):**

| Step | What | Status |
|---|---|---|
| 1 | Prior-art reading, verify B/F/FBF citations | Deferred — flagged (docs/FLAGGED.md), needs explicit time-budget go-ahead |
| 2 | Freeze v2, tag it | **Done.** Tagged `v2-frozen` at the commit that added architecture.md + docs scaffolding. |
| 3 | Crash consistency + poison-input gate | **Done.** See below. |
| 4 | Interval store (facts, commits, linearization fingerprint) | **Done.** See below. |
| 5 | Derivation tracking, implementer-set hashing | **Done.** See below. |
| 6 | IMPLEMENTS probe | Not started |
| 7+ | Everything else | Not started this session |

### Step 5 detail (derivation tracking, implementer-set hashing) — done

**`internal/derive` (pure logic, no DB):** `ImplementerSetHash` — sorted,
deduplicated, SHA-256 over the qualified implementer type names. Also
includes `MethodSetHash` (kept ONLY as the rejected v3 mechanism, so a test
can demonstrate why it's wrong, not for any store code to call) and
`FileStructuralHash`/`TypeMethodSetHash` for the other three
`derivations.input_hash` kinds §5 names (FILE, TYPE), for vocabulary
completeness — MODULE uses the version string directly, no hash function
needed.

**`TestSoundnessFix_MethodSetHashMissesNewImplementer_ImplementerSetHashCatchesIt`**
reproduces §2.2's worked example directly: constructs the exact scenario
(interface method set unchanged, implementer set changed) and proves
`MethodSetHash` stays constant across it (the v3 bug, made concrete) while
`ImplementerSetHash` changes (the fix).

**`internal/store/schema_v5.go`:** `atlas.derivations` (references
`atlas.facts(fact_id) ON DELETE CASCADE`) and `atlas.interface_implementers`
— the maintained, interval-based relation §2.2 requires ("a first-class
derived relation, not recomputed by scanning the program").

**`internal/store/derivation.go`:**
- `RecordDerivation`/`RecordDerivations` — tied to `*sql.Tx`, same
  crash-consistency reasoning as `OpenFact`.
- `StaleLiveFacts(ctx, q, repo, inputKind, inputKey, currentHash)` — THE
  invalidation query: every live fact whose recorded hash for that input no
  longer matches. This is the entire mechanism §2.2 promises — no
  per-trigger-type rule, one query, reused for every input kind.
- `UpsertInterfaceImplementers` — maintains the interval-based implementer-set
  relation, reports `changed` so callers know whether to run
  `StaleLiveFacts` at all.

**`TestSection2_2Fixture`** (`internal/store/derivation_test.go`) is
§9.1 fixture 3 ("add an implementer of an already-dispatched interface —
this is the §2.2 bug — it must fail before the fix") run against the
ACTUAL fix, end to end at the store level: opens a dispatch-site fact with
a recorded `INTERFACE` derivation at the old implementer-set hash, adds a
new implementer, and proves (a) `UpsertInterfaceImplementers` reports the
change, (b) `StaleLiveFacts` correctly flags the dispatch fact and does
**not** flag an unrelated fact, (c) a no-op update (same set) correctly
reports `changed=false`, (d) once the stale fact is closed it drops out of
`StaleLiveFacts`.

**Not done as part of step 5:** this is still the primitive layer, not
wired into the parser/callgraph pipeline — nothing yet calls
`derive.ImplementerSetHash` from real parsed Go source, and there's no
"index a real commit end-to-end through derivation tracking" loop. That
wiring, plus the actual §8 IMPLEMENTS probe, is the next task.

### Step 4 detail (interval store) — done

**Linearization (§2.1):** new `internal/linearize` package, no DB
dependency (pure git + hashing). `Walk(repoDir, branch)` runs `git rev-list
--first-parent` and assigns `seq` oldest-first; because a first-parent walk
guarantees each entry's first parent is the next entry in the (reversed)
list, parent hashes come for free with no extra `git log` calls.
`Fingerprint(commits, upToSeq)` and `VerifyFingerprint(commits,
lastAppliedSeq, storedFingerprint)` implement the rebase/force-push
detection §2.1 requires. **Actually tested against a real rebase**, not
just unit logic: `TestVerifyFingerprint_DetectsRebase` creates a real git
repo, fingerprints it, runs `git commit --amend` (a genuine history
rewrite), and asserts the fingerprint mismatch is caught.

**Interval facts table (§2.1/§5):** `internal/store/schema_v4.go` adds
`atlas.facts` — the exact §5 shape (`fact_id`, `valid_from`/`valid_to`,
`facts_live_uniq` partial unique index, `facts_interval` index) — living in
the `atlas` Postgres schema (see the step-3 schema-collision note; the same
reasoning applies here, since v2's `facts` table already exists in
`public`). `internal/store/interval.go` adds `OpenFact`/`CloseFactByKey`/
`CloseFactByID` (all require a `*sql.Tx`, deliberately — no non-transactional
fact-write path exists, so it's structurally hard to write a fact outside
`ApplyDelta`'s transaction and reopen the §3.1 crash-consistency hole) and
`QueryFactsAt`/`QueryLiveFacts` (accept either `*sql.DB` or `*sql.Tx`).

**Invariants actually verified by test, not just asserted in prose:**
- "Query at commit C" interval semantics
  (`TestOpenFact_VisibleOnlyWithinItsInterval`): a fact is invisible before
  `valid_from`, visible at and after `valid_from` while open, and invisible
  from `valid_to` onward (exclusive) once closed — checked at multiple
  `seq` values, not just one.
- **I2** (at most one open interval per logical fact) — `TestFactsLiveUniq_
  RejectsSecondOpenInterval` proves the database itself rejects a second
  concurrent open interval for the same natural key (not just "the code
  happens not to do this"), and that the failure correctly rolls back the
  whole delta (watermark stays at the prior seq).
- **I3** (`valid_to IS NULL OR valid_to > valid_from`) —
  `TestFactsCheckConstraint_RejectsValidToBeforeValidFrom` proves the
  `CHECK` constraint is live.

**Not done as part of step 4:** `atlas.commits` is not yet populated by
`linearize.Walk` automatically — that wiring (walk on startup, insert new
rows via `InsertCommits`, verify fingerprint via `VerifyFingerprint` before
resuming, refuse and require re-index on mismatch) is the "index this
commit" loop that steps 5-7 need anyway once there's something to derive
against; building it now against nothing to derive would be premature
plumbing. `interface_implementers`, `dependency_versions`,
`vulnerable_symbols`, `reachable_symbols` (§5's other tables) are correctly
deferred to steps 5, 7, and 9 respectively — not needed yet and not added
speculatively.

### Step 3 detail (crash consistency + poison-input gate) — done

**Poison-input gate (§3.2):** `internal/parser/poison.go` adds
`CheckPoison([]*packages.Package) PoisonResult`. Correction made mid-session:
`internal/parser/parser.go` already used `golang.org/x/tools/go/packages`
(the README's "stdlib go/parser+go/types only" description is stale, not
the code) — so no frontend migration was needed, only the missing gate
itself. `ParseRepo` was refactored (extract `LoadPackages`) with **zero
behavior change** — confirmed by re-running the pre-existing
`TestParseFixture` unchanged and passing. New tests:
`internal/parser/poison_test.go`, using a new deliberately-broken fixture
at `testdata/broken_fixture/`.

**Crash consistency (§3.1):** `internal/store/schema_v3.go` adds
`commits`, `repo_state`, `skipped_commits` (exact §5 shapes).
`internal/store/crash.go` adds `ApplyDelta` (single-transaction watermark +
data write) and `ResumeFromSeq`. Tested in
`internal/store/crash_test.go`:
- `TestApplyDelta_WatermarkCommitsAtomicallyWithData` — positive case.
- `TestApplyDelta_FailedDeltaLeavesNoWatermarkAdvance` — negative case,
  asserts a failed delta leaves zero trace (both the watermark AND the
  partial data write are rolled back).
- `TestSIGKILLInjection_RecoversToKnownSeq` — the actual §3.1-required
  test: real subprocess (`crash_helper_test.go`), killed via
  `cmd.Process.Kill()` (Windows `TerminateProcess`; see docs/DECISIONS.md
  for the platform note) at a randomized point (5-200ms) against an
  80ms-held transaction, asserting the store always recovers to a
  known-consistent `(commits, repo_state)` pair.
  - **Honest result:** first 500-trial run hit one apparent inconsistency
    at trial 203; investigated directly against Postgres rather than
    dismissed — the DB was actually fully consistent by the time of
    inspection, pointing to a transient read-after-kill race in the test
    itself, not the product. Added a bounded retry + full diagnostics to
    the test (see docs/DECISIONS.md for the full writeup) and reran
    **two full fresh 500-trial runs clean, zero mismatches (including zero
    transient ones)**.

All pre-existing tests (`internal/callgraph`, `internal/canonicalize`,
`internal/classifier`, `internal/differ`, `internal/incremental`,
`internal/parser`, `internal/reachability`, `internal/store`) still pass —
confirms the v2 oracle is undisturbed.

**Not done as part of step 3:** the gate is not yet wired into an
end-to-end "index this commit" loop (that loop is effectively steps 4-7's
job — the interval store needs to exist first for there to be a real
fact-writing path to gate). Step 3 delivers the *mechanism*
(`ApplyDelta`, `CheckPoison`) proven correct in isolation, per the build
order's "each step ends with the harness green" — there is no harness yet
for the full pipeline because that pipeline doesn't exist until later
steps.

*(This table is intentionally left for in-session updates rather than
pre-filled with claimed results — each row gets filled in as that step
actually goes green, per the instruction to never mark something done that
isn't.)*

**Known broken / incomplete going into next session:** see docs/FLAGGED.md
for anything requiring a human decision; anything not in FLAGGED.md that is
still incomplete is either not yet attempted or noted inline below as work
progresses.
