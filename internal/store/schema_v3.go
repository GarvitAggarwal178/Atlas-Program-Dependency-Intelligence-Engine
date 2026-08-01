package store

import (
	"context"
	"database/sql"
	"fmt"
)

// SchemaV3 adds the tables architecture.md section 5 requires for a
// linearized, crash-consistent commit sequence: the linearized commit list
// itself, the crash-consistency watermark, and the poison-input skip log.
//
// SchemaV3 lives in its own Postgres schema, "atlas", not the default
// "public" schema v2's tables occupy. architecture.md section 5 names one
// of its tables "facts" — the exact same name as v2's existing snapshot
// table (store.go's Schema constant). Since v2 must stay live, untouched,
// and queryable as the frozen oracle (architecture.md section 9.2) in the
// SAME database, "CREATE TABLE IF NOT EXISTS facts" for the v3.1 shape
// would either collide with or silently no-op against v2's differently-
// shaped table. A dedicated schema namespace ("atlas".*) sidesteps this
// entirely and keeps the two engines' storage fully separate while sharing
// one Postgres instance — see docs/DECISIONS.md.
const SchemaV3 = `
CREATE SCHEMA IF NOT EXISTS atlas;

-- Linearized commit sequence (first-parent walk of one named branch).
-- seq is the ONLY temporal coordinate anywhere in the v3.1 schema;
-- authored_at is decorative and must never appear in an ORDER BY.
CREATE TABLE IF NOT EXISTS atlas.commits (
    repo        TEXT   NOT NULL,
    seq         BIGINT NOT NULL,
    commit_hash TEXT   NOT NULL,
    parent_hash TEXT,
    authored_at TIMESTAMPTZ,
    PRIMARY KEY (repo, seq),
    UNIQUE (repo, commit_hash)
);

-- Crash-consistency watermark. Updated in the SAME transaction as the
-- fact writes for last_applied_seq, per architecture.md section 3.1 — a
-- delta is never considered applied unless its watermark write committed
-- atomically with its data writes.
CREATE TABLE IF NOT EXISTS atlas.repo_state (
    repo                       TEXT PRIMARY KEY,
    last_applied_seq          BIGINT NOT NULL,
    linearization_fingerprint TEXT NOT NULL
);

-- Commits deliberately not indexed because go/packages reported errors or
-- incomplete TypesInfo for at least one loaded package (architecture.md
-- section 3.2). The skip rate computed from this table is part of every
-- reported result, not a footnote.
CREATE TABLE IF NOT EXISTS atlas.skipped_commits (
    repo TEXT NOT NULL,
    seq  BIGINT NOT NULL,
    reason TEXT NOT NULL,   -- 'type_errors' | 'module_unavailable' | 'toolchain'
    detail TEXT,
    PRIMARY KEY (repo, seq)
);
`

// Commit is a single row in the linearized commits table.
type Commit struct {
	Repo       string
	Seq        int64
	CommitHash string
	ParentHash string // "" for the root commit
}

// RepoState is the crash-consistency watermark for one repo.
type RepoState struct {
	Repo                      string
	LastAppliedSeq            int64
	LinearizationFingerprint  string
}

// SkippedCommit is a single row in skipped_commits.
type SkippedCommit struct {
	Repo   string
	Seq    int64
	Reason string // matches the poison.Result.Reason vocabulary
	Detail string
}

const (
	SkipReasonTypeErrors       = "type_errors"
	SkipReasonModuleUnavailable = "module_unavailable"
	SkipReasonToolchain        = "toolchain"
)

// ApplySchemaV3 creates the commits/repo_state/skipped_commits tables.
func (s *DB) ApplySchemaV3(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, SchemaV3); err != nil {
		return fmt.Errorf("apply schema v3: %w", err)
	}
	return nil
}

// InsertCommits bulk-inserts the linearized commit sequence for a repo.
// Idempotent: re-running with the same (repo, seq) rows is a no-op.
func (s *DB) InsertCommits(ctx context.Context, commits []Commit) error {
	if len(commits) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO atlas.commits (repo, seq, commit_hash, parent_hash)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (repo, seq) DO NOTHING
	`)
	if err != nil {
		return fmt.Errorf("prepare commit insert: %w", err)
	}
	defer stmt.Close()

	for _, c := range commits {
		var parent interface{}
		if c.ParentHash != "" {
			parent = c.ParentHash
		}
		if _, err := stmt.ExecContext(ctx, c.Repo, c.Seq, c.CommitHash, parent); err != nil {
			return fmt.Errorf("insert commit seq=%d: %w", c.Seq, err)
		}
	}
	return tx.Commit()
}

// GetRepoState returns the crash-consistency watermark for repo, or
// (nil, nil) if no watermark has ever been written (fresh repo, index from
// seq 0).
func (s *DB) GetRepoState(ctx context.Context, repo string) (*RepoState, error) {
	var rs RepoState
	err := s.db.QueryRowContext(ctx, `
		SELECT repo, last_applied_seq, linearization_fingerprint
		FROM atlas.repo_state WHERE repo = $1
	`, repo).Scan(&rs.Repo, &rs.LastAppliedSeq, &rs.LinearizationFingerprint)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get repo_state: %w", err)
	}
	return &rs, nil
}

// RecordSkippedCommit records that seq was deliberately not indexed.
func (s *DB) RecordSkippedCommit(ctx context.Context, sc SkippedCommit) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO atlas.skipped_commits (repo, seq, reason, detail)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (repo, seq) DO UPDATE SET reason = EXCLUDED.reason, detail = EXCLUDED.detail
	`, sc.Repo, sc.Seq, sc.Reason, sc.Detail)
	if err != nil {
		return fmt.Errorf("record skipped commit seq=%d: %w", sc.Seq, err)
	}
	return nil
}

// ListCommits returns every row in atlas.commits for repo, ordered by seq
// ascending (oldest first) — the order the indexer must apply them in.
func (s *DB) ListCommits(ctx context.Context, repo string) ([]Commit, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT seq, commit_hash, COALESCE(parent_hash, '')
		FROM atlas.commits WHERE repo = $1 ORDER BY seq ASC
	`, repo)
	if err != nil {
		return nil, fmt.Errorf("list commits: %w", err)
	}
	defer rows.Close()

	var result []Commit
	for rows.Next() {
		c := Commit{Repo: repo}
		if err := rows.Scan(&c.Seq, &c.CommitHash, &c.ParentHash); err != nil {
			return nil, fmt.Errorf("scan commit: %w", err)
		}
		result = append(result, c)
	}
	return result, rows.Err()
}

// CommitCount returns the number of rows in atlas.commits for repo.
func (s *DB) CommitCount(ctx context.Context, repo string) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM atlas.commits WHERE repo = $1`, repo,
	).Scan(&n); err != nil {
		return 0, fmt.Errorf("count commits: %w", err)
	}
	return n, nil
}

// SkipRate returns (skipped, total, rate) for a repo — the fraction of
// commits in the linearized sequence that were refused by the poison-input
// gate. architecture.md section 3.2: "The skip rate is part of every
// reported result, not a footnote."
func (s *DB) SkipRate(ctx context.Context, repo string) (skipped, total int, err error) {
	if err = s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM atlas.commits WHERE repo = $1`, repo,
	).Scan(&total); err != nil {
		return 0, 0, fmt.Errorf("count commits: %w", err)
	}
	if err = s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM atlas.skipped_commits WHERE repo = $1`, repo,
	).Scan(&skipped); err != nil {
		return 0, 0, fmt.Errorf("count skipped_commits: %w", err)
	}
	return skipped, total, nil
}
