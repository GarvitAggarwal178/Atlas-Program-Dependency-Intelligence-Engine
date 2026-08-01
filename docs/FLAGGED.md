# Flagged — ambiguities, contradictions, or gaps not resolved by guessing

Format: `[date] issue — why it's blocking — what's needed from you`.
Items here are NOT silently resolved. Work continues on unrelated,
unblocked tasks per the build order in architecture.md §13.

---

## [2026-08-01] Repo/module identity: "Atlas" vs. "symex"

architecture.md refers throughout to the project as **Atlas**. The actual
Go module is `github.com/yourorg/symex`, and the README, CLI binary names
(`symex`, `symex-graph`, `symex-harness`), and package doc comments all say
"symex" / "Stage N."

**Why it's blocking:** Renaming the module path touches every import
statement across ~20 files and all three `cmd/` binaries. It's mechanical
but has real blast radius, and architecture.md never explicitly says
"rename the module" — it just uses "Atlas" as the project's name in prose.
Doing it unprompted risks being exactly the kind of "while I'm here" scope
creep the instructions forbid.

**What's needed:** Confirm whether to rename the module
(`github.com/yourorg/symex` → something Atlas-named), rename the binaries,
and update the README's framing (§0's "framing rule" — lead with the engine,
not the harness) — or leave the module name as legacy and treat "Atlas" as
architecture-doc-only vocabulary.

**Current call:** Left as `symex` / `github.com/yourorg/symex` for now, to
avoid a repo-wide rename with no explicit mandate. CLAUDE.md documents the
new name is "Atlas (formerly symex)" as a hedge.

---

## [2026-08-01] ~~`go/packages` frontend migration~~ — RESOLVED, was a misread

**Correction, same day:** The README (which documents "Stage 1" as
`go/parser`+`go/types`, standard library only) is stale relative to the
actual code. `internal/parser/parser.go` already uses
`golang.org/x/tools/go/packages` with `NeedSyntax|NeedTypes|NeedTypesInfo|
NeedImports|...`, exactly as §6 requires — this was verified by reading the
file directly, not the README. §6 is already satisfied at the frontend
level.

What is genuinely missing: `ParseRepo` (parser.go:75-90) iterates
`pkg.Errors` and only **logs** them (`fmt.Fprintf(os.Stderr, ...)`) before
continuing to extract facts from that package's (possibly incomplete)
`TypesInfo`. That is exactly the §3.2 poison-input failure mode —
"Derive facts from a partially-typed package and you write wrong facts
with valid derivations." This is not a frontend gap, it's the missing hard
gate itself, and it's what build-order step 3's poison-input gate fixes
directly against the existing `go/packages`-based frontend, with no
migration needed.

No open question here anymore — removing this as a blocker. Leaving the
entry (struck through) so the correction is visible rather than silently
disappearing.

---

## [2026-08-01] B/F and FBF citations unverified (architecture.md §4.3, §13 step 1)

architecture.md explicitly states: *"B/F and FBF were not verified directly
— confirm authors, venue, and the efficiency claim from primary sources
before citing either. Do not repeat a citation you have not opened."* This
is build-order step 1, timeboxed, before any code.

**Why it's blocking:** Verifying primary-source citations (Motik, Nenov,
Piro & Horrocks AAAI 2015 for B/F; Motik et al. ~2019 for FBF) requires
literature lookup and a human judgment call on how much time is worth
spending — the architecture doc itself flags this as a timebox decision,
not a mechanical task.

**What's needed:** Either explicit go-ahead to spend agent time on a
literature search (with a stated time budget), or confirmation this stays
deferred until closer to §4/DRed implementation (build-order step 9, not
yet reached this session).

**Current call:** Deferred. Not required for steps 2–6, which name DRed and
B/F only in prose/comments, not in enforced behavior.

---

## [2026-08-01] `chi/` directory at repo root (untracked)

A full local clone of go-chi/chi sits at the repo root, untracked in git.
Scripts (`scripts/run-harness.sh`) expect the harness fixture at `/tmp/...`,
not in-repo — so this looks like a manual, ad hoc clone left over from a
prior session, not something the project's tooling put there.

**Why it's blocking:** Could be someone's in-progress work (a specific
pinned chi checkout used for reproducing a harness result) or genuinely
disposable scratch. Deleting or `.gitignore`-ing it without knowing which
risks discarding something.

**What's needed:** Confirm whether `chi/` should be `.gitignore`d,
deleted, or left alone.

**Current call:** Left untouched. Not referenced by any new docs/schema
work in this session.

---

## [2026-08-01] `test/test/*` directory — deliberately gitignored, stale duplicate of `internal/*`

