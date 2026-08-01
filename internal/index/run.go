package index

import (
	"context"
	"fmt"
	"os/exec"

	"github.com/yourorg/symex/internal/linearize"
	"github.com/yourorg/symex/internal/store"
)

// RunResult aggregates the outcome of indexing a range of commits.
type RunResult struct {
	CommitsIndexed int // ApplyDelta ran and succeeded
	CommitsSkipped int // poison-gated, recorded in atlas.skipped_commits
	TotalOpened    int
	TotalClosed    int
	TotalUnchanged int
}

// RunIndexer is the orchestration loop architecture.md's build order needs
// for step 7 (measuring invalidation precision against the frozen v2
// oracle) and step 8 (the eight fixtures): given a real git repo, it syncs
// the linearized commit list (store.SyncCommits — this is also where a
// rewritten-history repo gets refused, section 2.1), then for every commit
// at or after the resume point, checks out that commit and runs
// IndexCommitFromRepo against it.
//
// repoDir's working tree is left checked out at whatever commit was
// processed last; RunIndexer restores the original HEAD before returning
// (success or error), mirroring internal/incremental's harness (v2) so
// callers don't have to think about it.
func RunIndexer(ctx context.Context, db *store.DB, repoDir, branch, modulePath, repo string) (*RunResult, error) {
	origHead, err := currentHead(repoDir)
	if err != nil {
		return nil, fmt.Errorf("resolve current HEAD: %w", err)
	}
	defer restoreHead(repoDir, origHead)

	sync, err := db.SyncCommits(ctx, repoDir, branch, repo)
	if err != nil {
		return nil, fmt.Errorf("sync commits: %w", err)
	}

	commits, err := db.ListCommits(ctx, repo)
	if err != nil {
		return nil, fmt.Errorf("list commits: %w", err)
	}

	lc := make([]linearize.Commit, len(commits))
	for i, c := range commits {
		lc[i] = linearize.Commit{Seq: c.Seq, CommitHash: c.CommitHash, ParentHash: c.ParentHash}
	}

	result := &RunResult{}
	for _, c := range commits {
		if c.Seq < sync.ResumeFromSeq {
			continue
		}

		if err := checkoutCommit(repoDir, c.CommitHash); err != nil {
			return result, fmt.Errorf("checkout seq=%d %s: %w", c.Seq, c.CommitHash, err)
		}

		fp, err := linearize.Fingerprint(lc, c.Seq)
		if err != nil {
			return result, fmt.Errorf("fingerprint seq=%d: %w", c.Seq, err)
		}

		stats, err := IndexCommitFromRepo(ctx, db, repoDir, modulePath, repo, c.Seq, fp)
		if err != nil {
			return result, fmt.Errorf("index seq=%d (%s): %w", c.Seq, c.CommitHash, err)
		}
		if stats == nil {
			result.CommitsSkipped++
			continue
		}
		result.CommitsIndexed++
		result.TotalOpened += stats.Opened
		result.TotalClosed += stats.Closed
		result.TotalUnchanged += stats.Unchanged
	}

	return result, nil
}

func currentHead(repoDir string) (string, error) {
	out, err := exec.Command("git", "-C", repoDir, "rev-parse", "HEAD").Output()
	if err != nil {
		return "", err
	}
	return string(trimNewline(out)), nil
}

func restoreHead(repoDir, ref string) {
	if ref == "" {
		return
	}
	_ = exec.Command("git", "-C", repoDir, "checkout", "-q", "--detach", ref).Run()
}

func checkoutCommit(repoDir, sha string) error {
	cmd := exec.Command("git", "-C", repoDir, "checkout", "-q", "--detach", sha)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git checkout %s: %w\n%s", sha, err, out)
	}
	return nil
}

func trimNewline(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}
