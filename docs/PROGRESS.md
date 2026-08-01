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
| 4 | Interval store (facts, commits, linearization fingerprint) | In progress this session |
| 5 | Derivation tracking, implementer-set hashing | Not started |
| 6 | IMPLEMENTS probe | Not started |
| 7+ | Everything else | Not started this session |

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
