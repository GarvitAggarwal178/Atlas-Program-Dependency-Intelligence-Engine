package index_test

import (
	"context"
	"testing"

	"github.com/yourorg/symex/internal/index"
	"github.com/yourorg/symex/internal/store"
)

// TestAnalyzerVersionChange_ForcesFullReapply is invariant I13 wired
// through the real pipeline: if the recorded analyzer_version doesn't
// match the current build's AnalyzerVersion(), every file must be treated
// as changed on the next index run, even if none of their content hashes
// actually moved.
func TestAnalyzerVersionChange_ForcesFullReapply(t *testing.T) {
	db := openIndexTestDB(t)
	repo := uniqueIndexTestRepo(t)
	dir := t.TempDir()
	const mod = "example.com/analyzerversion"

	writeModule(t, dir, map[string]string{
		"go.mod": "module " + mod + "\n\ngo 1.21\n",
		"main.go": `package main

func A() {}
func main() { A() }
`,
	})

	if _, err := index.IndexCommitFromRepo(context.Background(), db, dir, mod, repo, 0, "fp-0"); err != nil {
		t.Fatalf("index seq=0: %v", err)
	}

	live0, err := store.QueryLiveFacts(context.Background(), db.RawDB(), repo)
	if err != nil {
		t.Fatalf("QueryLiveFacts: %v", err)
	}
	if len(live0) == 0 {
		t.Fatal("expected some live facts after seq=0")
	}

	// Baseline: re-index the SAME unchanged source with a MATCHING
	// analyzer version. Nothing changed on disk, so this should be a
	// genuine no-op — ApplyFacts's touched() predicate should skip every
	// fact entirely (Unchanged stays 0, same as the established no-op
	// behavior elsewhere in this package, e.g. run_test.go's rerun check).
	baselineStats, err := index.IndexCommitFromRepo(context.Background(), db, dir, mod, repo, 1, "fp-1")
	if err != nil {
		t.Fatalf("index seq=1 (baseline, matching version): %v", err)
	}
	if baselineStats.Unchanged != 0 || baselineStats.Opened != 0 || baselineStats.Closed != 0 {
		t.Fatalf("expected a true no-op (matching analyzer version, no file changes) to touch nothing, got %+v", baselineStats)
	}

	// Now simulate an analyzer upgrade: overwrite the recorded version to
	// something that's definitely not the current build's real value.
	if err := db.SetAnalyzerVersion(context.Background(), repo, "definitely-not-the-real-version"); err != nil {
		t.Fatalf("SetAnalyzerVersion: %v", err)
	}

	// Re-index the SAME unchanged source again at seq=2. Without I13,
	// this would ALSO be a no-op (no file hash changed). With I13 wired,
	// every file must be forced into changedFiles, so every live fact gets
	// touched — refreshed in place (same natural key, so Unchanged++, not
	// necessarily a new fact_id: churning identity for an edge that didn't
	// actually change would be unnecessary and wrong; what I13 promises is
	// that the derivation gets RE-VALIDATED, which the "Unchanged" path
	// already does via RecordDerivations).
	forcedStats, err := index.IndexCommitFromRepo(context.Background(), db, dir, mod, repo, 2, "fp-2")
	if err != nil {
		t.Fatalf("index seq=2 (forced, mismatched version): %v", err)
	}
	if forcedStats.Unchanged == 0 {
		t.Errorf("expected an analyzer-version mismatch to force every live fact to be touched (Unchanged > 0), got %+v — I13 isn't actually forcing anything", forcedStats)
	}

	live2, err := store.QueryLiveFacts(context.Background(), db.RawDB(), repo)
	if err != nil {
		t.Fatalf("QueryLiveFacts after: %v", err)
	}
	if len(live2) != len(live0) {
		t.Fatalf("expected the same NUMBER of live facts (source never actually changed), got %d vs %d", len(live2), len(live0))
	}

	// And the recorded version should now be back to the real one.
	recorded, ok, err := db.GetAnalyzerVersion(context.Background(), repo)
	if err != nil {
		t.Fatalf("GetAnalyzerVersion: %v", err)
	}
	if !ok || recorded != index.AnalyzerVersion() {
		t.Errorf("expected recorded analyzer version to be updated to the real current value, got ok=%v recorded=%q", ok, recorded)
	}
}
