package store_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/yourorg/symex/internal/derive"
	"github.com/yourorg/symex/internal/store"
)

func openTestDBWithDerivations(t *testing.T) *store.DB {
	t.Helper()
	db := openTestDBWithFacts(t) // from interval_test.go
	if err := db.ApplySchemaV5(context.Background()); err != nil {
		t.Fatalf("apply schema v5: %v", err)
	}
	return db
}

// TestSection2_2Fixture is architecture.md section 2.2's worked example,
// end to end at the store level: a dispatch site's fact is derived with an
// INTERFACE input hash over the implementer set {SQLLedger}. A new type
// CacheLedger starts implementing the same interface — UpsertInterfaceImplementers
// must report changed=true with a new hash, and StaleLiveFacts must find
// the dispatch site's fact as needing withdrawal and re-derivation. An
// UNRELATED fact (no derivation on this interface) must NOT be flagged.
func TestSection2_2Fixture(t *testing.T) {
	db := openTestDBWithDerivations(t)
	repo := uniqueRepoName(t)
	const iface = "billing.Ledger"

	var dispatchFactID, unrelatedFactID int64
	oldHash := derive.ImplementerSetHash([]string{"billing.SQLLedger"})

	// Seed at seq=0: the dispatch site's fact, derived from FILE + INTERFACE
	// (oldHash), plus an unrelated fact that has nothing to do with this
	// interface.
	err := db.ApplyDelta(context.Background(), repo, 0, "fp-0", func(ctx context.Context, tx *sql.Tx) error {
		var err error
		dispatchFactID, err = store.OpenFact(ctx, tx, store.Fact{
			Repo: repo, Kind: store.FactKindCall,
			SourceSymbol: "payment.(Processor).ProcessPayment",
			TargetSymbol: "billing.Ledger.Debit",
			Provenance:   "interface_resolved",
			CallSite:     "payment/processor.go:36",
			SourceFile:   "payment/processor.go",
			SourceModule: "example.com/fixture",
			ValidFrom:    0,
		})
		if err != nil {
			return err
		}
		if err := store.RecordDerivations(ctx, tx, []store.Derivation{
			{FactID: dispatchFactID, InputKind: store.InputKindFile, InputKey: "payment/processor.go", InputHash: "filehash-1"},
			{FactID: dispatchFactID, InputKind: store.InputKindInterface, InputKey: iface, InputHash: oldHash},
		}); err != nil {
			return err
		}

		unrelatedFactID, err = store.OpenFact(ctx, tx, store.Fact{
			Repo: repo, Kind: store.FactKindCall,
			SourceSymbol: "pkg.Foo", TargetSymbol: "pkg.Bar",
			Provenance: "direct_call", CallSite: "pkg/foo.go:1",
			SourceFile: "pkg/foo.go", SourceModule: "example.com/fixture",
			ValidFrom: 0,
		})
		if err != nil {
			return err
		}
		return store.RecordDerivation(ctx, tx, store.Derivation{
			FactID: unrelatedFactID, InputKind: store.InputKindFile, InputKey: "pkg/foo.go", InputHash: "filehash-2",
		})
	})
	if err != nil {
		t.Fatalf("seed ApplyDelta: %v", err)
	}

	// Establish the interface_implementers row explicitly at the old hash
	// (mirrors what the real build pipeline would have done when it first
	// derived the dispatch fact).
	if err := db.ApplyDelta(context.Background(), repo, 1, "fp-1", func(ctx context.Context, tx *sql.Tx) error {
		changed, err := store.UpsertInterfaceImplementers(ctx, tx, repo, iface, oldHash, 0)
		if err != nil {
			return err
		}
		if !changed {
			t.Fatal("expected the FIRST UpsertInterfaceImplementers call to report changed=true (no prior row)")
		}
		return nil
	}); err != nil {
		t.Fatalf("seed interface_implementers: %v", err)
	}

	// A no-op update (same implementer set) must report changed=false and
	// must NOT flag the dispatch fact as stale.
	newHashSame := derive.ImplementerSetHash([]string{"billing.SQLLedger"})
	if err := db.ApplyDelta(context.Background(), repo, 2, "fp-2", func(ctx context.Context, tx *sql.Tx) error {
		changed, err := store.UpsertInterfaceImplementers(ctx, tx, repo, iface, newHashSame, 2)
		if err != nil {
			return err
		}
		if changed {
			t.Fatal("expected an unchanged implementer set to report changed=false")
		}
		return nil
	}); err != nil {
		t.Fatalf("no-op ApplyDelta: %v", err)
	}

	// THE FIXTURE: a new implementer (CacheLedger) appears.
	newHash := derive.ImplementerSetHash([]string{"billing.SQLLedger", "billing.CacheLedger"})
	if newHash == oldHash {
		t.Fatal("test setup error: hashes should differ")
	}

	var staleFactIDs []int64
	err = db.ApplyDelta(context.Background(), repo, 3, "fp-3", func(ctx context.Context, tx *sql.Tx) error {
		changed, err := store.UpsertInterfaceImplementers(ctx, tx, repo, iface, newHash, 3)
		if err != nil {
			return err
		}
		if !changed {
			t.Fatal("expected UpsertInterfaceImplementers to report changed=true when the implementer set changes")
		}

		stale, err := store.StaleLiveFacts(ctx, tx, repo, store.InputKindInterface, iface, newHash)
		if err != nil {
			return err
		}
		for _, f := range stale {
			staleFactIDs = append(staleFactIDs, f.FactID)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("fixture ApplyDelta: %v", err)
	}

	foundDispatch, foundUnrelated := false, false
	for _, id := range staleFactIDs {
		if id == dispatchFactID {
			foundDispatch = true
		}
		if id == unrelatedFactID {
			foundUnrelated = true
		}
	}
	if !foundDispatch {
		t.Error("expected the dispatch-site fact to be flagged stale after its interface gained a new implementer (this IS the section 2.2 fix)")
	}
	if foundUnrelated {
		t.Error("the unrelated fact (no derivation on this interface) must NOT be flagged stale")
	}

	// Now actually close the stale fact and confirm StaleLiveFacts no
	// longer returns it (it's not "live" anymore, so it's out of scope for
	// further invalidation until re-derived and re-opened).
	if err := db.ApplyDelta(context.Background(), repo, 4, "fp-4", func(ctx context.Context, tx *sql.Tx) error {
		return store.CloseFactByID(ctx, tx, dispatchFactID, 4)
	}); err != nil {
		t.Fatalf("close stale fact: %v", err)
	}

	staleAfterClose, err := store.StaleLiveFacts(context.Background(), db.RawDB(), repo, store.InputKindInterface, iface, newHash)
	if err != nil {
		t.Fatalf("StaleLiveFacts after close: %v", err)
	}
	for _, f := range staleAfterClose {
		if f.FactID == dispatchFactID {
			t.Error("a closed fact must not still be reported by StaleLiveFacts")
		}
	}
}
