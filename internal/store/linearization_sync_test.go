package store_test

import (
	"context"
	"database/sql"
	"os"
	"os/exec"
	"testing"

	"github.com/yourorg/symex/internal/linearize"
)

func newSyncTestRepo(t *testing.T, n int) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	run("config", "user.name", "test")
	run("config", "user.email", "test@example.com")
	for i := 0; i < n; i++ {
		fname := dir + string(os.PathSeparator) + "file.txt"
		if err := os.WriteFile(fname, []byte{byte('a' + i)}, 0644); err != nil {
			t.Fatalf("write file: %v", err)
		}
		run("add", "-A")
		run("commit", "-q", "-m", "commit")
	}
	return dir
}

func TestSyncCommits_PopulatesFreshRepo(t *testing.T) {
	db := openTestDB(t)
	repo := uniqueRepoName(t)
	dir := newSyncTestRepo(t, 3)

	result, err := db.SyncCommits(context.Background(), dir, "main", repo)
	if err != nil {
		t.Fatalf("SyncCommits: %v", err)
	}
	if result.TotalCommits != 3 || result.NewlyInserted != 3 {
		t.Fatalf("expected 3 total / 3 newly inserted, got %+v", result)
	}
	if result.ResumeFromSeq != 0 {
		t.Fatalf("expected ResumeFromSeq=0 for a repo with no prior watermark, got %d", result.ResumeFromSeq)
	}

	// Re-running with no new commits must insert 0 new rows (idempotent).
	result2, err := db.SyncCommits(context.Background(), dir, "main", repo)
	if err != nil {
		t.Fatalf("SyncCommits (rerun): %v", err)
	}
	if result2.NewlyInserted != 0 {
		t.Fatalf("expected 0 newly inserted on a no-op rerun, got %d", result2.NewlyInserted)
	}
}

// TestSyncCommits_RefusesOnRebase is the actual end-to-end demonstration of
// architecture.md section 2.1's mitigation, one level up from
// linearize.TestVerifyFingerprint_DetectsRebase: once a watermark has been
// recorded (simulating that facts were derived up to some seq), a rebase
// must cause SyncCommits itself to refuse, not just the underlying
// VerifyFingerprint helper.
func TestSyncCommits_RefusesOnRebase(t *testing.T) {
	db := openTestDB(t)
	repo := uniqueRepoName(t)
	dir := newSyncTestRepo(t, 3)

	if _, err := db.SyncCommits(context.Background(), dir, "main", repo); err != nil {
		t.Fatalf("initial SyncCommits: %v", err)
	}

	// Simulate that facts were derived through seq=2 (the last commit) by
	// recording a watermark the way ApplyDelta would, with the REAL
	// fingerprint SyncCommits would have stored for this history.
	commits, err := linearize.Walk(dir, "main")
	if err != nil {
		t.Fatalf("linearize.Walk: %v", err)
	}
	fp, err := linearize.Fingerprint(commits, int64(len(commits)-1))
	if err != nil {
		t.Fatalf("linearize.Fingerprint: %v", err)
	}
	if err := db.ApplyDelta(context.Background(), repo, 2, fp, func(ctx context.Context, tx *sql.Tx) error {
		return nil // no data writes needed for this test
	}); err != nil {
		t.Fatalf("seed watermark: %v", err)
	}

	// Rewrite history: amend the middle commit.
	amend := exec.Command("git", "-C", dir, "commit", "--amend", "-q", "-m", "amended")
	amend.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
	)
	if out, err := amend.CombinedOutput(); err != nil {
		t.Fatalf("git commit --amend: %v\n%s", err, out)
	}

	_, err = db.SyncCommits(context.Background(), dir, "main", repo)
	if err == nil {
		t.Fatal("expected SyncCommits to refuse after a history rewrite, got nil error")
	}
	t.Logf("got expected refusal: %v", err)
}
