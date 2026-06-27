package callgraph

import "github.com/yourorg/symex/internal/symboltable"

// ExpandInterfaceCall builds the implementor index from table and returns the
// list of concrete method targets for a call through ifaceName.MethodName.
//
// This is the production-code version of ExpandInterfaceCallExported (which
// lives in export_test.go and is only available during tests).
// The incremental engine and any other non-test code must call this.
func ExpandInterfaceCall(
	table *symboltable.RepoSymbolTable,
	ifaceName, methodName string,
) []string {
	implementors, methodIndex := buildImplementorIndex(table)
	return expandInterfaceCall(ifaceName, methodName, implementors, methodIndex)
}

// ExpandInterfaceCallWithIndex is a performance-oriented variant for callers
// that already built the implementor index (e.g. BuildV2 builds it once and
// re-uses it across all references in the repo). Avoids rebuilding the index
// per call site.
func ExpandInterfaceCallWithIndex(
	ifaceName, methodName string,
	implementors map[string][]*symboltable.DefinedSymbol,
	methodIndex map[string]string,
) []string {
	return expandInterfaceCall(ifaceName, methodName, implementors, methodIndex)
}
