package store

import (
	"context"
	"fmt"
)

// SchemaV6 adds reachable_symbols (architecture.md section 4/section 5) —
// the derived relation the DRed maintenance in internal/reach keeps
// current: which symbols are reachable from the entry-point set, as its
// own interval-based table, same shape/pattern as atlas.facts.
const SchemaV6 = `
CREATE TABLE IF NOT EXISTS atlas.reachable_symbols (
    repo TEXT NOT NULL,
    symbol TEXT NOT NULL,
    support_count INT NOT NULL,
    valid_from BIGINT NOT NULL,
    valid_to BIGINT,
    CHECK (valid_to IS NULL OR valid_to > valid_from)
);

CREATE UNIQUE INDEX IF NOT EXISTS reachable_symbols_live_uniq
    ON atlas.reachable_symbols (repo, symbol)
    WHERE valid_to IS NULL;
`

// ReachableSymbol is a single row in atlas.reachable_symbols.
type ReachableSymbol struct {
	Repo         string
	Symbol       string
	SupportCount int
	ValidFrom    int64
	ValidTo      *int64
}

// ApplySchemaV6 creates atlas.reachable_symbols.
func (s *DB) ApplySchemaV6(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, SchemaV6); err != nil {
		return fmt.Errorf("apply schema v6: %w", err)
	}
	return nil
}
