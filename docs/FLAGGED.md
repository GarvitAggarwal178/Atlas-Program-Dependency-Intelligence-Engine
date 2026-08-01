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

## [2026-08-01] `go/packages` frontend migration (§6) not yet started

architecture.md §6 is explicit: *"Not hand-rolled `go/parser` + manual
`go/types.Config`... Always check `pkg.Errors`."* The current
`internal/parser` package (v2) uses exactly the hand-rolled approach §6
rejects — standard-library-only `go/parser`/`go/types`, no `go/packages`,
no module/build-tag/vendoring resolution.

**Why it's blocking:** This is large, load-bearing surgery — it changes how
every file is loaded and therefore touches parser, canonicalize (bridges
into parser), callgraph, and the new poison-input gate (§3.2) which
requires `go/packages`' `pkg.Errors`/`TypesInfo` semantics specifically
(the hand-rolled frontend doesn't have an equivalent "the whole package
failed to type-check" signal in the same shape). It is also explicitly
**not** part of build-order steps 3–6 (crash consistency, interval store,
derivation tracking, IMPLEMENTS probe), which can be built against the
existing frontend's output shape (`RepoSymbolTable`) without waiting on this.

**What's needed:** Confirm this is in scope for this pass (it's real,
multi-day work per §16's "conceptual fraction doesn't compress" note) vs.
deferred to a later session. Interim poison-input gate (§3.2) implemented
now checks `go/parser` and `go/types.Config.Error` callback errors as the
best available proxy for "this commit doesn't build" — documented as an
approximation in docs/DECISIONS.md, not the real `pkg.Errors` gate §6/§3.2
actually specify.

**Current call:** Proceeding with steps 3–6 against the existing
`go/parser`+`go/types` frontend, with the poison-input gate implemented as
an approximation (see docs/DECISIONS.md). Flagging the full `go/packages`
migration as separate, larger, not-yet-started work.

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
