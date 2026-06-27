package store

import (
	"context"
	"fmt"
	"sort"
)

// ──────────────────────────────────────────────────────────────────────────────
// Incremental-engine read operations
// ──────────────────────────────────────────────────────────────────────────────

// LoadAllDispatchSitesForCallers returns all dispatch site rows (across all
// interfaces) for the given call site symbols. Used after retracting all
// interface_resolved edges from a set of callers, to get the complete set
// of dispatch sites that need to be re-expanded in step 4.
func (s *DB) LoadAllDispatchSitesForCallers(
	ctx context.Context,
	repo, commitHash string,
	callSiteSymbols []string,
) ([]DispatchSiteV2, error) {
	if len(callSiteSymbols) == 0 {
		return nil, nil
	}

	args := []interface{}{repo, commitHash}
	placeholders := ""
	for i, sym := range callSiteSymbols {
		if i > 0 {
			placeholders += ", "
		}
		args = append(args, sym)
		placeholders += fmt.Sprintf("$%d", i+3)
	}

	query := fmt.Sprintf(`
		SELECT interface_name, method_name, call_site_symbol, call_site_file
		FROM interface_dispatch_sites
		WHERE repo = $1 AND commit_hash = $2
		  AND call_site_symbol IN (%s)
	`, placeholders)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("load all dispatch sites for callers: %w", err)
	}
	defer rows.Close()

	var result []DispatchSiteV2
	for rows.Next() {
		var ds DispatchSiteV2
		ds.Repo = repo
		ds.CommitHash = commitHash
		if err := rows.Scan(&ds.InterfaceName, &ds.MethodName,
			&ds.CallSiteSymbol, &ds.CallSiteFile); err != nil {
			return nil, fmt.Errorf("scan dispatch site: %w", err)
		}
		result = append(result, ds)
	}
	return result, rows.Err()
}

// LoadDispatchSitesByInterfaces returns all interface_dispatch_sites rows for
// the given repo/commit where the interface_name is in the provided set.
//
// This is the cross-file lookup that makes incremental updates correct: when
// file F changes and one of its types stops/starts implementing interface I,
// we need every call site that dispatches through I — even if those call sites
// live in files that did NOT change.
func (s *DB) LoadDispatchSitesByInterfaces(
	ctx context.Context,
	repo, commitHash string,
	interfaceNames []string,
) ([]DispatchSite, error) {
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
		SELECT interface_name, call_site_symbol, call_site_file
		FROM interface_dispatch_sites
		WHERE repo = $1 AND commit_hash = $2
		  AND interface_name IN (%s)
	`, placeholders)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("load dispatch sites: %w", err)
	}
	defer rows.Close()

	var result []DispatchSite
	for rows.Next() {
		var ds DispatchSite
		ds.Repo = repo
		if err := rows.Scan(&ds.InterfaceName, &ds.CallSiteSymbol, &ds.CallSiteFile); err != nil {
			return nil, fmt.Errorf("scan dispatch site: %w", err)
		}
		result = append(result, ds)
	}
	return result, rows.Err()
}

// LoadAllDispatchSites returns every interface_dispatch_sites row for
// (repo, commitHash). Used when loading a full snapshot for incremental seeding.
func (s *DB) LoadAllDispatchSites(ctx context.Context, repo, commitHash string) ([]DispatchSite, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT interface_name, call_site_symbol, call_site_file
		FROM interface_dispatch_sites
		WHERE repo = $1 AND commit_hash = $2
	`, repo, commitHash)
	if err != nil {
		return nil, fmt.Errorf("load all dispatch sites: %w", err)
	}
	defer rows.Close()

	var result []DispatchSite
	for rows.Next() {
		var ds DispatchSite
		ds.Repo = repo
		if err := rows.Scan(&ds.InterfaceName, &ds.CallSiteSymbol, &ds.CallSiteFile); err != nil {
			return nil, fmt.Errorf("scan dispatch site: %w", err)
		}
		result = append(result, ds)
	}
	return result, rows.Err()
}

