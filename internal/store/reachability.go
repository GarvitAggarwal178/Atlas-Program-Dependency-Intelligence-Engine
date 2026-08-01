package store

import (
	"context"
	"database/sql"
	"fmt"
)

// OpenReachable opens a new live interval for symbol being reachable.
// Must be called with the *sql.Tx from an in-flight ApplyDelta call, same
// rule as every other fact/derivation write in this package.
func OpenReachable(ctx context.Context, tx *sql.Tx, repo, symbol string, supportCount int, seq int64) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO atlas.reachable_symbols (repo, symbol, support_count, valid_from, valid_to)
		VALUES ($1, $2, $3, $4, NULL)
	`, repo, symbol, supportCount, seq)
	if err != nil {
		return fmt.Errorf("open reachable %s: %w", symbol, err)
	}
	return nil
}

// CloseReachable closes symbol's live interval, if one exists.
func CloseReachable(ctx context.Context, tx *sql.Tx, repo, symbol string, seq int64) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE atlas.reachable_symbols SET valid_to = $1
		WHERE repo = $2 AND symbol = $3 AND valid_to IS NULL
	`, seq, repo, symbol)
	if err != nil {
		return fmt.Errorf("close reachable %s: %w", symbol, err)
	}
	return nil
}

// QueryLiveReachable returns every currently-reachable symbol for repo.
func QueryLiveReachable(ctx context.Context, q queryer, repo string) ([]ReachableSymbol, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT repo, symbol, support_count, valid_from, valid_to
		FROM atlas.reachable_symbols WHERE repo = $1 AND valid_to IS NULL
	`, repo)
	if err != nil {
		return nil, fmt.Errorf("query live reachable: %w", err)
	}
	defer rows.Close()

	var result []ReachableSymbol
	for rows.Next() {
		var r ReachableSymbol
		if err := rows.Scan(&r.Repo, &r.Symbol, &r.SupportCount, &r.ValidFrom, &r.ValidTo); err != nil {
			return nil, fmt.Errorf("scan reachable symbol: %w", err)
		}
		result = append(result, r)
	}
	return result, rows.Err()
}
