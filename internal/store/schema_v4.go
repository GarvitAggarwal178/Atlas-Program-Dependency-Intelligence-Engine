package store

import (
	"context"
	"fmt"
)

// SchemaV4 adds the interval-based facts table — architecture.md section
// 2.1 and section 5, verbatim in shape (adapted only to live in the atlas
// schema, per the collision note in schema_v3.go). This replaces the
// per-commit-snapshot model (v2's own "facts" table, in the public schema,
// PRIMARY KEY including commit_hash) with one row per fact LIFETIME:
// [valid_from, valid_to) over the linearized seq from atlas.commits.
const SchemaV4 = `
CREATE TABLE IF NOT EXISTS atlas.facts (
    fact_id               BIGSERIAL PRIMARY KEY,
    repo                  TEXT NOT NULL,
    kind                  TEXT NOT NULL,   -- 'CALL' today; 'IMPLEMENTS', 'IMPORTS' next (section 8)
    source_symbol         TEXT NOT NULL,
    target_symbol         TEXT NOT NULL,
    provenance            TEXT NOT NULL,   -- 'direct_call' | 'interface_resolved'
    call_site             TEXT NOT NULL,   -- disambiguates multiple sites — see facts_live_uniq note
    source_file           TEXT NOT NULL,
    source_module         TEXT NOT NULL,
    source_module_version TEXT NOT NULL,
    support_count         INT  NOT NULL DEFAULT 1,
    valid_from            BIGINT NOT NULL,
    valid_to              BIGINT,
    CHECK (valid_to IS NULL OR valid_to > valid_from)
);

-- At most one open interval per logical fact. call_site and provenance are
-- IN the key: omitting them collapses a fact derivable from two distinct
-- call sites into one row (architecture.md section 5).
CREATE UNIQUE INDEX IF NOT EXISTS facts_live_uniq
    ON atlas.facts (repo, kind, source_symbol, target_symbol, provenance, call_site, source_module)
    WHERE valid_to IS NULL;

CREATE INDEX IF NOT EXISTS facts_interval ON atlas.facts (repo, kind, valid_from, valid_to);
`

// Fact is a single row in atlas.facts: one derived fact's validity
// interval. ValidTo is nil for a currently-live fact (open interval).
type Fact struct {
	FactID              int64
	Repo                string
	Kind                string // 'CALL' today
	SourceSymbol        string
	TargetSymbol        string
	Provenance          string // 'direct_call' | 'interface_resolved'
	CallSite            string
	SourceFile          string
	SourceModule        string
	SourceModuleVersion string
	SupportCount        int
	ValidFrom           int64
	ValidTo             *int64 // nil == still live
}

const (
	FactKindCall = "CALL"

	// FactKindImplements is the new fact kind added by the section 8
	// IMPLEMENTS probe (internal/store/implements_probe_test.go): a
	// concrete type implements an interface. Its provenance is
	// ProvenanceImplements; it carries no call_site (empty string is used,
	// which is fine for facts_live_uniq — the pair (source_symbol,
	// target_symbol) is already unique per this kind+provenance since
	// call_site and provenance are both constant across all IMPLEMENTS facts).
	FactKindImplements = "IMPLEMENTS"

	// ProvenanceImplements is the provenance value for IMPLEMENTS facts.
	// Unlike CALL facts (direct_call | interface_resolved), an IMPLEMENTS
	// fact is never an over-approximation — go/types.Implements is exact —
	// so there is only one provenance value for this kind.
	ProvenanceImplements = "implements"
)

// ApplySchemaV4 creates the atlas.facts table and its indexes. Requires
// ApplySchemaV3 to have run first (atlas schema must exist).
func (s *DB) ApplySchemaV4(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, SchemaV4); err != nil {
		return fmt.Errorf("apply schema v4: %w", err)
	}
	return nil
}
