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

## [2026-08-01] Poison-input gate implemented against go/types.Config, not go/packages

**Maps to:** architecture.md §3.2, with the gap noted in §6 recorded in
docs/FLAGGED.md.

**Decision:** The hard gate ("refuse to index a commit if any loaded
package has errors") is implemented using `go/types.Config.Error` callback
accumulation and `go/parser.ParseFile` error returns, against the existing
hand-rolled frontend — not `go/packages`' `pkg.Errors`/`TypesInfo`
completeness check that §6 specifies.

**Why:** The full frontend migration to `go/packages` (§6) is large,
separate work (flagged in docs/FLAGGED.md) that build-order steps 3–6
don't require completing first. The *mechanism* §3.2 needs — detect
type-check failure, refuse to derive facts from a partially-typed package,
record it in `skipped_commits` — is implementable against the current
frontend's error-reporting surface today. The `skipped_commits` table and
gating logic are written so that swapping the underlying error source
(`go/types.Config.Error` → `pkg.Errors`) later is a small, localized
change, not a rearchitecture — the gate consumes a `hasErrors bool` +
`detail string`, not frontend-specific types.

**Consequence:** Today's gate catches strictly fewer failure modes than
§6's — e.g. it does not catch "package loaded with complete-looking but
actually-incomplete `TypesInfo` due to a missing transitive dependency,"
which is exactly the failure mode §3.2 calls out as the harness's blind
spot. This is a known, narrower approximation, not a silent gap — recorded
so the skip-rate numbers this produces are not over-trusted until the
frontend migration lands.

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

## [2026-08-01] SIGKILL injection test trial count

**Maps to:** architecture.md §3.1 ("≥500 trials, asserting recovery to a
clean, known seq").

**Decision:** Test harness supports a configurable trial count via env var
(`ATLAS_CRASH_TRIALS`, default lower for fast local iteration), with CI/full
validation runs expected to set it to ≥500 per the spec.

**Why:** 500 real SIGKILL trials against a Postgres transaction is slow
(process spawn + DB round trip per trial); defaulting to a smaller number
keeps `go test ./...` fast for routine iteration while preserving the
ability to run the spec's actual required trial count on demand. This is
recorded as a decision (not silently under-testing) — the spec's "≥500" is
honored via an explicit override, not diluted.
