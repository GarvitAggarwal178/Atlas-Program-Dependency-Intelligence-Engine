// Package store owns all Postgres interactions for the call graph engine.
//
// Call edges are persisted in a generic "facts" table (kind = 'CALL' today;
// other kinds are reserved for future fact types — see the project blueprint).
// The PRIMARY KEY on (repo, commit_hash, kind, source_symbol, target_symbol)
// means re-running a full build for the same commit is idempotent: ON CONFLICT
// DO NOTHING skips duplicates without error.
package store

import (
	"context"
	"database/sql"
	"fmt"

	// pq registers the "postgres" driver with database/sql.
	_ "github.com/lib/pq"
)

// Schema is the DDL for the tables owned by this package.
// Run once against a fresh database; safe to re-run (IF NOT EXISTS).
const Schema = `
CREATE TABLE IF NOT EXISTS facts (
    repo                    TEXT NOT NULL,
    commit_hash             TEXT NOT NULL,
    kind                    TEXT NOT NULL,   -- only 'CALL' is populated today
    source_symbol           TEXT NOT NULL,
    target_symbol           TEXT NOT NULL,
    provenance              TEXT NOT NULL,   -- 'direct_call' | 'interface_resolved'
    source_file             TEXT NOT NULL,
    source_module           TEXT NOT NULL,   -- the repo's own module path for in-repo facts
    source_module_version   TEXT NOT NULL,   -- "" for the main repo itself
    PRIMARY KEY (repo, commit_hash, kind, source_symbol, target_symbol)
);

CREATE TABLE IF NOT EXISTS symbols (
    repo            TEXT NOT NULL,
    commit_hash     TEXT NOT NULL,
    file_path       TEXT NOT NULL,
    symbol_name     TEXT NOT NULL,   -- fully-qualified
    kind            TEXT NOT NULL,   -- 'function' | 'method' | 'interface' | 'type'
    line_start      INT  NOT NULL,
    line_end        INT  NOT NULL,
    canonical_hash  TEXT NOT NULL,
    PRIMARY KEY (repo, commit_hash, file_path, symbol_name)
);

CREATE TABLE IF NOT EXISTS interface_dispatch_sites (
    repo              TEXT NOT NULL,
    commit_hash       TEXT NOT NULL,
    interface_name    TEXT NOT NULL,
    call_site_symbol  TEXT NOT NULL,
    call_site_file    TEXT NOT NULL,
    PRIMARY KEY (repo, commit_hash, interface_name, call_site_symbol)
);

CREATE INDEX IF NOT EXISTS idx_facts_target
    ON facts (repo, commit_hash, kind, target_symbol);

CREATE INDEX IF NOT EXISTS idx_symbols_file
    ON symbols (repo, commit_hash, file_path);
`

// DB wraps a *sql.DB and provides domain-typed operations.
type DB struct {
	db *sql.DB
	// modulePath is the main repo's own Go module path (from go.mod), used to
	// populate facts.source_module for in-repo facts. Set via SetModulePath.
	// Dependency-module facts (a different source_module/version) are not
	// produced in this version — see project blueprint section on the
	// generalized fact store.
	modulePath string
}

// Open opens a connection to Postgres using the provided DSN and verifies
// connectivity with a ping.
func Open(dsn string) (*DB, error) {
	fmt.Println("Opening postgres with:")
	fmt.Println(dsn)
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("sql.Open: %w", err)
	}
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	return &DB{db: db}, nil
}

// Close releases the underlying connection pool.
func (s *DB) Close() error { return s.db.Close() }

// SetModulePath records the main repo's own Go module path, used to populate
// facts.source_module for facts inserted via InsertEdges. Call this once,
// after resolving the module path (e.g. via internal/modpath), before any
// build or incremental update runs against this DB handle.
func (s *DB) SetModulePath(modulePath string) {
	s.modulePath = modulePath
}

// ApplySchema creates all tables and indexes if they don't exist.
func (s *DB) ApplySchema(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, Schema); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	return nil
}

// Edge is a single row in the facts table (kind = 'CALL').
type Edge struct {
	Repo         string
	CommitHash   string
	SourceSymbol string
	TargetSymbol string
	Provenance   string // "direct_call" | "interface_resolved"
	SourceFile   string
	// SourceModule and SourceModuleVersion identify which module the fact
	// came from. Callers normally leave these empty; InsertEdges defaults
	// SourceModule to the DB's configured module path (the main repo itself)
	// and SourceModuleVersion to "" — dependency-module facts are not
	// produced in this version.
	SourceModule        string
	SourceModuleVersion string
}

// SymbolRow is a single row in the symbols table.
type SymbolRow struct {
	Repo          string
	CommitHash    string
	FilePath      string
	SymbolName    string
	Kind          string
	LineStart     int
	LineEnd       int
	CanonicalHash string
}

// DispatchSite is a single row in interface_dispatch_sites.
type DispatchSite struct {
	Repo           string
	CommitHash     string
	InterfaceName  string
	CallSiteSymbol string
	CallSiteFile   string
}

