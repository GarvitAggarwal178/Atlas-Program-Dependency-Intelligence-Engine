// Package callgraph builds a directed call graph from a RepoSymbolTable and
// persists it to Postgres.
//
// # Edge construction
//
// Stage 1 (the AST parser) already classified every call reference as either
// direct_call or interface_resolved. This package does NOT re-derive that
// classification — it reads it directly from the CallReference.Kind field.
//
// For direct_call references: one edge is added, source→target.
//
// For interface_resolved references: the stage-1 callee is an interface method
// descriptor like "github.com/example/fixture/billing.Ledger.Debit". This
// package expands that single reference into N edges — one for each concrete
// type in the repo that implements the interface — all tagged interface_resolved.
//
// # Why over-approximation is correct here
//
// We cannot determine at static-analysis time which concrete implementation
// will be dispatched to at runtime without a full points-to analysis. Rather
// than miss real call edges (unsound), we include all possible targets
// (over-approximate). The provenance tag `interface_resolved` propagates through
// the reachability engine so that any test selected via such a path is reported
// in the "consider running" tier, not silently merged into "must run".
//
// This is a documented design decision, not a gap.
package callgraph

import (
	"context"
	"fmt"
	"strings"

	"github.com/yourorg/symex/internal/store"
	"github.com/yourorg/symex/internal/symboltable"
)

// BuildResult holds summary statistics from a build run.
type BuildResult struct {
	TotalEdges            int
	DirectEdges           int
	InterfaceResolvedEdges int
	SymbolsIndexed        int
	DispatchSites         int
}

// Build constructs the complete call graph for the given symbol table and
// persists it to Postgres. It performs a full rebuild: all existing edges for
// (repo, commitHash) are deleted first, so re-running is safe.
//
// repo is a stable identifier for the repository (e.g. its remote URL or a
// local path). commitHash is the git commit SHA at which the analysis was run.
func Build(
	ctx context.Context,
	db *store.DB,
	table *symboltable.RepoSymbolTable,
	repo, commitHash string,
) (*BuildResult, error) {

	// Step 1: Build the lookup structures we need for interface expansion.
	//
	// implementors maps "InterfaceName" → []*symboltable.DefinedSymbol for each
	// concrete type that implements it.
	//
	// methodIndex maps "pkg/path.(TypeName).MethodName" → fully-qualified symbol
	// for quick lookup when expanding interface calls.
	implementors, methodIndex := buildImplementorIndex(table)

	// Step 2: Collect all edges, symbol rows, and dispatch sites.
	var edges []store.Edge
	var symRows []store.SymbolRow
	var dispatchSites []store.DispatchSite

	result := &BuildResult{}

	for _, file := range table.Files {
		// Index every defined symbol in this file.
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
		}

		// Convert each call reference into one or more graph edges.
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
				result.DirectEdges++

			case symboltable.CallInterfaceResolved:
				// ref.Callee is in the form "pkg/path.InterfaceName.MethodName"
				// or "pkg/path.InterfaceName.MethodName" depending on how
				// go/types serialized the interface type.
				//
				// Parse it to extract the interface qualified name and method name.
				ifaceName, methodName, err := parseInterfaceCallee(ref.Callee)
				if err != nil {
					// Can't expand — record as-is so we don't silently drop it.
					edges = append(edges, store.Edge{
						Repo:         repo,
						CommitHash:   commitHash,
						SourceSymbol: ref.Caller,
						TargetSymbol: ref.Callee,
						Provenance:   "interface_resolved",
						SourceFile:   file.FilePath,
					})
					result.InterfaceResolvedEdges++
					continue
				}

				// Record the dispatch site for the reverse index (used by the
				// incremental engine in stage 3).
				dispatchSites = append(dispatchSites, store.DispatchSite{
					Repo:           repo,
					CommitHash:     commitHash,
					InterfaceName:  ifaceName,
					CallSiteSymbol: ref.Caller,
					CallSiteFile:   file.FilePath,
				})

				// Expand: add one edge per concrete implementation.
				concreteTargets := expandInterfaceCall(ifaceName, methodName, implementors, methodIndex)
				if len(concreteTargets) == 0 {
					// No known implementations — still record the raw edge so
					// the graph isn't missing information.
					edges = append(edges, store.Edge{
						Repo:         repo,
						CommitHash:   commitHash,
						SourceSymbol: ref.Caller,
						TargetSymbol: ref.Callee, // raw interface method descriptor
						Provenance:   "interface_resolved",
						SourceFile:   file.FilePath,
					})
					result.InterfaceResolvedEdges++
				} else {
					for _, target := range concreteTargets {
						edges = append(edges, store.Edge{
							Repo:         repo,
							CommitHash:   commitHash,
							SourceSymbol: ref.Caller,
							TargetSymbol: target,
							Provenance:   "interface_resolved",
							SourceFile:   file.FilePath,
						})
						result.InterfaceResolvedEdges++
					}
				}
			}
		}
	}

	// Step 3: Delete any previous graph for this (repo, commit) and persist.
	if err := db.DeleteGraphForCommit(ctx, repo, commitHash); err != nil {
		return nil, fmt.Errorf("delete old graph: %w", err)
	}
	if err := db.InsertSymbols(ctx, symRows); err != nil {
		return nil, fmt.Errorf("insert symbols: %w", err)
	}
	if err := db.InsertEdges(ctx, edges); err != nil {
		return nil, fmt.Errorf("insert edges: %w", err)
	}
	if err := db.InsertDispatchSites(ctx, dispatchSites); err != nil {
		return nil, fmt.Errorf("insert dispatch sites: %w", err)
	}

	result.TotalEdges = len(edges)
	result.SymbolsIndexed = len(symRows)
	result.DispatchSites = len(dispatchSites)
	return result, nil
}

