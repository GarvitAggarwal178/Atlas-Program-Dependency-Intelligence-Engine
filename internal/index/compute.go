// Package index wires internal/parser's output into the derivation-tracked
// interval store (internal/store), producing the CALL facts and their
// derivation inputs for a single commit's parsed symbol table.
//
// This is the missing piece flagged at the end of build-order step 5:
// derivation tracking existed as a primitive layer (open/close a fact,
// record what it was derived from, query what's stale) with nothing yet
// driving it from real parsed Go source. This package is that driver.
//
// What this package deliberately does NOT do: it does not skip re-parsing
// unchanged files (that's backward validation, architecture.md section
// 2.3 / build-order step 10) — every call to ComputeFacts does a full
// derivation pass over the given RepoSymbolTable. What it DOES do
// correctly is apply the RESULT through the interval store's open/close
// diff (ApplyFacts), so the live fact set at any seq is correct regardless
// of how much redundant work went into computing it. Skipping redundant
// work is a performance concern layered on top later; this package's job
// is correctness of the resulting fact set.
package index

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/yourorg/symex/internal/callgraph"
	"github.com/yourorg/symex/internal/derive"
	"github.com/yourorg/symex/internal/store"
	"github.com/yourorg/symex/internal/symboltable"
)

// FactWithDerivations pairs a fact (FactID left zero until OpenFact
// assigns one) with the derivation rows it should be recorded against.
type FactWithDerivations struct {
	Fact        store.Fact
	Derivations []store.Derivation // FactID filled in by ApplyFacts after Open/lookup
}

// ComputeFacts derives the full CALL fact set (with derivation inputs) for
// repoRoot's already-parsed table. repoRoot is used only to read file
// content for FILE input hashing — table must already reflect repoRoot's
// current on-disk state (i.e. the caller has checked out the right commit
// before parsing).
func ComputeFacts(repoRoot, modulePath string, table *symboltable.RepoSymbolTable) ([]FactWithDerivations, error) {
	fileHashes := make(map[string]string)
	getFileHash := func(relPath string) (string, error) {
		if h, ok := fileHashes[relPath]; ok {
			return h, nil
		}
		content, err := os.ReadFile(filepath.Join(repoRoot, relPath))
		if err != nil {
			return "", fmt.Errorf("read %s for FILE hash: %w", relPath, err)
		}
		h := derive.FileStructuralHash(content)
		fileHashes[relPath] = h
		return h, nil
	}

	// interfaceImplementers[ifaceName] = sorted implementer type names,
	// computed once per interface name the first time it's dispatched
	// through, since it doesn't depend on the call site.
	implementerCache := make(map[string][]string)
	getImplementers := func(ifaceName string) []string {
		if v, ok := implementerCache[ifaceName]; ok {
			return v
		}
		v := implementersOf(table, ifaceName)
		implementerCache[ifaceName] = v
		return v
	}

	var result []FactWithDerivations

	for _, file := range table.Files {
		fileHash, err := getFileHash(file.FilePath)
		if err != nil {
			return nil, err
		}

		for _, ref := range file.References {
			callSite := fmt.Sprintf("%s:%d", file.FilePath, ref.Line)

			switch ref.Kind {
			case symboltable.CallDirect:
				result = append(result, FactWithDerivations{
					Fact: store.Fact{
						Kind: store.FactKindCall, SourceSymbol: ref.Caller, TargetSymbol: ref.Callee,
						Provenance: "direct_call", CallSite: callSite,
						SourceFile: file.FilePath, SourceModule: modulePath,
					},
					Derivations: []store.Derivation{
						{InputKind: store.InputKindFile, InputKey: file.FilePath, InputHash: fileHash},
					},
				})

			case symboltable.CallInterfaceResolved:
				ifaceName, methodName, ok := splitInterfaceCallee(ref.Callee)
				if !ok {
					// Can't parse — keep a raw edge, no INTERFACE derivation
					// (mirrors v2's "don't silently drop it" behavior).
					result = append(result, FactWithDerivations{
						Fact: store.Fact{
							Kind: store.FactKindCall, SourceSymbol: ref.Caller, TargetSymbol: ref.Callee,
							Provenance: "interface_resolved", CallSite: callSite,
							SourceFile: file.FilePath, SourceModule: modulePath,
						},
						Derivations: []store.Derivation{
							{InputKind: store.InputKindFile, InputKey: file.FilePath, InputHash: fileHash},
						},
					})
					continue
				}

				implementers := getImplementers(ifaceName)
				ifaceHash := derive.ImplementerSetHash(implementers)
				derivations := []store.Derivation{
					{InputKind: store.InputKindFile, InputKey: file.FilePath, InputHash: fileHash},
					{InputKind: store.InputKindInterface, InputKey: ifaceName, InputHash: ifaceHash},
				}

				targets := callgraph.ExpandInterfaceCall(table, ifaceName, methodName)
				if len(targets) == 0 {
					result = append(result, FactWithDerivations{
						Fact: store.Fact{
							Kind: store.FactKindCall, SourceSymbol: ref.Caller, TargetSymbol: ref.Callee,
							Provenance: "interface_resolved", CallSite: callSite,
							SourceFile: file.FilePath, SourceModule: modulePath,
						},
						Derivations: derivations,
					})
					continue
				}
				for _, target := range targets {
					result = append(result, FactWithDerivations{
						Fact: store.Fact{
							Kind: store.FactKindCall, SourceSymbol: ref.Caller, TargetSymbol: target,
							Provenance: "interface_resolved", CallSite: callSite,
							SourceFile: file.FilePath, SourceModule: modulePath,
						},
						Derivations: derivations,
					})
				}
			}
		}
	}

	return result, nil
}

// implementersOf returns the qualified names of every concrete type in
// table that lists ifaceName in its ImplementedInterfaces.
func implementersOf(table *symboltable.RepoSymbolTable, ifaceName string) []string {
	var names []string
	for _, f := range table.Files {
		for _, sym := range f.Defined {
			if sym.Kind != symboltable.KindType {
				continue
			}
			for _, iface := range sym.ImplementedInterfaces {
				if iface == ifaceName {
					names = append(names, sym.QualifiedName)
				}
			}
		}
	}
	return names
}

// splitInterfaceCallee mirrors internal/callgraph's parseInterfaceCallee
// (unexported there): "pkg/path.InterfaceName.MethodName" ->
// ("pkg/path.InterfaceName", "MethodName").
func splitInterfaceCallee(callee string) (ifaceName, methodName string, ok bool) {
	lastDot := strings.LastIndex(callee, ".")
	if lastDot < 0 {
		return "", "", false
	}
	ifaceName = callee[:lastDot]
	methodName = callee[lastDot+1:]
	if ifaceName == "" || methodName == "" {
		return "", "", false
	}
	return ifaceName, methodName, true
}
