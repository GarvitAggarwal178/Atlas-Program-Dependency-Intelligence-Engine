package store_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/yourorg/symex/internal/store"
)

func openTestDBWithFacts(t *testing.T) *store.DB {
	t.Helper()
	db := openTestDB(t) // from crash_test.go — skips if Postgres unreachable
	if err := db.ApplySchemaV4(context.Background()); err != nil {
		t.Fatalf("apply schema v4: %v", err)
	}
	return db
}

func baseFact(repo string) store.Fact {
	return store.Fact{
		Repo:                repo,
		Kind:                store.FactKindCall,
		SourceSymbol:        "pkg.A",
		TargetSymbol:        "pkg.B",
		Provenance:          "direct_call",
		CallSite:            "pkg.A:10",
		SourceFile:          "pkg/a.go",
		SourceModule:        "example.com/pkg",
		SourceModuleVersion: "",
	}
}

// TestOpenFact_VisibleOnlyWithinItsInterval is the core section 5 "query at
// commit C" semantics: a fact opened at seq=2 must not be visible at
// seq=1, must be visible at seq=2 and after (while still open), and once
// closed at seq=5 must not be visible at seq>=5, but must still be visible
// at seq=4.
func TestOpenFact_VisibleOnlyWithinItsInterval(t *testing.T) {
	db := openTestDBWithFacts(t)
	repo := uniqueRepoName(t)
	f := baseFact(repo)
	f.ValidFrom = 2

	var factID int64
	err := db.ApplyDelta(context.Background(), repo, 2, "fp-2", func(ctx context.Context, tx *sql.Tx) error {
		id, err := store.OpenFact(ctx, tx, f)
		factID = id
		return err
	})
	if err != nil {
		t.Fatalf("ApplyDelta (open): %v", err)
	}

	assertVisible := func(seq int64, want bool) {
		t.Helper()
		facts, err := store.QueryFactsAt(context.Background(), db.RawDB(), repo, seq)
		if err != nil {
			t.Fatalf("QueryFactsAt(seq=%d): %v", seq, err)
		}
		got := false
		for _, ff := range facts {
			if ff.FactID == factID {
				got = true
			}
		}
		if got != want {
			t.Errorf("seq=%d: fact visible=%v, want %v", seq, got, want)
		}
	}

	assertVisible(1, false) // before valid_from
	assertVisible(2, true)  // exactly valid_from
	assertVisible(4, true)  // still open

	// Close at seq=5.
	err = db.ApplyDelta(context.Background(), repo, 5, "fp-5", func(ctx context.Context, tx *sql.Tx) error {
		found, err := store.CloseFactByKey(ctx, tx, repo, f.Kind, f.SourceSymbol, f.TargetSymbol,
			f.Provenance, f.CallSite, f.SourceModule, 5)
		if err != nil {
			return err
		}
		if !found {
			t.Error("expected CloseFactByKey to find the live fact")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("ApplyDelta (close): %v", err)
	}

	assertVisible(4, true)  // still visible just before valid_to
	assertVisible(5, false) // valid_to is exclusive: not visible at seq==valid_to
	assertVisible(6, false)

	live, err := store.QueryLiveFacts(context.Background(), db.RawDB(), repo)
	if err != nil {
		t.Fatalf("QueryLiveFacts: %v", err)
	}
	for _, ff := range live {
		if ff.FactID == factID {
			t.Error("closed fact must not appear in QueryLiveFacts")
		}
	}
}

// TestFactsLiveUniq_RejectsSecondOpenInterval is invariant I2: at most one
// open interval per logical fact (repo, kind, source_symbol, target_symbol,
// provenance, call_site, source_module), enforced by the database, not
// just by convention.
func TestFactsLiveUniq_RejectsSecondOpenInterval(t *testing.T) {
	db := openTestDBWithFacts(t)
	repo := uniqueRepoName(t)
	f := baseFact(repo)
	f.ValidFrom = 0

	if err := db.ApplyDelta(context.Background(), repo, 0, "fp-0", func(ctx context.Context, tx *sql.Tx) error {
		_, err := store.OpenFact(ctx, tx, f)
		return err
	}); err != nil {
		t.Fatalf("first OpenFact: %v", err)
	}

	// Attempt to open the SAME natural key again without closing the first
	// — must fail (facts_live_uniq is a partial unique index on
	// valid_to IS NULL).
	f2 := f
	f2.ValidFrom = 1
	err := db.ApplyDelta(context.Background(), repo, 1, "fp-1", func(ctx context.Context, tx *sql.Tx) error {
		_, err := store.OpenFact(ctx, tx, f2)
		return err
	})
	if err == nil {
		t.Fatal("expected opening a second live interval for the same natural key to fail (I2 violated)")
	}

	// The whole delta must have rolled back — repo_state must still be at
	// seq=0, not seq=1, since the seq=1 delta failed.
	rs, err := db.GetRepoState(context.Background(), repo)
	if err != nil {
		t.Fatalf("GetRepoState: %v", err)
	}
	if rs.LastAppliedSeq != 0 {
		t.Fatalf("expected watermark to stay at 0 after the failed seq=1 delta, got %d", rs.LastAppliedSeq)
	}
}

// TestFactsCheckConstraint_RejectsValidToBeforeValidFrom is invariant I3.
func TestFactsCheckConstraint_RejectsValidToBeforeValidFrom(t *testing.T) {
	db := openTestDBWithFacts(t)
	repo := uniqueRepoName(t)

	_, err := db.RawDB().ExecContext(context.Background(), `
		INSERT INTO atlas.facts
		    (repo, kind, source_symbol, target_symbol, provenance, call_site,
		     source_file, source_module, source_module_version, support_count,
		     valid_from, valid_to)
		VALUES ($1, 'CALL', 'pkg.A', 'pkg.B', 'direct_call', 'pkg.A:1',
		        'pkg/a.go', 'example.com/pkg', '', 1, 5, 3)
	`, repo)
	if err == nil {
		t.Fatal("expected the CHECK(valid_to IS NULL OR valid_to > valid_from) constraint to reject valid_to < valid_from")
	}
}
