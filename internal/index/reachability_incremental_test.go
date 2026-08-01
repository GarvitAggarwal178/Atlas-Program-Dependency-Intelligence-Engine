package index_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/yourorg/symex/internal/index"
	"github.com/yourorg/symex/internal/parser"
	"github.com/yourorg/symex/internal/reach"
	"github.com/yourorg/symex/internal/store"
)

// indexOneCommit computes facts for repoRoot's CURRENT on-disk state and
// applies them (full pass) to repo at seq, without touching reachability.
// Returns the applied facts so the caller can drive reachability
// maintenance separately (full vs incremental, on separate repo
// namespaces, for comparison).
func indexOneCommit(t *testing.T, db *store.DB, repoRoot, modulePath, repo string, seq int64, fingerprint string) {
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
	facts, _, err := index.ComputeFacts(repoRoot, modulePath, table)
	if err != nil {
		t.Fatalf("ComputeFacts: %v", err)
	}

	err = db.ApplyDelta(context.Background(), repo, seq, fingerprint, func(ctx context.Context, tx *sql.Tx) error {
		allFiles := make(map[string]bool)
		for _, f := range table.Files {
			allFiles[f.FilePath] = true
		}
		_, err := index.ApplyFacts(ctx, tx, repo, seq, facts, allFiles)
		return err
	})
	if err != nil {
		t.Fatalf("ApplyDelta seq=%d: %v", seq, err)
	}
}

func liveReachableSet(t *testing.T, db *store.DB, repo string) map[string]bool {
	t.Helper()
	rows, err := store.QueryLiveReachable(context.Background(), db.RawDB(), repo)
	if err != nil {
		t.Fatalf("QueryLiveReachable: %v", err)
	}
	result := make(map[string]bool, len(rows))
	for _, r := range rows {
		result[r.Symbol] = true
	}
	return result
}

// TestMaintainReachabilityIncremental_MatchesFullRecompute is the
// strongest available proof for the production wiring: the SAME sequence
// of real commits (real Go source, real ApplyFacts) is applied to two
// separate repo namespaces — one maintained via MaintainReachability (full
// recompute) and one via MaintainReachabilityIncremental (the real DRed
// mechanism) — and reachable_symbols must match exactly after every single
// commit, not just at the end.
func TestMaintainReachabilityIncremental_MatchesFullRecompute(t *testing.T) {
	db := openIndexTestDB(t)
	if err := db.ApplySchemaV6(context.Background()); err != nil {
		t.Fatalf("apply schema v6: %v", err)
	}
	repoFull := uniqueIndexTestRepo(t) + ":full"
	repoIncr := uniqueIndexTestRepo(t) + ":incr"
	dir := t.TempDir()
	const mod = "example.com/reachcompare"
	entry := []string{mod + ".main"}

	commits := []map[string]string{
		{ // seq 0: baseline. main -> A -> B -> A cycle, but also main -> C directly.
			"go.mod": "module " + mod + "\n\ngo 1.21\n",
			"main.go": `package main

func A() { B() }
func B() { A() }
func C() {}

func main() { A(); C() }
`,
		},
		{ // seq 1: remove main's call to A (the fixture 8 scenario, live).
			"main.go": `package main

func A() { B() }
func B() { A() }
func C() {}

func main() { C() }
`,
		},
		{ // seq 2: add a brand new function D, called from C.
			"main.go": `package main

func A() { B() }
func B() { A() }
func C() { D() }
func D() {}

func main() { C() }
`,
		},
		{ // seq 3: restore main's call to A (cycle comes back to life).
			"main.go": `package main

func A() { B() }
func B() { A() }
func C() { D() }
func D() {}

func main() { A(); C() }
`,
		},
		{ // seq 4: interface-based dispatch added, exercising a differently-shaped edge set.
			"main.go": `package main

type Speaker interface{ Speak() }
type Dog struct{}
func (d *Dog) Speak() { A() }

func A() { B() }
func B() { A() }
func C() { D() }
func D() {}
func Use(s Speaker) { s.Speak() }

func main() { A(); C(); Use(&Dog{}) }
`,
		},
	}

	for i, files := range commits {
		writeModule(t, dir, files)
		seq := int64(i)
		fp := "fp"

		// Full-recompute path.
		indexOneCommit(t, db, dir, mod, repoFull, seq, fp)
		if err := db.ApplyDelta(context.Background(), repoFull, seq, fp+"-reach", func(ctx context.Context, tx *sql.Tx) error {
			_, _, err := index.MaintainReachability(ctx, tx, repoFull, seq, entry)
			return err
		}); err != nil {
			t.Fatalf("seq=%d: full-recompute MaintainReachability: %v", seq, err)
		}

		// Incremental path: snapshot BEFORE ApplyFacts, apply facts, then
		// maintain reachability incrementally using that snapshot.
		var snapshotEdges []reach.Edge
		var snapshotReachable map[string]bool
		if err := db.ApplyDelta(context.Background(), repoIncr, seq, fp+"-snap", func(ctx context.Context, tx *sql.Tx) error {
			var err error
			snapshotEdges, snapshotReachable, err = index.SnapshotReachabilityState(ctx, tx, repoIncr)
			return err
		}); err != nil {
			t.Fatalf("seq=%d: snapshot delta: %v", seq, err)
		}

		indexOneCommit(t, db, dir, mod, repoIncr, seq, fp)

		if err := db.ApplyDelta(context.Background(), repoIncr, seq, fp+"-reach", func(ctx context.Context, tx *sql.Tx) error {
			_, _, err := index.MaintainReachabilityIncremental(ctx, tx, repoIncr, seq, entry, snapshotEdges, snapshotReachable)
			return err
		}); err != nil {
			t.Fatalf("seq=%d: incremental MaintainReachabilityIncremental: %v", seq, err)
		}

		full := liveReachableSet(t, db, repoFull)
		incr := liveReachableSet(t, db, repoIncr)

		if len(full) != len(incr) {
			t.Fatalf("seq=%d: reachable set size mismatch: full=%v incr=%v", seq, sortedKeysMap(full), sortedKeysMap(incr))
		}
		for sym := range full {
			if !incr[sym] {
				t.Errorf("seq=%d: %s reachable via full recompute but NOT via incremental", seq, sym)
			}
		}
		for sym := range incr {
			if !full[sym] {
				t.Errorf("seq=%d: %s reachable via incremental but NOT via full recompute", seq, sym)
			}
		}
	}
}

func sortedKeysMap(m map[string]bool) []string {
	var keys []string
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
