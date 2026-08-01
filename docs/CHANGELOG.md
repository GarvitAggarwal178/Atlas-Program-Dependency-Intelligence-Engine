# Changelog

One line per commit. Human-readable, no filler.

- Add architecture.md diff, CLAUDE.md, and docs scaffolding (PROGRESS/DECISIONS/FLAGGED/CHANGELOG) per Atlas v3.1 rebuild kickoff.
- Tag v2-frozen: freeze v2 (symex) as the correctness oracle before Atlas v3.1 rebuild.
- Add poison-input gate (CheckPoison) against go/packages pkg.Errors/TypesInfo, plus broken-fixture regression tests.
- Add crash-consistency schema (commits/repo_state/skipped_commits) and ApplyDelta single-transaction watermark primitive, verified via SIGKILL injection.
- Move v3.1 tables into a dedicated atlas Postgres schema to avoid colliding with v2's existing facts table.
- Add internal/linearize (first-parent commit walk + rebase-detecting fingerprint) and the atlas.facts interval-based schema with OpenFact/CloseFact/QueryFactsAt.
- Add derivation tracking (atlas.derivations, atlas.interface_implementers) with implementer-set hashing and the StaleLiveFacts invalidation query; section 2.2 soundness fixture passes end to end.
- Add and PASS the section 8 IMPLEMENTS probe: a new fact kind requires zero new invalidation code, confirmed by diff, not just argument.
- Add SyncCommits: populates atlas.commits from a fresh linearize.Walk and refuses on a detected history rewrite (real end-to-end rebase-refusal test, not just unit-level).
- Add internal/index (ComputeFacts/ApplyFacts/IndexCommitFromRepo): the real parser-to-interval-store pipeline, with an end-to-end section 2.2 fixture run against actual parsed Go source instead of hand-built facts, and the poison gate wired in for real.
- Add store.ListCommits and index.RunIndexer: drives IndexCommitFromRepo across a real multi-commit git history via real checkouts.
- Fix CheckPoison: zero loaded packages (e.g. a go.mod-only commit) is Clean, not poison — found via RunIndexer's end-to-end test against real history.
- Add section 9.1 fixtures 1, 2, 4, 6, 7 as index-pipeline integration tests; flag that internal/index is full-rebuild-via-diff, not yet selective, so a section 10.1 measurement would be misleading right now.
