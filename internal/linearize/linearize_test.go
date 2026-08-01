package linearize_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/yourorg/symex/internal/linearize"
)

// newTestRepo creates a throwaway git repo with n linear commits on
// "main" and returns its path.
func newTestRepo(t *testing.T, n int) string {
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
		fname := filepath.Join(dir, "file.txt")
		content := []byte{byte('a' + i)}
		if err := os.WriteFile(fname, content, 0644); err != nil {
			t.Fatalf("write file: %v", err)
		}
		run("add", "-A")
		run("commit", "-q", "-m", "commit "+string(rune('a'+i)))
	}
	return dir
}

func TestWalk_AssignsSeqOldestFirstWithCorrectParents(t *testing.T) {
	dir := newTestRepo(t, 4)

	commits, err := linearize.Walk(dir, "main")
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(commits) != 4 {
		t.Fatalf("expected 4 commits, got %d", len(commits))
	}

	for i, c := range commits {
		if c.Seq != int64(i) {
			t.Errorf("commits[%d].Seq = %d, want %d", i, c.Seq, i)
		}
		if c.CommitHash == "" {
			t.Errorf("commits[%d].CommitHash is empty", i)
		}
		if i == 0 {
			if c.ParentHash != "" {
				t.Errorf("root commit should have empty ParentHash, got %q", c.ParentHash)
			}
		} else {
			if c.ParentHash != commits[i-1].CommitHash {
				t.Errorf("commits[%d].ParentHash = %q, want %q (commits[%d].CommitHash)",
					i, c.ParentHash, commits[i-1].CommitHash, i-1)
			}
		}
	}

	// Distinct commit hashes.
	seen := make(map[string]bool)
	for _, c := range commits {
		if seen[c.CommitHash] {
			t.Fatalf("duplicate commit hash %s", c.CommitHash)
		}
		seen[c.CommitHash] = true
	}
}

func TestFingerprint_DeterministicAndOrderSensitive(t *testing.T) {
	dir := newTestRepo(t, 5)
	commits, err := linearize.Walk(dir, "main")
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}

	fp1, err := linearize.Fingerprint(commits, 4)
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	fp2, err := linearize.Fingerprint(commits, 4)
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	if fp1 != fp2 {
		t.Fatalf("Fingerprint is not deterministic: %s != %s", fp1, fp2)
	}

	// A shorter prefix must produce a DIFFERENT fingerprint.
	fpPrefix, err := linearize.Fingerprint(commits, 2)
	if err != nil {
		t.Fatalf("Fingerprint prefix: %v", err)
	}
	if fpPrefix == fp1 {
		t.Fatal("fingerprint over a shorter prefix must differ from the full-length fingerprint")
	}
}

// TestVerifyFingerprint_DetectsRebase is the whole point of section 2.1's
// mitigation: a rebase/amend that changes a historical commit's hash must
// be caught, not silently accepted.
func TestVerifyFingerprint_DetectsRebase(t *testing.T) {
	dir := newTestRepo(t, 3)
	before, err := linearize.Walk(dir, "main")
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}

	storedFP, err := linearize.Fingerprint(before, 2)
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}

	result, err := linearize.VerifyFingerprint(before, 2, storedFP)
	if err != nil {
		t.Fatalf("VerifyFingerprint: %v", err)
	}
	if !result.Stable {
		t.Fatal("expected stable history to verify as Stable=true before any rewrite")
	}

	// Simulate a rebase: amend the middle commit, which rewrites its hash
	// AND every descendant's hash (this is exactly what git rebase does).
	cmd := exec.Command("git", "-C", dir, "commit", "--amend", "-q", "-m", "commit b (amended)")
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit --amend: %v\n%s", err, out)
	}

	after, err := linearize.Walk(dir, "main")
	if err != nil {
		t.Fatalf("Walk after amend: %v", err)
	}

	result, err = linearize.VerifyFingerprint(after, 2, storedFP)
	if err != nil {
		t.Fatalf("VerifyFingerprint after amend: %v", err)
	}
	if result.Stable {
		t.Fatal("expected VerifyFingerprint to detect the amend as an unstable rewrite, got Stable=true")
	}
	if result.ComputedFingerprint == storedFP {
		t.Fatal("computed fingerprint after amend must differ from the pre-amend stored fingerprint")
	}
}

func TestVerifyFingerprint_FreshRepoIsTriviallyStable(t *testing.T) {
	dir := newTestRepo(t, 2)
	commits, err := linearize.Walk(dir, "main")
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	result, err := linearize.VerifyFingerprint(commits, -1, "")
	if err != nil {
		t.Fatalf("VerifyFingerprint: %v", err)
	}
	if !result.Stable {
		t.Fatal("a fresh repo with nothing indexed yet (lastAppliedSeq=-1) must be trivially stable")
	}
}
