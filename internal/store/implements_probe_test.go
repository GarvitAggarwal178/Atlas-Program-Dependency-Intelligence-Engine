package store_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/yourorg/symex/internal/store"
)

// TestImplementsProbe_ZeroNewInvalidationCode is architecture.md section 8,
// build-order step 6: "Add IMPLEMENTS as a new derived relation and attempt
// to write zero new invalidation code. If it requires new hand-written
// invalidation code, the architectural claim is false and the derivation
// model needs work before another line of it is built."
//
// The proof this test constitutes is in what it does NOT do: every call
// below is to a function that already existed for CALL facts
// (store.OpenFact, store.RecordDerivation, store.StaleLiveFacts,
// store.CloseFactByID — all from interval.go/derivation.go, written in
// build-order steps 4-5 with no knowledge that an IMPLEMENTS kind would
// ever exist). The only NEW code anywhere in this change is this test file
// plus two string constants (FactKindImplements, ProvenanceImplements in
// schema_v4.go) — labels, not invalidation logic. If that's not enough to
// make an IMPLEMENTS fact correctly invalidate when its underlying file
// changes, the section 2.2 architectural claim is false.
func TestImplementsProbe_ZeroNewInvalidationCode(t *testing.T) {
	db := openTestDBWithDerivations(t)
	repo := uniqueRepoName(t)

	const (
		typeSym = "billing.SQLLedger"
		ifaceSym = "billing.Ledger"
		file    = "billing/sql_ledger.go"
	)

	var implementsFactID int64
	err := db.ApplyDelta(context.Background(), repo, 0, "fp-0", func(ctx context.Context, tx *sql.Tx) error {
		var err error
		implementsFactID, err = store.OpenFact(ctx, tx, store.Fact{
			Repo:         repo,
			Kind:         store.FactKindImplements, // the ONLY new fact kind
			SourceSymbol: typeSym,
			TargetSymbol: ifaceSym,
			Provenance:   store.ProvenanceImplements,
			CallSite:     "", // IMPLEMENTS facts have no call site
			SourceFile:   file,
			SourceModule: "example.com/fixture",
			ValidFrom:    0,
		})
		if err != nil {
			return err
		}
		// Record its derivation exactly the way a CALL fact would: this
		// IMPLEMENTS fact was derived by inspecting `file`'s structural
		// content. Same RecordDerivation function, same FILE input kind,
		// nothing IMPLEMENTS-specific about the call.
		return store.RecordDerivation(ctx, tx, store.Derivation{
			FactID:    implementsFactID,
			InputKind: store.InputKindFile,
			InputKey:  file,
			InputHash: "filehash-v1",
		})
	})
	if err != nil {
		t.Fatalf("seed ApplyDelta: %v", err)
	}

	// The file's content changes (e.g. SQLLedger's method bodies are edited
	// — the IMPLEMENTS relationship itself might or might not still hold,
	// but the derivation model doesn't need to know that in advance: it
	// just needs to correctly identify that THIS fact's recorded input no
	// longer matches and must be re-derived).
	stale, err := store.StaleLiveFacts(context.Background(), db.RawDB(), repo, store.InputKindFile, file, "filehash-v2")
	if err != nil {
		t.Fatalf("StaleLiveFacts: %v", err)
	}
	found := false
	for _, f := range stale {
		if f.FactID == implementsFactID {
			found = true
			if f.Kind != store.FactKindImplements {
				t.Errorf("expected returned fact's Kind to be IMPLEMENTS, got %q", f.Kind)
			}
		}
	}
	if !found {
		t.Fatal("PROBE FAILED: the IMPLEMENTS fact was not correctly identified as stale by the existing, unmodified StaleLiveFacts — the section 2.2 architectural claim does not hold without new invalidation code")
	}

	// Negative control: querying with the file's CURRENT (unchanged) hash
	// must NOT flag the fact.
	notStale, err := store.StaleLiveFacts(context.Background(), db.RawDB(), repo, store.InputKindFile, file, "filehash-v1")
	if err != nil {
		t.Fatalf("StaleLiveFacts (unchanged hash): %v", err)
	}
	for _, f := range notStale {
		if f.FactID == implementsFactID {
			t.Fatal("an IMPLEMENTS fact whose recorded FILE hash still matches must not be flagged stale")
		}
	}

	// Close and re-derive, using the same CloseFactByID/OpenFact pair a
	// CALL fact's retract-then-rederive cycle would use. Again: no new
	// function, no IMPLEMENTS-specific code path.
	err = db.ApplyDelta(context.Background(), repo, 1, "fp-1", func(ctx context.Context, tx *sql.Tx) error {
		if err := store.CloseFactByID(ctx, tx, implementsFactID, 1); err != nil {
			return err
		}
		newID, err := store.OpenFact(ctx, tx, store.Fact{
			Repo: repo, Kind: store.FactKindImplements,
			SourceSymbol: typeSym, TargetSymbol: ifaceSym,
			Provenance: store.ProvenanceImplements, CallSite: "",
			SourceFile: file, SourceModule: "example.com/fixture",
			ValidFrom: 1,
		})
		if err != nil {
			return err
		}
		return store.RecordDerivation(ctx, tx, store.Derivation{
			FactID: newID, InputKind: store.InputKindFile, InputKey: file, InputHash: "filehash-v2",
		})
	})
	if err != nil {
		t.Fatalf("re-derive ApplyDelta: %v", err)
	}

	live, err := store.QueryLiveFacts(context.Background(), db.RawDB(), repo)
	if err != nil {
		t.Fatalf("QueryLiveFacts: %v", err)
	}
	liveImplements := 0
	for _, f := range live {
		if f.Kind == store.FactKindImplements && f.SourceSymbol == typeSym && f.TargetSymbol == ifaceSym {
			liveImplements++
		}
	}
	if liveImplements != 1 {
		t.Fatalf("expected exactly 1 live IMPLEMENTS fact for %s -> %s after retract-then-rederive, got %d",
			typeSym, ifaceSym, liveImplements)
	}
}