**Update:** `.gitignore` has `/testdata` (since the first commit) and
`/test` (added in the most recent v2 commit, "fixed the bug in incremental
commit changes"). So `test/test/` isn't accidental clutter — the previous
author deliberately excluded it from git, in the same commit that fixed
the incremental engine bug. Its content (`test/test/incremental/engine.go`
still says `DELETE FROM call_edges` where `internal/incremental/engine.go`
has since moved to the generic `facts` table) reads like a manual
copy-scratch-and-compare workspace used while developing that fix, left
behind locally. Consistent with `/testdata` also being gitignored from
commit 1 — `testdata/fixture` (used by every parser/callgraph test) is
itself untracked, so this repo has apparently never actually shipped its
own test fixtures through git; they're expected to already exist in the
working tree. (My new `testdata/broken_fixture`, added for the
poison-input gate tests, is untracked for the same reason — see
docs/DECISIONS.md.)

**Why it's blocking:** Still not clearly safe to delete — "the previous
author's scratch workspace" is a stronger guess than before, but still a
guess. Also worth a decision independent of deletion: should `testdata/`
actually be gitignored? As-is, a fresh clone of this repo cannot run its
own test suite (`internal/parser`, `internal/callgraph`, etc. all depend on
`testdata/fixture` existing) — that seems unintentional, not a design
choice, and may be worth fixing (un-ignore `testdata/`, or add a
fixture-generation script) independent of what happens to `test/test/`.

**What's needed:** Confirm whether `test/test/` can be deleted, and
separately, confirm whether `/testdata` should be un-ignored so the test
suite is actually reproducible from a clean clone.

**Current call:** Left both untouched. New testdata (`broken_fixture`)
follows the existing (gitignored) convention rather than fighting it.

---

## [2026-08-01] ~~`internal/index` is "correct full-rebuild-via-diff," not yet selective~~ — RESOLVED

**Update:** fixed. `ApplyFacts` now actually uses `StaleLiveFacts` +
changed-file detection to scope every open/close/refresh to just what
changed — see the "make ApplyFacts actually selective" commit and
`TestSelective_UntouchedFileFactsAreNeverTouched`, which proves it via
`fact_id` stability (an untouched file's facts keep their exact fact_id
across a commit that only touched a different file). A §10.1 measurement
against real history could now mean something. Original writeup below,
kept for the record.

**What this means concretely:** `ComputeFacts` re-derives facts for
**every** file in the repo on every commit (a full parse pass), and
`ApplyFacts` diffs the result against the currently-live fact set to
decide what to open/close. This is architecturally correct — `RunIndexer`
proves the resulting live-fact-set at each commit matches the real call
graph, including across the exact section 2.2 adversarial case (a new
interface implementer in an unrelated file) — but it means `ApplyFacts`'s
`Closed` count is, by construction, **always exactly** "the facts that
actually differ between this commit and the last," because that's
literally what it's computing (a diff of two full derivations), not a
result of `StaleLiveFacts`-driven selective re-derivation actually
skipping unaffected work.

**Why this blocks step 7 specifically:** architecture.md §10.1's
invalidation-precision measurement is "facts withdrawn ÷ facts that
actually differed under full rebuild." For the current `internal/index`
pipeline, withdrawn-facts and actually-differed-facts are **the same
computation** — the ratio would trivially equal 1.0 on every commit, not
because invalidation is precise, but because there is no invalidation
DECISION being measured yet. Running this measurement now and reporting a
1.0 ratio would misrepresent what's been built — it would look like a
perfect result while actually meaning "we didn't test the thing §10.1
exists to test."

**What real selective invalidation would require, not yet built:** detect
which files changed between consecutive commits (via `git diff
--name-only`, same as v2's `internal/differ`/`internal/incremental`
already do), re-derive facts only for those files plus whatever
`StaleLiveFacts` identifies as transitively affected (interface
implementer-set changes, etc.), and leave every other file's facts
untouched rather than recomputing and re-diffing them. `StaleLiveFacts`
and `UpsertInterfaceImplementers` (steps 5-6) already provide the correct
primitives for this — `internal/index` just isn't using them to skip work
yet, only to prove correctness of the diff-based approach.

**Current call:** did not attempt §10.1's measurement with a misleading
methodology. Proceeding instead with build-order step 8 (the remaining
§9.1 fixtures), which test correctness of the resulting fact set — a
property the current full-rebuild-via-diff pipeline can already
legitimately be judged against, independent of whether it's selective yet.
Building real selective invalidation is flagged here as the prerequisite
for step 7 to mean what it claims to mean.

## [2026-08-01] §10.3 govulncheck differential — scope/resources

architecture.md §10.3 requires selecting real repos, normalizing against
Go's vulnerability database (not raw OSV), and pre-registering a
disagreement taxonomy in git before running the comparison.

**Why it's blocking:** This is a resource/scope decision (which repos, how
many commits, how much of a time budget) explicitly deferred to build-order
step 13 — many steps past where this session's unattended work is starting
(steps 3–6). Not currently blocking anything; noted here so it isn't
silently skipped later without a decision point.

**What's needed:** Nothing yet — revisit when build-order steps 7–12 are
done.

**Current call:** Out of scope for this session; recorded so future
sessions don't silently skip a decision that needs to be made explicitly
when the time comes.
