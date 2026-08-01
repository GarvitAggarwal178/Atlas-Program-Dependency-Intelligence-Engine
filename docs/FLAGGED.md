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

## [2026-08-01] ~~B/F and FBF citations unverified~~ — RESOLVED, verified from primary sources

**Update:** verified via WebSearch + WebFetch against Motik's own Oxford
CS publication page and the AAAI/journal records directly (not a
secondhand summary):

- **B/F**: Motik, B., Nenov, Y., Piro, R., & Horrocks, I. (2015).
  "Incremental Update of Datalog Materialisation: the Backward/Forward
  Algorithm." *Proceedings of the AAAI Conference on Artificial
  Intelligence*, 29(1), pp. 1560–1568. AAAI-15, Austin, Texas, Jan 25–30
  2015. Confirmed via `ojs.aaai.org` (the paper's own AAAI page) and the
  PDF hosted directly on Motik's Oxford page.
  architecture.md's efficiency claim — "B/F exists specifically because
  DRed is inefficient when facts have many alternative derivations" — is
  accurate to the paper's own stated motivation, confirmed by the
  abstract/summary text, not just the title.
- **FBF**: same four authors — Motik, Nenov, Piro, Horrocks (2019).
  "Maintenance of Datalog Materialisations Revisited." *Artificial
  Intelligence*, Volume 269, pp. 76–136. architecture.md's "~2019" hedge
  is exact (not approximate), and its claim that FBF "reportedly also
  presents counting that handles recursive rules" is confirmed: the paper
  covers three algorithms (counting, DRed, FBF) with theoretical
  complexity analysis, and states both DRed and B/F are instances of FBF
  under specific parameter choices — i.e. FBF is the more general
  algorithm the other two specialize from, exactly as architecture.md's
  framing implies.

Both citations check out; nothing needs correcting in architecture.md's
§4.3 text. Recorded here so the verification itself (sources checked,
not just "trust me") is on the record, per the section 13 build-order
requirement to do this before writing any comparative sentence.

## [2026-08-01] B/F and FBF citations unverified (architecture.md §4.3, §13 step 1) — superseded, see above

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

## [2026-08-01] MODULE deltas: version tracking is real, fact invalidation isn't wired — and shouldn't be faked

`internal/modver` (real go.mod parsing) and `atlas.dependency_versions`
(real interval-maintenance, tested) exist now. What's deliberately NOT
built: actually tying any fact to a dependency's version so a bump
withdraws it.

**Why not:** the current fact-derivation model (`internal/index.ComputeFacts`)
only derives facts from the analyzed repo's OWN source — `LoadPackages`
loads `"./..."`, the main module's own packages. It does not parse or
derive facts from a dependency's source, and `collectAllInterfaces` (see
the cross-package fix above) only scans `pkgs` from that same "./..." load
— it never sees a dependency's own interface declarations either. That
means there is currently no fact in the system whose correctness actually
depends on a specific dependency version: nothing to honestly attach a
`MODULE` derivation to.

I could have wired `UpsertDependencyVersion`'s `changed` signal into
`StaleLiveFacts(MODULE, ...)` calls anyway, but every fact's `MODULE`
input would be fabricated — recorded without ever having been consulted
during derivation. That would look like working invalidation in a demo
and silently invalidate nothing real, which is worse than not building it:
it's the over-invalidation-is-a-number, under-invalidation-is-a-bug
asymmetry from section 2.2, except inverted into "looks wired, isn't."

**What real MODULE-delta support requires:** expanding `ComputeFacts` (or
a parallel path) to also derive facts from dependency-module source —
loading and scanning packages outside `"./..."` with `NeedSyntax` — which
is a real, separate expansion of the frontend's scope, not a small addition
on top of what exists. Flagging as a real next task rather than
half-building it.

## [2026-08-01] Still open: govulncheck differential (§10.3) and performance crossover (§10.2)

Of the three lower-priority items grouped together (citation verification,
govulncheck differential, performance crossover), only citation
verification got done this round (see the resolved entry above) — it was
the one bounded, tool-available task in the group. The other two are
genuinely different in kind, not just bigger:

- **§10.3 govulncheck differential** needs picking real target repos, a
  real OSV/govuln database integration, and a pre-registered disagreement
  taxonomy committed before running anything — real scope/resource
  decisions, not more coding against what already exists.
- **§10.2 performance crossover** needs a stated load model, real
  hardware description, and repeat-count/variance methodology — the
  architecture doc is explicit that "one run on a laptop is not a number."

Both are architecture.md's own explicitly-labeled "findings" steps (12-13
of 15), correctly positioned after everything build-order steps 3-9
deliver (which is now fully done, including real incremental DRed and a
real, evidenced §10.1 measurement). Not started — genuinely needs a
resourcing/scope decision from you, not just more autonomous coding time.

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