// buildImplementorIndex builds two lookup structures from the full repo symbol
// table:
//
//  1. implementors: maps fully-qualified interface name → slice of concrete
//     DefinedSymbol entries (the type, not its methods) that implement it.
//
//  2. methodIndex: maps "pkg/path.(TypeName).MethodName" → the fully-qualified
//     method symbol name. This is used to quickly find what to point the
//     expanded edge at.
func buildImplementorIndex(
	table *symboltable.RepoSymbolTable,
) (
	implementors map[string][]*symboltable.DefinedSymbol,
	methodIndex map[string]string,
) {
	implementors = make(map[string][]*symboltable.DefinedSymbol)
	methodIndex = make(map[string]string)

	for fi := range table.Files {
		file := &table.Files[fi]
		for si := range file.Defined {
			sym := &file.Defined[si]
			switch sym.Kind {
			case symboltable.KindType:
				// Record that this concrete type implements each listed interface.
				for _, iface := range sym.ImplementedInterfaces {
					implementors[iface] = append(implementors[iface], sym)
				}
			case symboltable.KindMethod:
				// e.g. "pkg/path.(SQLLedger).Debit"
				methodIndex[sym.QualifiedName] = sym.QualifiedName
			}
		}
	}
	return implementors, methodIndex
}

// expandInterfaceCall returns the list of fully-qualified method symbols that
// should receive an edge when a call to `ifaceName.methodName` is encountered.
//
// For each concrete type that implements ifaceName, we look for a method with
// the same name in the methodIndex. The qualified method name is constructed as
// "pkg/path.(TypeName).MethodName" which is the same format Stage 1 uses.
func expandInterfaceCall(
	ifaceName, methodName string,
	implementors map[string][]*symboltable.DefinedSymbol,
	methodIndex map[string]string,
) []string {
	types, ok := implementors[ifaceName]
	if !ok {
		return nil
	}

	var targets []string
	for _, concreteType := range types {
		// Construct the expected qualified method name:
		// "importPath.(TypeName).MethodName"
		candidate := fmt.Sprintf("%s.(%s).%s",
			extractPackagePath(concreteType.QualifiedName),
			concreteType.Name,
			methodName,
		)
		if _, exists := methodIndex[candidate]; exists {
			targets = append(targets, candidate)
		}
	}
	return targets
}

// parseInterfaceCallee splits a callee string of the form
// "pkg/path.InterfaceName.MethodName" or
// "pkg/path.InterfaceName.MethodName" (go/types uses the full package path)
// into (interfaceQualifiedName, methodName).
//
// The callee from Stage 1 is produced by:
//   ifaceName := recvType.String()  // e.g. "github.com/example/fixture/billing.Ledger"
//   methodName := fn.Sel.Name       // e.g. "Debit"
//   callee = ifaceName + "." + methodName
//
// So the last dot-separated segment is the method name; everything before it
// is the fully-qualified interface name (which itself contains dots in the
// package path).
func parseInterfaceCallee(callee string) (ifaceName, methodName string, err error) {
	// The method name is the last segment after the final ".".
	lastDot := strings.LastIndex(callee, ".")
	if lastDot < 0 {
		return "", "", fmt.Errorf("cannot parse interface callee %q: no dot found", callee)
	}
	ifaceName = callee[:lastDot]
	methodName = callee[lastDot+1:]
	if ifaceName == "" || methodName == "" {
		return "", "", fmt.Errorf("cannot parse interface callee %q: empty segment", callee)
	}
	return ifaceName, methodName, nil
}

