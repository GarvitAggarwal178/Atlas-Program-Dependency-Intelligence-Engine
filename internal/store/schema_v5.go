package store

import (
	"context"
	"fmt"
)

// SchemaV5 adds derivation tracking (architecture.md section 2.2) —
// derivations records what each fact was derived from, and
// interface_implementers is the maintained, interval-based relation whose
// hash (computed by internal/derive.ImplementerSetHash — the implementer
// SET, not the method set; see that package's doc comment for the full
// soundness argument) drives invalidation for interface-dispatched facts.
const SchemaV5 = `
-- What each fact was derived from (section 2.2). Drives both withdrawal
-- and backward validation (section 2.3, not yet built).
CREATE TABLE IF NOT EXISTS atlas.derivations (
    fact_id    BIGINT NOT NULL REFERENCES atlas.facts(fact_id) ON DELETE CASCADE,
    input_kind TEXT NOT NULL,   -- 'FILE' | 'TYPE' | 'INTERFACE' | 'MODULE'
    input_key  TEXT NOT NULL,
    input_hash TEXT NOT NULL,   -- FILE: structural hash
                                 -- TYPE: method-set hash
                                 -- INTERFACE: IMPLEMENTER-SET hash (section 2.2 — not method set)
                                 -- MODULE: version string
    PRIMARY KEY (fact_id, input_kind, input_key)
);

CREATE INDEX IF NOT EXISTS derivations_reverse ON atlas.derivations (input_kind, input_key, input_hash);

-- Maintained implementer sets: a first-class derived relation, not
-- recomputed by scanning the whole program on every query.
CREATE TABLE IF NOT EXISTS atlas.interface_implementers (
    repo TEXT NOT NULL,
    interface_id TEXT NOT NULL,
    implementer_set_hash TEXT NOT NULL,
    valid_from BIGINT NOT NULL,
    valid_to BIGINT,
    CHECK (valid_to IS NULL OR valid_to > valid_from)
);

CREATE UNIQUE INDEX IF NOT EXISTS interface_implementers_live_uniq
    ON atlas.interface_implementers (repo, interface_id)
    WHERE valid_to IS NULL;
`

// Derivation is a single row in atlas.derivations: one recorded input a
// fact was derived from.
type Derivation struct {
	FactID    int64
	InputKind string // 'FILE' | 'TYPE' | 'INTERFACE' | 'MODULE'
	InputKey  string
	InputHash string
}

const (
	InputKindFile      = "FILE"
	InputKindType      = "TYPE"
	InputKindInterface = "INTERFACE"
	InputKindModule    = "MODULE"
)

// InterfaceImplementers is a single row in atlas.interface_implementers.
type InterfaceImplementers struct {
	Repo                string
	InterfaceID         string
	ImplementerSetHash  string
	ValidFrom           int64
	ValidTo             *int64
}

// ApplySchemaV5 creates atlas.derivations and atlas.interface_implementers.
// Requires ApplySchemaV3 (atlas schema) and ApplySchemaV4 (atlas.facts,
// which atlas.derivations references) to have run first.
func (s *DB) ApplySchemaV5(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, SchemaV5); err != nil {
		return fmt.Errorf("apply schema v5: %w", err)
	}
	return nil
}
