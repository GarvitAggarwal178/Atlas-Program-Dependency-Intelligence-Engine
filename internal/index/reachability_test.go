package index_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/yourorg/symex/internal/index"
	"github.com/yourorg/symex/internal/parser"
	"github.com/yourorg/symex/internal/store"
)

// indexAndMaintainReachability is a small test-only wrapper combining
// ApplyFacts and MaintainReachability in one delta — real callers would do
// the same thing inside their own ApplyDelta, but IndexCommitFromRepo
// itself deliberately doesn't call MaintainReachability automatically
// (the entry-point model is an open scope question — see
// docs/FLAGGED.md), so tests exercise the combination directly.
func indexAndMaintainReachability(
	t *testing.T, db *store.DB, repoRoot, modulePath, repo string, seq int64, fingerprint string, entryPoints []string,
) {
	t.Helper()
	pkgs, fset, err := parser.LoadPackages(repoRoot)
	if err != nil {
		t.Fatalf("LoadPackages: %v", err)
	}
	poison := parser.CheckPoison(pkgs)
	if !poison.Clean {
		t.Fatalf("expected a clean commit, got poison: %s / %s", poison.Reason, poison.Detail)
	}
	table := parser.BuildSymbolTable(pkgs, fset, repoRoot, modulePath)
	facts, newHashes, err := index.ComputeFacts(repoRoot, modulePath, table)
	if err != nil {
		t.Fatalf("ComputeFacts: %v", err)
	}
	oldHashes, err := store.LiveFileHashes(context.Background(), db.RawDB(), repo)
	if err != nil {
		t.Fatalf("LiveFileHashes: %v", err)
	}
	_ = newHashes
	_ = oldHashes

	err = db.ApplyDelta(context.Background(), repo, seq, fingerprint, func(ctx context.Context, tx *sql.Tx) error {
		// Full pass every time in this test helper (nil changedFiles map
		// means "touch nothing" under the new selective ApplyFacts, so we
		// pass every file explicitly to force a full apply — simplest
		// correct thing for a reachability test, which cares about the
		// END STATE of reachable_symbols, not selective-apply behavior
		// which is already covered elsewhere).
		allFiles := make(map[string]bool)
		for f := range newHashes {
			allFiles[f] = true
		}
		for f := range oldHashes {
			allFiles[f] = true
		}
		if _, err := index.ApplyFacts(ctx, tx, repo, seq, facts, allFiles); err != nil {
			return err
		}
		_, _, err := index.MaintainReachability(ctx, tx, repo, seq, entryPoints)
		return err
	})
	if err != nil {
		t.Fatalf("ApplyDelta seq=%d: %v", seq, err)
	}
}

// TestMaintainReachability_Fixture8ThroughRealSource is the section 4.4
// fixture run through the actual pipeline (real Go source, real facts,
// real reachable_symbols table) instead of synthetic edges — internal/reach's
// tests already prove the algorithm itself; this proves the store wiring
// around it (OpenReachable/CloseReachable/QueryLiveReachable, applied via
// ApplyDelta) actually produces the right end state.
func TestMaintainReachability_Fixture8ThroughRealSource(t *testing.T) {
	db := openIndexTestDB(t)
	if err := db.ApplySchemaV6(context.Background()); err != nil {
		t.Fatalf("apply schema v6: %v", err)
	}
	repo := uniqueIndexTestRepo(t)
	dir := t.TempDir()
	const mod = "example.com/cyclefixture"
	entry := []string{mod + ".main"}

	writeModule(t, dir, map[string]string{
		"go.mod": "module " + mod + "\n\ngo 1.21\n",
		"main.go": `package main

func A() { B() }
func B() { A() }

func main() { A() }
`,
	})

	indexAndMaintainReachability(t, db, dir, mod, repo, 0, "fp-0", entry)

	live, err := store.QueryLiveReachable(context.Background(), db.RawDB(), repo)
	if err != nil {
		t.Fatalf("QueryLiveReachable: %v", err)
	}
	reachable := make(map[string]bool)
	for _, r := range live {
		reachable[r.Symbol] = true
	}
	for _, want := range []string{mod + ".main", mod + ".A", mod + ".B"} {
		if !reachable[want] {
			t.Fatalf("seq=0: expected %s reachable, got %v", want, reachable)
		}
	}

	// Delete the sole external edge into the cycle: main no longer calls A.
	writeModule(t, dir, map[string]string{
		"main.go": `package main

func A() { B() }
func B() { A() }

func main() {}
`,
	})

	indexAndMaintainReachability(t, db, dir, mod, repo, 1, "fp-1", entry)

	liveAfter, err := store.QueryLiveReachable(context.Background(), db.RawDB(), repo)
	if err != nil {
		t.Fatalf("QueryLiveReachable after: %v", err)
	}
	reachableAfter := make(map[string]bool)
	for _, r := range liveAfter {
		reachableAfter[r.Symbol] = true
	}

	if reachableAfter[mod+".A"] {
		t.Error("A must NOT be reachable after main->A is deleted, even though A and B still call each other (this is the whole point of fixture 8)")
	}
	if reachableAfter[mod+".B"] {
		t.Error("B must NOT be reachable after main->A is deleted")
	}
	if !reachableAfter[mod+".main"] {
		t.Error("main itself must still be reachable")
	}
}
