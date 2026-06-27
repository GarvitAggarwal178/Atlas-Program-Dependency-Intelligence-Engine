// Package incremental implements the incremental call graph update engine.
//
// # The correctness problem that naive implementations get wrong
//
// File A contains:  proc.ledger.Debit()   ← call through Ledger interface
// File B contains:  type SQLLedger struct  ← implements Ledger
//
// Naive incremental: file B changes → retract B's edges, reinsert B's edges.
// Result: A's interface_resolved edges still point to the OLD SQLLedger methods.
// If B's change is "SQLLedger now implements Ledger differently" or "a new type
// CacheLedger in B now implements Ledger", A's edges are WRONG and we never
// touched A.
//
// The fix requires the cross-file step: when B's type implementer set changes,
// look up every call site dispatching through Ledger (in ANY file), retract
// their interface_resolved edges, and recompute with the updated implementer set.
//
// # The exact 4-step rule
//
// When file F changes from commit A to commit B:
//
//  1. Retract: DELETE FROM call_edges WHERE source_file = F.
//              DELETE FROM symbols WHERE file_path = F.
//              DELETE FROM interface_dispatch_sites WHERE call_site_file = F.
//              DELETE FROM type_ifaces WHERE source_file = F.
//
//  2. Cross-file recomputation:
//     Load type_ifaces for F at the base commit (before retraction — we loaded
//     it before step 1). Compare with F's NEW types from newTable.
//     For each interface I whose implementer set changed, load ALL dispatch
//     sites for I from interface_dispatch_sites (any file). Retract their
//     interface_resolved edges.
//
//  3. Reparse F (at headCommit) and insert new edges, symbols, type_ifaces,
//     and dispatch_sites.
//
//  4. Recompute and reinsert interface_resolved edges for call sites from
//     step 2, using the new implementer set.
package incremental

import (
	"context"
	"fmt"
	"strings"

	"github.com/yourorg/symex/internal/callgraph"
	"github.com/yourorg/symex/internal/parser"
	"github.com/yourorg/symex/internal/store"
	"github.com/yourorg/symex/internal/symboltable"
)

// UpdateResult holds statistics from one incremental update.
type UpdateResult struct {
	FilesChanged        int
	CrossFileSitesFixed int // dispatch sites in unchanged files that needed update
	InterfacesAffected  int
	NewEdgesInserted    int
	NewSymbolsInserted  int
}

// Update applies the incremental update from baseCommit to headCommit.
//
// Precondition: the graph for (repo, baseCommit) must exist in Postgres.
// changedFiles must be repo-relative paths (same format as source_file in DB).
func Update(
	ctx context.Context,
	db *store.DB,
	repoRoot, modulePath string,
	repo, baseCommit, headCommit string,
	changedFiles []string,
) (*UpdateResult, error) {
	result := &UpdateResult{FilesChanged: len(changedFiles)}

	// ── Initialise: copy base graph to head ──────────────────────────────────
	// CopyGraphToCommit copies call_edges, symbols, interface_dispatch_sites,
	// AND type_ifaces in a single transaction.
	if err := db.CopyGraphToCommit(ctx, repo, baseCommit, headCommit); err != nil {
		return nil, fmt.Errorf("copy graph: %w", err)
	}

	// ── Parse head commit to get new symbol table ─────────────────────────────
	// We parse the full repo at headCommit. The implementor index is global
	// (cross-file), so we need the full picture to expand interface calls.
	newTable, err := parser.ParseRepo(repoRoot, modulePath)
	if err != nil {
		return nil, fmt.Errorf("parse repo at head: %w", err)
	}

	// ── Process each changed file ─────────────────────────────────────────────
	for _, changedFile := range changedFiles {
		if !strings.HasSuffix(changedFile, ".go") {
			continue
		}

		n, crossFixed, err := updateOneFile(ctx, db, repo, headCommit, changedFile, newTable)
		if err != nil {
			fmt.Printf("  warn: %s: %v\n", changedFile, err)
			continue
		}
		result.NewEdgesInserted += n
		result.CrossFileSitesFixed += crossFixed
	}

	return result, nil
}

