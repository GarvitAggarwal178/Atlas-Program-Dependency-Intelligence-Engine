package store

import (
	"context"
	"fmt"

	"github.com/yourorg/symex/internal/linearize"
)

// SyncResult reports what SyncCommits did.
type SyncResult struct {
	// TotalCommits is the length of the fresh first-parent walk.
	TotalCommits int
	// NewlyInserted is how many commits were newly inserted into
	// atlas.commits (0 on a repo with no new commits since last sync).
	NewlyInserted int
	// ResumeFromSeq is where fact-derivation should resume from
	// (independent of how many commits are known — this tracks
	// last_applied_seq from repo_state, i.e. how far DERIVATION has
	// progressed, not how far the commit list has been populated).
	ResumeFromSeq int64
}

// SyncCommits is architecture.md section 2.1's "verify it at the start of
// every index run" step, plus populating atlas.commits from a fresh
// first-parent walk.
//
// It: (1) walks repoDir's branch fresh, (2) if a watermark already exists
// (repo_state), verifies the fingerprint over the previously-indexed
// prefix is unchanged — refusing with an error on mismatch, exactly as
// section 2.1 specifies ("Mismatch => refuse to proceed and require a full
// re-index"), (3) inserts any commits at or beyond the current known
// length into atlas.commits.
//
// SyncCommits does NOT advance repo_state.last_applied_seq — that only
// happens as a side effect of ApplyDelta actually deriving facts for a
// seq. Populating atlas.commits and deriving facts for those commits are
// deliberately separate operations: this function can run standalone to
// discover what new commits exist without committing to deriving facts
// for all of them in the same call.
func (s *DB) SyncCommits(ctx context.Context, repoDir, branch, repo string) (*SyncResult, error) {
	walked, err := linearize.Walk(repoDir, branch)
	if err != nil {
		return nil, fmt.Errorf("walk %s@%s: %w", repoDir, branch, err)
	}

	resumeSeq, err := s.ResumeFromSeq(ctx, repo)
	if err != nil {
		return nil, fmt.Errorf("resume from seq: %w", err)
	}

	rs, err := s.GetRepoState(ctx, repo)
	if err != nil {
		return nil, fmt.Errorf("get repo state: %w", err)
	}
	if rs != nil {
		lc := make([]linearize.Commit, len(walked))
		for i, c := range walked {
			lc[i] = linearize.Commit{Seq: c.Seq, CommitHash: c.CommitHash, ParentHash: c.ParentHash}
		}
		verify, err := linearize.VerifyFingerprint(lc, rs.LastAppliedSeq, rs.LinearizationFingerprint)
		if err != nil {
			return nil, fmt.Errorf("verify fingerprint: %w", err)
		}
		if !verify.Stable {
			return nil, fmt.Errorf(
				"linearization fingerprint mismatch for repo %q at seq<=%d: history was rewritten "+
					"(rebase/force-push) — refusing to proceed; a full re-index is required "+
					"(architecture.md section 2.1)",
				repo, rs.LastAppliedSeq,
			)
		}
	}

	rows := make([]Commit, len(walked))
	for i, c := range walked {
		rows[i] = Commit{Repo: repo, Seq: c.Seq, CommitHash: c.CommitHash, ParentHash: c.ParentHash}
	}

	beforeTotal, err := s.CommitCount(ctx, repo)
	if err != nil {
		return nil, fmt.Errorf("count existing commits: %w", err)
	}

	if err := s.InsertCommits(ctx, rows); err != nil {
		return nil, fmt.Errorf("insert commits: %w", err)
	}

	afterTotal, err := s.CommitCount(ctx, repo)
	if err != nil {
		return nil, fmt.Errorf("count commits after insert: %w", err)
	}

	return &SyncResult{
		TotalCommits:  len(walked),
		NewlyInserted: afterTotal - beforeTotal,
		ResumeFromSeq: resumeSeq,
	}, nil
}