// LoadAllSymbols returns every symbol row for (repo, commitHash).
func (s *DB) LoadAllSymbols(ctx context.Context, repo, commitHash string) ([]SymbolRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT file_path, symbol_name, kind, line_start, line_end, canonical_hash
		FROM symbols
		WHERE repo = $1 AND commit_hash = $2
	`, repo, commitHash)
	if err != nil {
		return nil, fmt.Errorf("load all symbols: %w", err)
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

// LoadAllEdges returns every edge for (repo, commitHash).
// Used by the harness to diff full vs incremental graphs.
func (s *DB) LoadAllEdges(ctx context.Context, repo, commitHash string) ([]Edge, error) {
	return s.LoadEdges(ctx, repo, commitHash)
}

// ──────────────────────────────────────────────────────────────────────────────
// Incremental-engine write operations — file-level retraction
// ──────────────────────────────────────────────────────────────────────────────

// RetractFile removes all rows that originated from sourceFile in the given
// (repo, commitHash) graph. This is step 1 of the incremental update rule:
// "retract every edge whose source_file = F."
//
// All three tables are retracted in a single transaction so the graph never
// enters a partially-retracted state.
func (s *DB) RetractFile(ctx context.Context, repo, commitHash, sourceFile string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// 1. Retract all call edges whose source is in this file.
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM call_edges
		WHERE repo = $1 AND commit_hash = $2 AND source_file = $3
	`, repo, commitHash, sourceFile); err != nil {
		return fmt.Errorf("retract call_edges for %s: %w", sourceFile, err)
	}

	// 2. Retract all symbol rows for this file.
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM symbols
		WHERE repo = $1 AND commit_hash = $2 AND file_path = $3
	`, repo, commitHash, sourceFile); err != nil {
		return fmt.Errorf("retract symbols for %s: %w", sourceFile, err)
	}

	// 3. Retract dispatch site entries where the call site is in this file.
	//    (The call site's edges will be re-inserted after re-parsing F.)
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM interface_dispatch_sites
		WHERE repo = $1 AND commit_hash = $2 AND call_site_file = $3
	`, repo, commitHash, sourceFile); err != nil {
		return fmt.Errorf("retract dispatch_sites for %s: %w", sourceFile, err)
	}

	// 4. Retract type→interface relationships for this file.
	//    Must be in the same transaction so the graph never enters a state
	//    where edges are gone but type_ifaces still show old implementors.
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM type_ifaces
		WHERE repo = $1 AND commit_hash = $2 AND source_file = $3
	`, repo, commitHash, sourceFile); err != nil {
		return fmt.Errorf("retract type_ifaces for %s: %w", sourceFile, err)
	}

	return tx.Commit()
}

// RetractInterfaceEdgesForCallSites removes interface_resolved edges for the
// given call sites, scoped to specific target symbols derived from the
// affected interfaces. This prevents over-retraction: a call site that
// dispatches through multiple interfaces should only lose the edges for the
// interfaces whose implementer set actually changed.
//
// sites is the list of dispatch sites (interface + call site symbol pairs)
// whose edges need to be retracted. For each site, we delete edges where
// source_symbol = site.CallSiteSymbol AND target_symbol ends with ".MethodName"
// via the implementor index. Since we don't have the old implementor list
// here, we use a simpler but correct approach: delete all interface_resolved
// edges from the call site symbol that point to targets which were methods
// of types implementing the affected interface.
//
// The simplest correct implementation: for each (callSiteSymbol, interfaceName)
// pair, delete interface_resolved edges from callSiteSymbol whose target
// is a method on any type that implemented interfaceName at the base commit.
// Since we retract BEFORE inserting new data, the targets that currently
// exist in call_edges for this call site are exactly the OLD implementors'
// methods — so we can simply delete all interface_resolved edges from the
// call site symbol that came from files we know were implementing the interface.
//
// Practical implementation: we delete all interface_resolved edges from the
// given call site symbols, but ONLY if those call sites appear in the
// dispatch sites list for the affected interfaces. We rely on step 4
// (recomputeEdges) to reinsert ALL interface_resolved edges for these
// call sites correctly — including edges for unaffected interfaces,
// which are re-expanded from the same newTable.
//
// This means step 4 must reinsert ALL interface_resolved edges for the
// affected call sites, not just the ones for the changed interface.
// See recomputeEdges in engine.go for how this is handled.
func (s *DB) RetractInterfaceEdgesForCallSites(
	ctx context.Context,
	repo, commitHash string,
	callSiteSymbols []string,
) error {
	if len(callSiteSymbols) == 0 {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, `
		DELETE FROM call_edges
		WHERE repo = $1 AND commit_hash = $2
		  AND source_symbol = $3
		  AND provenance = 'interface_resolved'
	`)
	if err != nil {
		return fmt.Errorf("prepare retract stmt: %w", err)
	}
	defer stmt.Close()

	for _, sym := range callSiteSymbols {
		if _, err := stmt.ExecContext(ctx, repo, commitHash, sym); err != nil {
			return fmt.Errorf("retract interface edges for %s: %w", sym, err)
		}
	}

	return tx.Commit()
}

// ──────────────────────────────────────────────────────────────────────────────
// Graph copy — used to seed an incremental update from the previous commit
// ──────────────────────────────────────────────────────────────────────────────

// CopyGraphToCommit copies all rows from (repo, fromCommit) to (repo, toCommit)
// in all three tables, with the commit_hash field updated to toCommit.
// This is how the incremental engine initialises a new commit's graph:
// it starts with a verbatim copy of the previous commit's graph and then
// applies file-level retractions and re-insertions.
//
// Any existing rows for (repo, toCommit) are deleted first so the operation
// is idempotent.
func (s *DB) CopyGraphToCommit(ctx context.Context, repo, fromCommit, toCommit string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// Delete any pre-existing data for toCommit.
	for _, tbl := range []string{"call_edges", "symbols", "interface_dispatch_sites", "type_ifaces"} {
		if _, err := tx.ExecContext(ctx,
			fmt.Sprintf("DELETE FROM %s WHERE repo=$1 AND commit_hash=$2", tbl),
			repo, toCommit,
		); err != nil {
			return fmt.Errorf("clear %s for %s: %w", tbl, toCommit, err)
		}
	}

	// Copy call_edges.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO call_edges
		    (repo, commit_hash, source_symbol, target_symbol, provenance, source_file)
		SELECT repo, $1, source_symbol, target_symbol, provenance, source_file
		FROM call_edges
		WHERE repo = $2 AND commit_hash = $3
		ON CONFLICT DO NOTHING
	`, toCommit, repo, fromCommit); err != nil {
		return fmt.Errorf("copy call_edges: %w", err)
	}

	// Copy symbols.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO symbols
		    (repo, commit_hash, file_path, symbol_name, kind, line_start, line_end, canonical_hash)
		SELECT repo, $1, file_path, symbol_name, kind, line_start, line_end, canonical_hash
		FROM symbols
		WHERE repo = $2 AND commit_hash = $3
		ON CONFLICT DO NOTHING
	`, toCommit, repo, fromCommit); err != nil {
		return fmt.Errorf("copy symbols: %w", err)
	}

	// Copy interface_dispatch_sites including method_name (required for
	// cross-file recomputation in step 4 of the incremental rule).
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO interface_dispatch_sites
		    (repo, commit_hash, interface_name, method_name, call_site_symbol, call_site_file)
		SELECT repo, $1, interface_name, method_name, call_site_symbol, call_site_file
		FROM interface_dispatch_sites
		WHERE repo = $2 AND commit_hash = $3
		ON CONFLICT DO NOTHING
	`, toCommit, repo, fromCommit); err != nil {
		return fmt.Errorf("copy dispatch_sites: %w", err)
	}

	// Copy type_ifaces — required for incremental engine to detect
	// implementer-set changes without re-parsing the base commit.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO type_ifaces
		    (repo, commit_hash, type_name, interface_name, source_file)
		SELECT repo, $1, type_name, interface_name, source_file
		FROM type_ifaces
		WHERE repo = $2 AND commit_hash = $3
		ON CONFLICT DO NOTHING
	`, toCommit, repo, fromCommit); err != nil {
		return fmt.Errorf("copy type_ifaces: %w", err)
	}

	return tx.Commit()
}

// ──────────────────────────────────────────────────────────────────────────────
// Harness helpers
// ──────────────────────────────────────────────────────────────────────────────

// EdgeKey is a canonical string key for an edge, suitable for set operations
// in the differential harness.
func EdgeKey(e Edge) string {
	return e.SourceSymbol + "→" + e.TargetSymbol + ":" + e.Provenance
}

// EdgeSet converts a slice of edges to a map[EdgeKey]Edge for O(1) lookup.
func EdgeSet(edges []Edge) map[string]Edge {
	m := make(map[string]Edge, len(edges))
	for _, e := range edges {
		m[EdgeKey(e)] = e
	}
	return m
}

// DiffEdgeSets returns edges present in a but not b (onlyA) and edges present
// in b but not a (onlyB).
func DiffEdgeSets(a, b map[string]Edge) (onlyA, onlyB []Edge) {
	for k, e := range a {
		if _, ok := b[k]; !ok {
			onlyA = append(onlyA, e)
		}
	}
	for k, e := range b {
		if _, ok := a[k]; !ok {
			onlyB = append(onlyB, e)
		}
	}
	sort.Slice(onlyA, func(i, j int) bool { return EdgeKey(onlyA[i]) < EdgeKey(onlyA[j]) })
	sort.Slice(onlyB, func(i, j int) bool { return EdgeKey(onlyB[i]) < EdgeKey(onlyB[j]) })
	return
}
