package index_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/yourorg/symex/internal/index"
	"github.com/yourorg/symex/internal/store"
)

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

const (
	runTestGoMod = "module example.com/runtest\n\ngo 1.21\n"
	mainV0       = "package main\n\nfunc A() {}\n\nfunc main() { A() }\n"
	mainV1       = "package main\n\nfunc A() {}\n\nfunc B() { A() }\n\nfunc main() { A(); B() }\n"
	mainV2       = "package main\n\nfunc A() {}\n\nfunc B() { A() }\n\nfunc C() { B() }\n\nfunc main() { A(); B(); C() }\n"
)

// newRunTestRepo creates a real, buildable Go module with 3 commits, each
// adding a new function and a new direct-call edge, so RunIndexer has
// something genuine to index across a real commit sequence.
func newRunTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitRun(t, dir, "init", "-q", "-b", "main")
	gitRun(t, dir, "config", "user.name", "test")
	gitRun(t, dir, "config", "user.email", "test@example.com")

	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(runTestGoMod), 0644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-q", "-m", "go.mod")

	for _, src := range []string{mainV0, mainV1, mainV2} {
		if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(src), 0644); err != nil {
			t.Fatalf("write main.go: %v", err)
		}
		gitRun(t, dir, "add", "-A")
		gitRun(t, dir, "commit", "-q", "-m", "update main.go")
	}
	return dir
}

func TestRunIndexer_IndexesRealCommitHistory(t *testing.T) {
	db := openIndexTestDB(t)
	repo := uniqueIndexTestRepo(t)
	dir := newRunTestRepo(t)

	origHead, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}

	result, err := index.RunIndexer(context.Background(), db, dir, "main", "example.com/runtest", repo)
	if err != nil {
		t.Fatalf("RunIndexer: %v", err)
	}
	if result.CommitsIndexed != 4 { // go.mod commit + 3 main.go versions
		t.Fatalf("expected 4 commits indexed, got %+v", result)
	}
	if result.CommitsSkipped != 0 {
		t.Fatalf("expected 0 skipped commits (all buildable), got %+v", result)
	}

	// HEAD must be restored to what it was before RunIndexer touched the
	// working tree.
	afterHead, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse HEAD after: %v", err)
	}
	if string(origHead) != string(afterHead) {
		t.Errorf("RunIndexer must restore the original HEAD: before=%s after=%s", origHead, afterHead)
	}

	live, err := store.QueryLiveFacts(context.Background(), db.RawDB(), repo)
	if err != nil {
		t.Fatalf("QueryLiveFacts: %v", err)
	}
	edges := make(map[string]bool)
	for _, f := range live {
		edges[f.SourceSymbol+"->"+f.TargetSymbol] = true
	}

	want := []string{
		"example.com/runtest.main->example.com/runtest.A",
		"example.com/runtest.main->example.com/runtest.B",
		"example.com/runtest.main->example.com/runtest.C",
		"example.com/runtest.B->example.com/runtest.A",
		"example.com/runtest.C->example.com/runtest.B",
	}
	for _, w := range want {
		if !edges[w] {
			t.Errorf("missing expected final-state edge %s (got %d live facts: %v)", w, len(live), edges)
		}
	}

	// Re-running with no new commits must be a full no-op: everything is
	// already applied, so nothing at or after ResumeFromSeq remains.
	result2, err := index.RunIndexer(context.Background(), db, dir, "main", "example.com/runtest", repo)
	if err != nil {
		t.Fatalf("RunIndexer (rerun): %v", err)
	}
	if result2.CommitsIndexed != 0 {
		t.Fatalf("expected a no-op rerun to index 0 commits, got %+v", result2)
	}
}