// updateOneFile applies the 4-step rule for a single changed file.
func updateOneFile(
	ctx context.Context,
	db *store.DB,
	repo, headCommit, changedFile string,
	newTable *symboltable.RepoSymbolTable,
) (newEdges, crossFixed int, err error) {

	// ── Step 2 prep: load OLD type→interface data BEFORE retraction ──────────
	// At this point headCommit's graph is a verbatim copy of baseCommit
	// (CopyGraphToCommit was called before updateOneFile). So reading
	// headCommit here gives us the BASE state of type_ifaces for this file —
	// the "before" picture we need to diff against the new symbol table.
	// We MUST do this before Step 1 retracts the file's type_ifaces rows.
	oldTypeIfaces, err := db.LoadTypeIfacesForFile(ctx, repo, headCommit, changedFile)
	if err != nil {
		return 0, 0, fmt.Errorf("load old type_ifaces: %w", err)
	}
	// Build old: typeName → set of interface names
	oldIfaceSet := make(map[string]map[string]bool)
	for _, ti := range oldTypeIfaces {
		if oldIfaceSet[ti.TypeName] == nil {
			oldIfaceSet[ti.TypeName] = make(map[string]bool)
		}
		oldIfaceSet[ti.TypeName][ti.InterfaceName] = true
	}

	// ── Step 1: Retract everything from this file ─────────────────────────────
	// RetractFile deletes call_edges, symbols, interface_dispatch_sites, and
	// type_ifaces for this file in a single transaction.
	if err := db.RetractFile(ctx, repo, headCommit, changedFile); err != nil {
		return 0, 0, fmt.Errorf("retract file: %w", err)
	}

	// ── Step 2: Find affected interfaces ─────────────────────────────────────
	// Compare old type→interface map with new (from newTable).
	newIfaceSet := extractNewIfaceSet(changedFile, newTable)
	affectedIfaces := diffIfaceSets(oldIfaceSet, newIfaceSet)

	var crossFileSites []store.DispatchSiteV2
	if len(affectedIfaces) > 0 {
		// Load dispatch sites for affected interfaces — cross-file lookup.
		sites, err := db.LoadDispatchSitesByInterfacesV2(ctx, repo, headCommit, affectedIfaces)
		if err != nil {
			return 0, 0, fmt.Errorf("load cross-file dispatch sites: %w", err)
		}

		// Collect unique call site symbols.
		var syms []string
		seen := make(map[string]bool)
		for _, ds := range sites {
			if !seen[ds.CallSiteSymbol] {
				syms = append(syms, ds.CallSiteSymbol)
				seen[ds.CallSiteSymbol] = true
			}
		}

		// Retract ALL interface_resolved edges for these call site symbols.
		// IMPORTANT: we retract all, not just for the affected interface, because
		// RetractInterfaceEdgesForCallSites deletes by caller symbol + provenance.
		// Step 4 will reload ALL dispatch sites for these symbols and reinsert
		// the complete correct set (for all interfaces they dispatch through).
		if err := db.RetractInterfaceEdgesForCallSites(ctx, repo, headCommit, syms); err != nil {
			return 0, 0, fmt.Errorf("retract cross-file interface edges: %w", err)
		}

		// Load ALL dispatch sites for the affected call site symbols, not just
		// those for the changed interface. This is necessary because we retracted
		// ALL interface_resolved edges from these callers above, so we must
		// reinsert edges for ALL interfaces they dispatch through.
		allSitesForCallers, err := db.LoadAllDispatchSitesForCallers(ctx, repo, headCommit, syms)
		if err != nil {
			return 0, 0, fmt.Errorf("load all dispatch sites for callers: %w", err)
		}
		crossFileSites = allSitesForCallers
		crossFixed = len(sites) // report the number of directly-affected sites
	}

	// ── Step 3: Insert new data for changedFile ───────────────────────────────
	edges, symRows, dispatchSitesV2, typeIfaceRows := extractFileDataV2(
		changedFile, repo, headCommit, newTable,
	)

	if err := db.InsertSymbols(ctx, symRows); err != nil {
		return 0, 0, fmt.Errorf("insert symbols: %w", err)
	}
	if err := db.InsertEdges(ctx, edges); err != nil {
		return 0, 0, fmt.Errorf("insert edges: %w", err)
	}
	if err := db.InsertDispatchSitesV2(ctx, dispatchSitesV2); err != nil {
		return 0, 0, fmt.Errorf("insert dispatch sites: %w", err)
	}
	if err := db.InsertTypeIfaces(ctx, typeIfaceRows); err != nil {
		return 0, 0, fmt.Errorf("insert type_ifaces: %w", err)
	}
	newEdges += len(edges)

	// ── Step 4: Recompute cross-file interface_resolved edges ─────────────────
	// For each cross-file call site identified in step 2, we now have the
	// updated implementer set (newTable reflects headCommit's state).
	// Re-expand and re-insert.
	recomputed := recomputeEdges(repo, headCommit, crossFileSites, newTable)
	if err := db.InsertEdges(ctx, recomputed); err != nil {
		return 0, 0, fmt.Errorf("insert recomputed cross-file edges: %w", err)
	}
	newEdges += len(recomputed)

	return newEdges, crossFixed, nil
}