// InsertEdges bulk-inserts call edges (as facts with kind='CALL') inside a
// single transaction. Duplicate edges (same primary key) are silently skipped.
//
// SourceModule defaults to the DB's configured module path (s.modulePath, the
// main repo itself) and SourceModuleVersion defaults to "" when an edge
// doesn't set them explicitly — every fact produced today comes from the
// repo's own source, not a dependency.
func (s *DB) InsertEdges(ctx context.Context, edges []Edge) error {
	if len(edges) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() // no-op after Commit

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO facts
		    (repo, commit_hash, kind, source_symbol, target_symbol, provenance,
		     source_file, source_module, source_module_version)
		VALUES ($1, $2, 'CALL', $3, $4, $5, $6, $7, $8)
		ON CONFLICT DO NOTHING
	`)
	if err != nil {
		return fmt.Errorf("prepare edge insert: %w", err)
	}
	defer stmt.Close()

	for _, e := range edges {
		mod := e.SourceModule
		if mod == "" {
			mod = s.modulePath
		}
		if _, err := stmt.ExecContext(ctx,
			e.Repo, e.CommitHash, e.SourceSymbol,
			e.TargetSymbol, e.Provenance, e.SourceFile,
			mod, e.SourceModuleVersion,
		); err != nil {
			return fmt.Errorf("insert edge %s->%s: %w", e.SourceSymbol, e.TargetSymbol, err)
		}
	}
	return tx.Commit()
}

// InsertSymbols bulk-inserts symbol rows. Duplicates are skipped.
func (s *DB) InsertSymbols(ctx context.Context, rows []SymbolRow) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO symbols
		    (repo, commit_hash, file_path, symbol_name, kind, line_start, line_end, canonical_hash)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT DO NOTHING
	`)
	if err != nil {
		return fmt.Errorf("prepare symbol insert: %w", err)
	}
	defer stmt.Close()

	for _, r := range rows {
		if _, err := stmt.ExecContext(ctx,
			r.Repo, r.CommitHash, r.FilePath, r.SymbolName,
			r.Kind, r.LineStart, r.LineEnd, r.CanonicalHash,
		); err != nil {
			return fmt.Errorf("insert symbol %s: %w", r.SymbolName, err)
		}
	}
	return tx.Commit()
}

// InsertDispatchSites bulk-inserts interface dispatch site rows. Duplicates skipped.
func (s *DB) InsertDispatchSites(ctx context.Context, sites []DispatchSite) error {
	if len(sites) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO interface_dispatch_sites
		    (repo, commit_hash, interface_name, call_site_symbol, call_site_file)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT DO NOTHING
	`)
	if err != nil {
		return fmt.Errorf("prepare dispatch site insert: %w", err)
	}
	defer stmt.Close()

	for _, ds := range sites {
		if _, err := stmt.ExecContext(ctx,
			ds.Repo, ds.CommitHash, ds.InterfaceName, ds.CallSiteSymbol, ds.CallSiteFile,
		); err != nil {
			return fmt.Errorf("insert dispatch site: %w", err)
		}
	}
	return tx.Commit()
}

// LoadEdges retrieves all CALL facts for a specific (repo, commit_hash) into
// memory. This is the read path used by the reachability engine to load the
// graph.
func (s *DB) LoadEdges(ctx context.Context, repo, commitHash string) ([]Edge, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT source_symbol, target_symbol, provenance, source_file,
		       source_module, source_module_version
		FROM facts
		WHERE repo = $1 AND commit_hash = $2 AND kind = 'CALL'
	`, repo, commitHash)
	if err != nil {
		return nil, fmt.Errorf("query edges: %w", err)
	}
	defer rows.Close()

	var edges []Edge
	for rows.Next() {
		var e Edge
		e.Repo = repo
		e.CommitHash = commitHash
		if err := rows.Scan(&e.SourceSymbol, &e.TargetSymbol, &e.Provenance, &e.SourceFile,
			&e.SourceModule, &e.SourceModuleVersion); err != nil {
			return nil, fmt.Errorf("scan edge: %w", err)
		}
		edges = append(edges, e)
	}
	return edges, rows.Err()
}

// LoadSymbolsForFiles retrieves all symbol rows for the given file paths at a
// specific commit. Used by the differ to map changed lines to changed symbols.
func (s *DB) LoadSymbolsForFiles(ctx context.Context, repo, commitHash string, files []string) ([]SymbolRow, error) {
	if len(files) == 0 {
		return nil, nil
	}
	// Build a parameterised IN clause.
	args := []interface{}{repo, commitHash}
	placeholders := ""
	for i, f := range files {
		if i > 0 {
			placeholders += ", "
		}
		args = append(args, f)
		placeholders += fmt.Sprintf("$%d", i+3)
	}

	query := fmt.Sprintf(`
		SELECT file_path, symbol_name, kind, line_start, line_end, canonical_hash
		FROM symbols
		WHERE repo = $1 AND commit_hash = $2
		  AND file_path IN (%s)
		ORDER BY file_path, line_start
	`, placeholders)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query symbols: %w", err)
	}
	defer rows.Close()

	var result []SymbolRow
	for rows.Next() {
		var r SymbolRow
		r.Repo = repo
		r.CommitHash = commitHash
		if err := rows.Scan(&r.FilePath, &r.SymbolName, &r.Kind,
			&r.LineStart, &r.LineEnd, &r.CanonicalHash); err != nil {
			return nil, fmt.Errorf("scan symbol: %w", err)
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

// DeleteGraphForCommit removes all facts and symbols for a (repo, commit)
// pair. Used when re-running a full build for the same commit.
func (s *DB) DeleteGraphForCommit(ctx context.Context, repo, commitHash string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, tbl := range []string{"facts", "symbols", "interface_dispatch_sites", "type_ifaces"} {
		if _, err := tx.ExecContext(ctx,
			fmt.Sprintf("DELETE FROM %s WHERE repo=$1 AND commit_hash=$2", tbl),
			repo, commitHash,
		); err != nil {
			return fmt.Errorf("delete from %s: %w", tbl, err)
		}
	}
	return tx.Commit()
}

// EdgeCount returns the number of CALL facts stored for a (repo, commit) pair.
// Useful for sanity-checking after a build.
func (s *DB) EdgeCount(ctx context.Context, repo, commitHash string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM facts WHERE repo=$1 AND commit_hash=$2 AND kind='CALL'",
		repo, commitHash,
	).Scan(&n)
	return n, err
}
