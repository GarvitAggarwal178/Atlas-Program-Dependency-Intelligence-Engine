package store

import (
	"context"
	"database/sql"
	"fmt"
)

// queryer is satisfied by both *sql.DB and *sql.Tx. Read paths (QueryFactsAt,
// QueryLiveFacts) accept either, since reads don't need to participate in a
// write transaction. Write paths (OpenFact, CloseFact*) take a *sql.Tx
// explicitly and deliberately do NOT accept a bare *sql.DB — every fact
// write must happen inside the single transaction ApplyDelta owns
// (architecture.md section 3.1), and accepting *sql.DB here would make it
// easy to accidentally write facts outside that transaction, silently
// reopening the crash-consistency hole section 3.1 exists to close.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...interface{}) *sql.Row
}

// OpenFact inserts a new fact with an open interval: valid_from = f.ValidFrom,
// valid_to = NULL. Must be called with the *sql.Tx from an in-flight
// ApplyDelta call. Returns the assigned fact_id.
//
// If a live fact already exists for the same natural key (repo, kind,
// source_symbol, target_symbol, provenance, call_site, source_module —
// exactly the facts_live_uniq index), this is a caller error: the natural
// key's fact must be closed (CloseFactByKey) before a new one can be
// opened for it. That invariant (I2: at most one open interval per logical
// fact) is enforced by the database via facts_live_uniq, not just by
// convention — a violating insert returns a Postgres unique-violation
// error, not silent duplication.
func OpenFact(ctx context.Context, tx *sql.Tx, f Fact) (int64, error) {
	var factID int64
	err := tx.QueryRowContext(ctx, `
		INSERT INTO atlas.facts
		    (repo, kind, source_symbol, target_symbol, provenance, call_site,
		     source_file, source_module, source_module_version, support_count,
		     valid_from, valid_to)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, NULL)
		RETURNING fact_id
	`, f.Repo, f.Kind, f.SourceSymbol, f.TargetSymbol, f.Provenance, f.CallSite,
		f.SourceFile, f.SourceModule, f.SourceModuleVersion, supportCountOrDefault(f.SupportCount),
		f.ValidFrom,
	).Scan(&factID)
	if err != nil {
		return 0, fmt.Errorf("open fact %s->%s: %w", f.SourceSymbol, f.TargetSymbol, err)
	}
	return factID, nil
}

func supportCountOrDefault(n int) int {
	if n <= 0 {
		return 1
	}
	return n
}

// CloseFactByKey closes the currently-open interval for the fact matching
// the given natural key, setting valid_to = seq. Must be called with the
// *sql.Tx from an in-flight ApplyDelta call.
//
// Returns (found=false, nil) if no live fact matches the key — closing a
// fact that isn't open is a no-op, not an error, since callers doing
// retract-then-rederive naturally attempt to close facts that may or may
// not currently be live.
func CloseFactByKey(
	ctx context.Context, tx *sql.Tx,
	repo, kind, sourceSymbol, targetSymbol, provenance, callSite, sourceModule string,
	seq int64,
) (found bool, err error) {
	res, err := tx.ExecContext(ctx, `
		UPDATE atlas.facts SET valid_to = $1
		WHERE repo = $2 AND kind = $3 AND source_symbol = $4 AND target_symbol = $5
		  AND provenance = $6 AND call_site = $7 AND source_module = $8
		  AND valid_to IS NULL
	`, seq, repo, kind, sourceSymbol, targetSymbol, provenance, callSite, sourceModule)
	if err != nil {
		return false, fmt.Errorf("close fact %s->%s: %w", sourceSymbol, targetSymbol, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("close fact %s->%s: rows affected: %w", sourceSymbol, targetSymbol, err)
	}
	return n > 0, nil
}

// CloseFactByID closes the open interval for a specific fact_id, setting
// valid_to = seq. Must be called with the *sql.Tx from an in-flight
// ApplyDelta call.
func CloseFactByID(ctx context.Context, tx *sql.Tx, factID, seq int64) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE atlas.facts SET valid_to = $1 WHERE fact_id = $2 AND valid_to IS NULL
	`, seq, factID)
	if err != nil {
		return fmt.Errorf("close fact_id=%d: %w", factID, err)
	}
	return nil
}

// QueryFactsAt returns every fact live at seq: valid_from <= seq AND
// (valid_to IS NULL OR valid_to > seq) — architecture.md section 5's
// "Query at commit C." q may be a *sql.DB or a *sql.Tx.
func QueryFactsAt(ctx context.Context, q queryer, repo string, seq int64) ([]Fact, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT fact_id, repo, kind, source_symbol, target_symbol, provenance, call_site,
		       source_file, source_module, source_module_version, support_count,
		       valid_from, valid_to
		FROM atlas.facts
		WHERE repo = $1 AND valid_from <= $2 AND (valid_to IS NULL OR valid_to > $2)
	`, repo, seq)
	if err != nil {
		return nil, fmt.Errorf("query facts at seq=%d: %w", seq, err)
	}
	defer rows.Close()
	return scanFacts(rows)
}

// QueryLiveFacts returns every currently-open fact (valid_to IS NULL) for
// repo — the "current state" view, equivalent to QueryFactsAt at the
// latest applied seq but without needing to know what that seq is.
func QueryLiveFacts(ctx context.Context, q queryer, repo string) ([]Fact, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT fact_id, repo, kind, source_symbol, target_symbol, provenance, call_site,
		       source_file, source_module, source_module_version, support_count,
		       valid_from, valid_to
		FROM atlas.facts
		WHERE repo = $1 AND valid_to IS NULL
	`, repo)
	if err != nil {
		return nil, fmt.Errorf("query live facts: %w", err)
	}
	defer rows.Close()
	return scanFacts(rows)
}

func scanFacts(rows *sql.Rows) ([]Fact, error) {
	var result []Fact
	for rows.Next() {
		var f Fact
		if err := rows.Scan(
			&f.FactID, &f.Repo, &f.Kind, &f.SourceSymbol, &f.TargetSymbol, &f.Provenance, &f.CallSite,
			&f.SourceFile, &f.SourceModule, &f.SourceModuleVersion, &f.SupportCount,
			&f.ValidFrom, &f.ValidTo,
		); err != nil {
			return nil, fmt.Errorf("scan fact: %w", err)
		}
		result = append(result, f)
	}
	return result, rows.Err()
}
