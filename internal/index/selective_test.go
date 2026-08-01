package index_test

import (
	"context"
	"testing"

	"github.com/yourorg/symex/internal/index"
	"github.com/yourorg/symex/internal/store"
)

// TestSelective_UntouchedFileFactsAreNeverTouched is the actual proof that
// ApplyFacts is selective, not just "happens to produce the same end
// result." A multi-file repo is indexed, then ONE file is edited. Facts
// belonging to the OTHER, untouched file must keep the exact same fact_id
// across the second index run — if they'd been closed and reopened (the
// old full-rebuild-via-diff behavior), they'd get a NEW fact_id, since
// OpenFact always assigns a fresh one.
func TestSelective_UntouchedFileFactsAreNeverTouched(t *testing.T) {
	db := openIndexTestDB(t)
	repo := uniqueIndexTestRepo(t)
	dir := t.TempDir()
	const mod = "example.com/selective"

	writeModule(t, dir, map[string]string{
		"go.mod": "module " + mod + "\n\ngo 1.21\n",
		"a.go": `package main

func A() {}
`,
		"b.go": `package main

func B() { A() }

func main() { B() }
`,
	})

	if _, err := index.IndexCommitFromRepo(context.Background(), db, dir, mod, repo, 0, "fp-0"); err != nil {
		t.Fatalf("index seq=0: %v", err)
	}

	before, err := store.QueryLiveFacts(context.Background(), db.RawDB(), repo)
	if err != nil {
		t.Fatalf("QueryLiveFacts: %v", err)
	}
	if len(before) == 0 {
		t.Fatal("expected some live facts after seq=0")
	}
	beforeIDs := make(map[string]int64) // edge -> fact_id
	for _, f := range before {
		beforeIDs[f.SourceSymbol+"->"+f.TargetSymbol] = f.FactID
	}
	bToAID, ok := beforeIDs[mod+".B->"+mod+".A"]
	if !ok {
		t.Fatalf("expected B->A edge to exist, got %v", beforeIDs)
	}

	// Only touch a.go. b.go's facts (B->A, main->B) should be completely
	// untouched by seq=1's indexing.
	writeModule(t, dir, map[string]string{
		"a.go": `package main

func A() { /* comment added, still does nothing */ }
`,
	})

	if _, err := index.IndexCommitFromRepo(context.Background(), db, dir, mod, repo, 1, "fp-1"); err != nil {
		t.Fatalf("index seq=1: %v", err)
	}

	after, err := store.QueryLiveFacts(context.Background(), db.RawDB(), repo)
	if err != nil {
		t.Fatalf("QueryLiveFacts after: %v", err)
	}
	afterIDs := make(map[string]int64)
	for _, f := range after {
		afterIDs[f.SourceSymbol+"->"+f.TargetSymbol] = f.FactID
	}

	bToAIDAfter, ok := afterIDs[mod+".B->"+mod+".A"]
	if !ok {
		t.Fatal("B->A edge disappeared after touching an unrelated file")
	}
	if bToAIDAfter != bToAID {
		t.Errorf("B->A fact_id changed from %d to %d after only a.go was touched — it was closed and reopened when it shouldn't have been touched at all",
			bToAID, bToAIDAfter)
	}

	mainToBID, ok1 := beforeIDs[mod+".main->"+mod+".B"]
	mainToBIDAfter, ok2 := afterIDs[mod+".main->"+mod+".B"]
	if !ok1 || !ok2 || mainToBID != mainToBIDAfter {
		t.Errorf("main->B fact_id changed (%d -> %d) after only a.go was touched", mainToBID, mainToBIDAfter)
	}
}