// extractNewIfaceSet returns the new type→interfaces map for types defined in
// changedFile according to newTable.
func extractNewIfaceSet(
	changedFile string,
	table *symboltable.RepoSymbolTable,
) map[string]map[string]bool {
	result := make(map[string]map[string]bool)
	for _, file := range table.Files {
		if file.FilePath != changedFile {
			continue
		}
		for _, sym := range file.Defined {
			if sym.Kind != symboltable.KindType {
				continue
			}
			ifaces := make(map[string]bool)
			for _, iface := range sym.ImplementedInterfaces {
				ifaces[iface] = true
			}
			result[sym.QualifiedName] = ifaces
		}
	}
	return result
}

// diffIfaceSets returns interface names whose implementer sets changed.
func diffIfaceSets(
	old map[string]map[string]bool, // typeName → ifaceSet (old)
	new map[string]map[string]bool, // typeName → ifaceSet (new)
) []string {
	affected := make(map[string]bool)

	// Types removed or with changed interface sets.
	for typeName, oldIfaces := range old {
		newIfaces, exists := new[typeName]
		if !exists {
			// Type was deleted — all its interfaces lost an implementor.
			for iface := range oldIfaces {
				affected[iface] = true
			}
			continue
		}
		// Interfaces lost.
		for iface := range oldIfaces {
			if !newIfaces[iface] {
				affected[iface] = true
			}
		}
		// Interfaces gained.
		for iface := range newIfaces {
			if !oldIfaces[iface] {
				affected[iface] = true
			}
		}
	}

	// Types newly added.
	for typeName, newIfaces := range new {
		if _, existed := old[typeName]; !existed {
			for iface := range newIfaces {
				affected[iface] = true
			}
		}
	}

	result := make([]string, 0, len(affected))
	for iface := range affected {
		result = append(result, iface)
	}
	return result
}

