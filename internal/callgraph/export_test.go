// export_test.go exposes internal functions for white-box testing.
// This file is only compiled when running tests (the _test.go suffix).
package callgraph

import "github.com/yourorg/symex/internal/symboltable"

// ParseInterfaceCalleeExported is the exported test accessor for
// parseInterfaceCallee.
func ParseInterfaceCalleeExported(callee string) (ifaceName, methodName string, err error) {
	return parseInterfaceCallee(callee)
}

// ExpandInterfaceCallExported builds the implementor index from table and then
// calls expandInterfaceCall, returning the resulting target list.
// Used by tests to verify expansion without requiring a live Postgres.
func ExpandInterfaceCallExported(
	table *symboltable.RepoSymbolTable,
	ifaceName, methodName string,
) []string {
	implementors, methodIndex := buildImplementorIndex(table)
	return expandInterfaceCall(ifaceName, methodName, implementors, methodIndex)
}
