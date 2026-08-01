package index_test

import (
	"context"
	"testing"

	"github.com/yourorg/symex/internal/index"
)

// TestCacheHitRate_ReflectsWhatActuallyChanged is architecture.md section
// 2.3's reporting requirement: "Report cache-hit rate as a diagnostic."
// First commit: nothing was cached yet, hit rate must be 0. Second
// commit, touching only 1 of 3 files: hit rate must reflect the other 2
// being skipped.
func TestCacheHitRate_ReflectsWhatActuallyChanged(t *testing.T) {
	db := openIndexTestDB(t)
	repo := uniqueIndexTestRepo(t)
	dir := t.TempDir()
	const mod = "example.com/cachehitrate"

	writeModule(t, dir, map[string]string{
		"go.mod": "module " + mod + "\n\ngo 1.21\n",
		"a.go":    "package main\n\nfunc A() {}\n",
		"b.go":    "package main\n\nfunc B() {}\n",
		"main.go": "package main\n\nfunc main() { A(); B() }\n",
	})

	stats0, err := index.IndexCommitFromRepo(context.Background(), db, dir, mod, repo, 0, "fp-0")
	if err != nil {
		t.Fatalf("index seq=0: %v", err)
	}
	if stats0.FilesTotal != 3 {
		t.Fatalf("expected 3 known files at seq=0, got %d", stats0.FilesTotal)
	}
	if stats0.CacheHitRate() != 0 {
		t.Errorf("expected 0%% cache hit rate on the very first commit (nothing was cached yet), got %.2f", stats0.CacheHitRate())
	}

	// Touch only a.go.
	writeModule(t, dir, map[string]string{
		"a.go": "package main\n\nfunc A() { /* touched */ }\n",
	})

	stats1, err := index.IndexCommitFromRepo(context.Background(), db, dir, mod, repo, 1, "fp-1")
	if err != nil {
		t.Fatalf("index seq=1: %v", err)
	}
	if stats1.FilesTotal != 3 {
		t.Fatalf("expected 3 known files at seq=1, got %d", stats1.FilesTotal)
	}
	if stats1.FilesChanged != 1 {
		t.Fatalf("expected exactly 1 changed file at seq=1, got %d", stats1.FilesChanged)
	}
	wantRate := 2.0 / 3.0
	if got := stats1.CacheHitRate(); got < wantRate-0.001 || got > wantRate+0.001 {
		t.Errorf("expected cache hit rate ~%.4f (2 of 3 files skipped), got %.4f", wantRate, got)
	}
}