// extractFileDataV2 extracts all insertable data from changedFile in newTable,
// returning edges, symbol rows, v2 dispatch sites, and type_iface rows.
func extractFileDataV2(
	changedFile, repo, commitHash string,
	table *symboltable.RepoSymbolTable,
) (
	edges []store.Edge,
	symRows []store.SymbolRow,
	dispatchSites []store.DispatchSiteV2,
	typeIfaces []store.TypeIface,
) {
	for _, file := range table.Files {
		if file.FilePath != changedFile {
			continue
		}

		for _, sym := range file.Defined {
			symRows = append(symRows, store.SymbolRow{
				Repo:          repo,
				CommitHash:    commitHash,
				FilePath:      file.FilePath,
				SymbolName:    sym.QualifiedName,
				Kind:          string(sym.Kind),
				LineStart:     sym.Lines.Start,
				LineEnd:       sym.Lines.End,
				CanonicalHash: sym.CanonicalHash,
			})

			// Record type→interface relationships for the incremental engine.
			if sym.Kind == symboltable.KindType {
				for _, iface := range sym.ImplementedInterfaces {
					typeIfaces = append(typeIfaces, store.TypeIface{
						Repo:          repo,
						CommitHash:    commitHash,
						TypeName:      sym.QualifiedName,
						InterfaceName: iface,
						SourceFile:    file.FilePath,
					})
				}
			}
		}

		for _, ref := range file.References {
			switch ref.Kind {
			case symboltable.CallDirect:
				edges = append(edges, store.Edge{
					Repo:         repo,
					CommitHash:   commitHash,
					SourceSymbol: ref.Caller,
					TargetSymbol: ref.Callee,
					Provenance:   "direct_call",
					SourceFile:   file.FilePath,
				})

			case symboltable.CallInterfaceResolved:
				ifaceName, methodName, err := parseInterfaceCallee(ref.Callee)
				if err != nil {
					edges = append(edges, store.Edge{
						Repo:         repo,
						CommitHash:   commitHash,
						SourceSymbol: ref.Caller,
						TargetSymbol: ref.Callee,
						Provenance:   "interface_resolved",
						SourceFile:   file.FilePath,
					})
					continue
				}

				// V2 dispatch site records the method name too.
				dispatchSites = append(dispatchSites, store.DispatchSiteV2{
					Repo:           repo,
					CommitHash:     commitHash,
					InterfaceName:  ifaceName,
					MethodName:     methodName,
					CallSiteSymbol: ref.Caller,
					CallSiteFile:   file.FilePath,
				})

				// Expand to concrete targets using the new full symbol table.
				targets := callgraph.ExpandInterfaceCall(table, ifaceName, methodName)
				if len(targets) == 0 {
					edges = append(edges, store.Edge{
						Repo:         repo,
						CommitHash:   commitHash,
						SourceSymbol: ref.Caller,
						TargetSymbol: ref.Callee,
						Provenance:   "interface_resolved",
						SourceFile:   file.FilePath,
					})
				} else {
					for _, target := range targets {
						edges = append(edges, store.Edge{
							Repo:         repo,
							CommitHash:   commitHash,
							SourceSymbol: ref.Caller,
							TargetSymbol: target,
							Provenance:   "interface_resolved",
							SourceFile:   file.FilePath,
						})
					}
				}
			}
		}
		break
	}
	return
}

// recomputeEdges reinserts interface_resolved edges for cross-file call sites
// using the updated implementer set from newTable.
func recomputeEdges(
	repo, commitHash string,
	sites []store.DispatchSiteV2,
	newTable *symboltable.RepoSymbolTable,
) []store.Edge {
	var edges []store.Edge
	for _, ds := range sites {
		targets := callgraph.ExpandInterfaceCall(newTable, ds.InterfaceName, ds.MethodName)
		if len(targets) == 0 {
			// No implementations found — keep a raw edge so information isn't lost.
			edges = append(edges, store.Edge{
				Repo:         repo,
				CommitHash:   commitHash,
				SourceSymbol: ds.CallSiteSymbol,
				TargetSymbol: ds.InterfaceName + "." + ds.MethodName,
				Provenance:   "interface_resolved",
				SourceFile:   ds.CallSiteFile,
			})
		} else {
			for _, target := range targets {
				edges = append(edges, store.Edge{
					Repo:         repo,
					CommitHash:   commitHash,
					SourceSymbol: ds.CallSiteSymbol,
					TargetSymbol: target,
					Provenance:   "interface_resolved",
					SourceFile:   ds.CallSiteFile,
				})
			}
		}
	}
	return edges
}

func parseInterfaceCallee(callee string) (ifaceName, methodName string, err error) {
	lastDot := strings.LastIndex(callee, ".")
	if lastDot < 0 {
		return "", "", fmt.Errorf("no dot in callee %q", callee)
	}
	return callee[:lastDot], callee[lastDot+1:], nil
}
