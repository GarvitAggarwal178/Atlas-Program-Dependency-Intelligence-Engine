package store

import (
	"context"
	"database/sql"
	"fmt"
)

// DeltaFunc performs the data writes for one delta (one commit's worth of
// fact insertions/retractions/interval closes) against tx. It must not call
// Commit or Rollback itself — ApplyDelta owns the transaction lifecycle.
type DeltaFunc func(ctx context.Context, tx *sql.Tx) error

// ApplyDelta is the crash-consistency primitive required by architecture.md
// section 3.1.
//
// It runs fn inside a single transaction and, only if fn succeeds, upserts
// repo_state.last_applied_seq = seq and
// repo_state.linearization_fingerprint = fingerprint IN THE SAME
// TRANSACTION before committing.
//
// Why this is the whole mechanism: "A delta at seq = N must close
// intervals, insert facts, insert derivations, adjust reachability support,
// and record that N was applied. Without a recorded watermark, a crash
// mid-delta leaves a store that is neither N-1 nor N, and nothing can
// detect it." Postgres's own crash recovery rolls back any transaction that
// was not committed — so a process killed mid-ApplyDelta leaves the
// database at exactly its state before the call (watermark included). On
// restart, ResumeFromSeq reads last_applied_seq and the caller resumes at
// last_applied_seq+1, which is always a fully-applied, self-consistent
// state to resume from.
//
// If fn returns an error, or the watermark upsert itself fails, the entire
// transaction is rolled back.
func (s *DB) ApplyDelta(ctx context.Context, repo string, seq int64, fingerprint string, fn DeltaFunc) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin delta tx: %w", err)
	}
	defer tx.Rollback() // no-op after a successful Commit

	if err := fn(ctx, tx); err != nil {
		return fmt.Errorf("apply delta seq=%d: %w", seq, err)
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO repo_state (repo, last_applied_seq, linearization_fingerprint)
		VALUES ($1, $2, $3)
		ON CONFLICT (repo) DO UPDATE
		    SET last_applied_seq = EXCLUDED.last_applied_seq,
		        linearization_fingerprint = EXCLUDED.linearization_fingerprint
	`, repo, seq, fingerprint); err != nil {
		return fmt.Errorf("update repo_state watermark seq=%d: %w", seq, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit delta seq=%d: %w", seq, err)
	}
	return nil
}

// ResumeFromSeq returns the seq the engine should resume indexing from: the
// watermark's last_applied_seq + 1, or 0 for a repo with no watermark yet
// (fresh index). architecture.md section 3.1: "On startup, resume from
// last_applied_seq + 1."
func (s *DB) ResumeFromSeq(ctx context.Context, repo string) (int64, error) {
	rs, err := s.GetRepoState(ctx, repo)
	if err != nil {
		return 0, err
	}
	if rs == nil {
		return 0, nil
	}
	return rs.LastAppliedSeq + 1, nil
}
