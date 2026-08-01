package store

import (
	"context"
	"database/sql"
	"fmt"
)

// RecordDerivation inserts one (fact_id, input_kind, input_key, input_hash)
// row. Must be called with the *sql.Tx from an in-flight ApplyDelta call —
// same reasoning as OpenFact/CloseFact* in interval.go: a derivation
// recorded outside the fact's own write transaction could observe a
// fact_id that never actually committed.
func RecordDerivation(ctx context.Context, tx *sql.Tx, d Derivation) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO atlas.derivations (fact_id, input_kind, input_key, input_hash)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (fact_id, input_kind, input_key) DO UPDATE SET input_hash = EXCLUDED.input_hash
	`, d.FactID, d.InputKind, d.InputKey, d.InputHash)
	if err != nil {
		return fmt.Errorf("record derivation fact_id=%d %s/%s: %w", d.FactID, d.InputKind, d.InputKey, err)
	}
	return nil
}

// RecordDerivations is a convenience loop over RecordDerivation for the
// common case of a fact with multiple recorded inputs (e.g. an
// interface-dispatched call depends on FILE, INTERFACE, and TYPE all at
// once).
func RecordDerivations(ctx context.Context, tx *sql.Tx, ds []Derivation) error {
	for _, d := range ds {
		if err := RecordDerivation(ctx, tx, d); err != nil {
			return err
		}
	}
	return nil
}

// StaleLiveFacts is architecture.md section 2.2's core query: "withdraw
// exactly the facts whose recorded inputs changed." It returns every
// currently-live fact (valid_to IS NULL) that recorded a derivation on
// (inputKind, inputKey) whose stored input_hash no longer matches
// currentHash — i.e. every fact that must be closed and re-derived because
// one of its inputs moved.
//
// This is the ENTIRE invalidation mechanism for a changed input: no
// per-trigger-type hand-written rule, just "compare the recorded hash to
// the current one." Callers drive this per changed input (a changed file's
// FILE hash, a changed interface's IMPLEMENTER-SET hash via
// UpsertInterfaceImplementers, a changed type's TYPE hash, a bumped
// MODULE version) — the query itself doesn't know or care which kind of
// input triggered it.
func StaleLiveFacts(ctx context.Context, q queryer, repo, inputKind, inputKey, currentHash string) ([]Fact, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT f.fact_id, f.repo, f.kind, f.source_symbol, f.target_symbol, f.provenance, f.call_site,
		       f.source_file, f.source_module, f.source_module_version, f.support_count,
		       f.valid_from, f.valid_to
		FROM atlas.facts f
		JOIN atlas.derivations d ON d.fact_id = f.fact_id
		WHERE f.repo = $1 AND f.valid_to IS NULL
		  AND d.input_kind = $2 AND d.input_key = $3 AND d.input_hash != $4
	`, repo, inputKind, inputKey, currentHash)
	if err != nil {
		return nil, fmt.Errorf("query stale live facts (%s/%s): %w", inputKind, inputKey, err)
	}
	defer rows.Close()
	return scanFacts(rows)
}

// GetLiveInterfaceImplementerHash returns the currently-recorded
// implementer-set hash for (repo, interfaceID), or ok=false if no live row
// exists yet (the interface has never been seen, or every prior row was
// closed without a replacement — which shouldn't normally happen once an
// interface has at least one interval opened).
func GetLiveInterfaceImplementerHash(ctx context.Context, q queryer, repo, interfaceID string) (hash string, ok bool, err error) {
	err = q.QueryRowContext(ctx, `
		SELECT implementer_set_hash FROM atlas.interface_implementers
		WHERE repo = $1 AND interface_id = $2 AND valid_to IS NULL
	`, repo, interfaceID).Scan(&hash)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("get live interface_implementers hash for %s: %w", interfaceID, err)
	}
	return hash, true, nil
}

// UpsertInterfaceImplementers maintains atlas.interface_implementers as its
// own derived relation (architecture.md section 2.2's "required property:
// ... maintained as its own derived relation, not recomputed by scanning
// the program"). Given the interface's CURRENT full implementer set, it:
//
//  1. Computes newHash = derive.ImplementerSetHash(currentImplementers)
//     (the caller passes newHash in, computed by internal/derive, so this
//     package stays free of any dependency on how the hash is derived).
//  2. If a live row already exists with the same hash, does nothing
//     (changed=false) — this is the common case: most commits don't touch
//     this particular interface's implementer set.
//  3. Otherwise closes any existing live row at seq and opens a new one —
//     changed=true, meaning the caller must now call StaleLiveFacts for
//     (INTERFACE, interfaceID, newHash) to find every fact that needs
//     withdrawal and re-derivation.
//
// Must be called with the *sql.Tx from an in-flight ApplyDelta call.
func UpsertInterfaceImplementers(ctx context.Context, tx *sql.Tx, repo, interfaceID, newHash string, seq int64) (changed bool, err error) {
	oldHash, ok, err := GetLiveInterfaceImplementerHash(ctx, tx, repo, interfaceID)
	if err != nil {
		return false, err
	}
	if ok && oldHash == newHash {
		return false, nil
	}

	if ok {
		if _, err := tx.ExecContext(ctx, `
			UPDATE atlas.interface_implementers SET valid_to = $1
			WHERE repo = $2 AND interface_id = $3 AND valid_to IS NULL
		`, seq, repo, interfaceID); err != nil {
			return false, fmt.Errorf("close interface_implementers for %s: %w", interfaceID, err)
		}
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO atlas.interface_implementers (repo, interface_id, implementer_set_hash, valid_from, valid_to)
		VALUES ($1, $2, $3, $4, NULL)
	`, repo, interfaceID, newHash, seq); err != nil {
		return false, fmt.Errorf("open interface_implementers for %s: %w", interfaceID, err)
	}

	return true, nil
}
