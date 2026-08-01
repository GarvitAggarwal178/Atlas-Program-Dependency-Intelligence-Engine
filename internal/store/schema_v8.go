package store

import (
	"context"
	"database/sql"
	"fmt"
)

// SchemaV8 adds analyzer_state — one row per repo recording the
// AnalyzerVersion last used to index it (architecture.md invariant I13).
// Deliberately separate from repo_state/ApplyDelta's crash-consistency
// watermark: this is advisory metadata (worst case of it being one commit
// stale is a spurious extra full re-apply, not a correctness bug), not
// something that needs to participate in the same transaction as fact
// writes.
const SchemaV8 = `
CREATE TABLE IF NOT EXISTS atlas.analyzer_state (
    repo             TEXT PRIMARY KEY,
    analyzer_version TEXT NOT NULL
);
`

// ApplySchemaV8 creates atlas.analyzer_state.
func (s *DB) ApplySchemaV8(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, SchemaV8); err != nil {
		return fmt.Errorf("apply schema v8: %w", err)
	}
	return nil
}

// GetAnalyzerVersion returns the analyzer version last recorded for repo,
// or ok=false if none has ever been recorded (fresh repo).
func (s *DB) GetAnalyzerVersion(ctx context.Context, repo string) (version string, ok bool, err error) {
	err = s.db.QueryRowContext(ctx, `
		SELECT analyzer_version FROM atlas.analyzer_state WHERE repo = $1
	`, repo).Scan(&version)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("get analyzer version: %w", err)
	}
	return version, true, nil
}

// SetAnalyzerVersion records the analyzer version used to index repo.
func (s *DB) SetAnalyzerVersion(ctx context.Context, repo, version string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO atlas.analyzer_state (repo, analyzer_version)
		VALUES ($1, $2)
		ON CONFLICT (repo) DO UPDATE SET analyzer_version = EXCLUDED.analyzer_version
	`, repo, version)
	if err != nil {
		return fmt.Errorf("set analyzer version: %w", err)
	}
	return nil
}
