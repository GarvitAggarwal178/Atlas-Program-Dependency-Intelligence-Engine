package store

import (
	"context"
	"fmt"
)

// SchemaV2 extends the base schema with columns and tables needed for
// correct incremental updates.
//
// Changes from base schema:
//
//  1. interface_dispatch_sites gains a method_name column. Without this, the
//     incremental engine can retract stale interface_resolved edges (step 2)
//     but cannot recompute them (step 4) without re-parsing the call site's
//     file — which may not have changed and is therefore not available.
//
//  2. type_ifaces table: persists the set of interfaces each concrete type
//     implements at each commit. This lets the incremental engine compare the
//     old vs new implementer set for a type without requiring a full re-parse
//     of the base commit.
const SchemaV2 = `
-- Add method_name to interface_dispatch_sites if it doesn't exist.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_name = 'interface_dispatch_sites'
          AND column_name = 'method_name'
    ) THEN
        ALTER TABLE interface_dispatch_sites ADD COLUMN method_name TEXT NOT NULL DEFAULT '';
    END IF;
END;
$$;

-- type_ifaces: which interfaces does each concrete type implement, per commit.
-- Populated during full build and updated during incremental updates.
-- Used by the incremental engine to detect when a type's implementer set
-- changes without requiring a re-parse of the base commit.
CREATE TABLE IF NOT EXISTS type_ifaces (
    repo            TEXT NOT NULL,
    commit_hash     TEXT NOT NULL,
    type_name       TEXT NOT NULL,   -- fully-qualified type name
    interface_name  TEXT NOT NULL,   -- fully-qualified interface name
    source_file     TEXT NOT NULL,
    PRIMARY KEY (repo, commit_hash, type_name, interface_name)
);

CREATE INDEX IF NOT EXISTS idx_type_ifaces_iface
    ON type_ifaces (repo, commit_hash, interface_name);

CREATE INDEX IF NOT EXISTS idx_type_ifaces_file
    ON type_ifaces (repo, commit_hash, source_file);
`

// TypeIface is a single row in type_ifaces.
type TypeIface struct {
	Repo          string
	CommitHash    string
	TypeName      string
	InterfaceName string
	SourceFile    string
}

// DispatchSiteV2 is the extended version of DispatchSite with MethodName.
// We keep the old DispatchSite for compatibility and add this separately.
type DispatchSiteV2 struct {
	Repo           string
	CommitHash     string
	InterfaceName  string
	MethodName     string // the specific method being called
	CallSiteSymbol string
	CallSiteFile   string
}

// ApplySchemaV2 applies the V2 schema extensions.
func (s *DB) ApplySchemaV2(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, SchemaV2); err != nil {
		return fmt.Errorf("apply schema v2: %w", err)
	}
	return nil
}

// InsertTypeIfaces bulk-inserts type→interface relationships.
func (s *DB) InsertTypeIfaces(ctx context.Context, rows []TypeIface) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO type_ifaces (repo, commit_hash, type_name, interface_name, source_file)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT DO NOTHING
	`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, r := range rows {
		if _, err := stmt.ExecContext(ctx, r.Repo, r.CommitHash, r.TypeName, r.InterfaceName, r.SourceFile); err != nil {
			return fmt.Errorf("insert type_iface %s→%s: %w", r.TypeName, r.InterfaceName, err)
		}
	}
	return tx.Commit()
}

// InsertDispatchSitesV2 inserts dispatch sites with method names.
func (s *DB) InsertDispatchSitesV2(ctx context.Context, sites []DispatchSiteV2) error {
	if len(sites) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO interface_dispatch_sites
		    (repo, commit_hash, interface_name, method_name, call_site_symbol, call_site_file)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (repo, commit_hash, interface_name, call_site_symbol) DO UPDATE
		    SET method_name = EXCLUDED.method_name
	`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, ds := range sites {
		if _, err := stmt.ExecContext(ctx,
			ds.Repo, ds.CommitHash, ds.InterfaceName, ds.MethodName,
			ds.CallSiteSymbol, ds.CallSiteFile,
		); err != nil {
			return fmt.Errorf("insert dispatch site v2: %w", err)
		}
	}
	return tx.Commit()
}

// LoadDispatchSitesByInterfacesV2 returns extended dispatch sites for the
// given interfaces, including the method_name column.
func (s *DB) LoadDispatchSitesByInterfacesV2(
	ctx context.Context,
	repo, commitHash string,
	interfaceNames []string,
) ([]DispatchSiteV2, error) {
	if len(interfaceNames) == 0 {
		return nil, nil
	}

	args := []interface{}{repo, commitHash}
	placeholders := ""
	for i, n := range interfaceNames {
		if i > 0 {
			placeholders += ", "
		}
		args = append(args, n)
		placeholders += fmt.Sprintf("$%d", i+3)
	}

	query := fmt.Sprintf(`
		SELECT interface_name, method_name, call_site_symbol, call_site_file
		FROM interface_dispatch_sites
		WHERE repo = $1 AND commit_hash = $2
		  AND interface_name IN (%s)
	`, placeholders)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("load dispatch sites v2: %w", err)
	}
	defer rows.Close()

	var result []DispatchSiteV2
	for rows.Next() {
		var ds DispatchSiteV2
		ds.Repo = repo
		ds.CommitHash = commitHash
		if err := rows.Scan(&ds.InterfaceName, &ds.MethodName,
			&ds.CallSiteSymbol, &ds.CallSiteFile); err != nil {
			return nil, fmt.Errorf("scan dispatch site v2: %w", err)
		}
		result = append(result, ds)
	}
	return result, rows.Err()
}

// LoadTypeIfacesForFile returns all type→interface rows for a given file at a
// given commit. Used to compute "old implementer set" before retraction.
func (s *DB) LoadTypeIfacesForFile(ctx context.Context, repo, commitHash, filePath string) ([]TypeIface, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT type_name, interface_name
		FROM type_ifaces
		WHERE repo = $1 AND commit_hash = $2 AND source_file = $3
	`, repo, commitHash, filePath)
	if err != nil {
		return nil, fmt.Errorf("load type_ifaces for %s: %w", filePath, err)
	}
	defer rows.Close()

	var result []TypeIface
	for rows.Next() {
		var ti TypeIface
		ti.Repo = repo
		ti.CommitHash = commitHash
		ti.SourceFile = filePath
		if err := rows.Scan(&ti.TypeName, &ti.InterfaceName); err != nil {
			return nil, fmt.Errorf("scan type_iface: %w", err)
		}
		result = append(result, ti)
	}
	return result, rows.Err()
}

// RetractTypeIfacesForFile removes all type_ifaces rows for a given file.
// Called in step 1 of the incremental update, alongside RetractFile.
func (s *DB) RetractTypeIfacesForFile(ctx context.Context, repo, commitHash, filePath string) error {
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM type_ifaces
		WHERE repo = $1 AND commit_hash = $2 AND source_file = $3
	`, repo, commitHash, filePath)
	return err
}

// CopyTypeIfacesToCommit copies type_ifaces rows from baseCommit to headCommit.
// Called during the graph copy step in incremental.Update.
func (s *DB) CopyTypeIfacesToCommit(ctx context.Context, repo, fromCommit, toCommit string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO type_ifaces (repo, commit_hash, type_name, interface_name, source_file)
		SELECT repo, $1, type_name, interface_name, source_file
		FROM type_ifaces
		WHERE repo = $2 AND commit_hash = $3
		ON CONFLICT DO NOTHING
	`, toCommit, repo, fromCommit)
	return err
}