// extractPackagePath returns the import path portion of a qualified symbol
// name. For "pkg/path.SymbolName" it returns "pkg/path".
func extractPackagePath(qualifiedName string) string {
	lastDot := strings.LastIndex(qualifiedName, ".")
	if lastDot < 0 {
		return qualifiedName
	}
	return qualifiedName[:lastDot]
}

// InMemoryGraph is the adjacency list representation of the call graph loaded
// into memory for reachability queries. Loaded from Postgres by LoadInMemory.
type InMemoryGraph struct {
	// Edges maps source_symbol → []Edge (the outgoing edges).
	Edges map[string][]store.Edge

	// ReverseEdges maps target_symbol → []Edge (the incoming edges).
	// Not used by BFS reachability forward, but needed for reverse-impact queries.
	ReverseEdges map[string][]store.Edge
}

// LoadInMemory fetches all edges for (repo, commitHash) from Postgres and
// builds an in-memory adjacency list. This is the graph the reachability
// engine walks.
func LoadInMemory(ctx context.Context, db *store.DB, repo, commitHash string) (*InMemoryGraph, error) {
	edges, err := db.LoadEdges(ctx, repo, commitHash)
	if err != nil {
		return nil, fmt.Errorf("load edges: %w", err)
	}

	g := &InMemoryGraph{
		Edges:        make(map[string][]store.Edge),
		ReverseEdges: make(map[string][]store.Edge),
	}
	for _, e := range edges {
		g.Edges[e.SourceSymbol] = append(g.Edges[e.SourceSymbol], e)
		g.ReverseEdges[e.TargetSymbol] = append(g.ReverseEdges[e.TargetSymbol], e)
	}
	return g, nil
}

// BuildV2 is the same as Build but also populates the type_ifaces table and
// uses DispatchSiteV2 (with method_name) for correct incremental updates.
// This should be used instead of Build when incremental updates are enabled.
func BuildV2(
	ctx context.Context,
	db *store.DB,
	table *symboltable.RepoSymbolTable,
	repo, commitHash string,
) (*BuildResult, error) {
	implementors, methodIndex := buildImplementorIndex(table)

	var edges []store.Edge
	var symRows []store.SymbolRow
	var dispatchSitesV2 []store.DispatchSiteV2
	var typeIfaces []store.TypeIface
	result := &BuildResult{}

	for _, file := range table.Files {
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
				result.DirectEdges++

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
					result.InterfaceResolvedEdges++
					continue
				}

				dispatchSitesV2 = append(dispatchSitesV2, store.DispatchSiteV2{
					Repo:           repo,
					CommitHash:     commitHash,
					InterfaceName:  ifaceName,
					MethodName:     methodName,
					CallSiteSymbol: ref.Caller,
					CallSiteFile:   file.FilePath,
				})

				concreteTargets := expandInterfaceCall(ifaceName, methodName, implementors, methodIndex)
				if len(concreteTargets) == 0 {
					edges = append(edges, store.Edge{
						Repo:         repo,
						CommitHash:   commitHash,
						SourceSymbol: ref.Caller,
						TargetSymbol: ref.Callee,
						Provenance:   "interface_resolved",
						SourceFile:   file.FilePath,
					})
					result.InterfaceResolvedEdges++
				} else {
					for _, target := range concreteTargets {
						edges = append(edges, store.Edge{
							Repo:         repo,
							CommitHash:   commitHash,
							SourceSymbol: ref.Caller,
							TargetSymbol: target,
							Provenance:   "interface_resolved",
							SourceFile:   file.FilePath,
						})
						result.InterfaceResolvedEdges++
					}
				}
			}
		}
	}

	if err := db.DeleteGraphForCommit(ctx, repo, commitHash); err != nil {
		return nil, fmt.Errorf("delete old graph: %w", err)
	}
	if err := db.InsertSymbols(ctx, symRows); err != nil {
		return nil, fmt.Errorf("insert symbols: %w", err)
	}
	if err := db.InsertEdges(ctx, edges); err != nil {
		return nil, fmt.Errorf("insert edges: %w", err)
	}
	if err := db.InsertDispatchSitesV2(ctx, dispatchSitesV2); err != nil {
		return nil, fmt.Errorf("insert dispatch sites v2: %w", err)
	}
	if err := db.InsertTypeIfaces(ctx, typeIfaces); err != nil {
		return nil, fmt.Errorf("insert type_ifaces: %w", err)
	}

	result.TotalEdges = len(edges)
	result.SymbolsIndexed = len(symRows)
	result.DispatchSites = len(dispatchSitesV2)
	return result, nil
}
