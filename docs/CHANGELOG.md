# Changelog

One line per commit. Human-readable, no filler.

- Add architecture.md diff, CLAUDE.md, and docs scaffolding (PROGRESS/DECISIONS/FLAGGED/CHANGELOG) per Atlas v3.1 rebuild kickoff.
- Tag v2-frozen: freeze v2 (symex) as the correctness oracle before Atlas v3.1 rebuild.
- Add poison-input gate (CheckPoison) against go/packages pkg.Errors/TypesInfo, plus broken-fixture regression tests.
- Add crash-consistency schema (commits/repo_state/skipped_commits) and ApplyDelta single-transaction watermark primitive, verified via SIGKILL injection.
