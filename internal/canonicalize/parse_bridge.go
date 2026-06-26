package canonicalize

import (
	"go/ast"
	"go/parser"
	"go/token"
)

// _parseBytes is the bridge that lets hash.go call go/parser without importing
// the project's own parser package (which would be a circular dependency).
func _parseBytes(fset *token.FileSet, src []byte) (*ast.File, error) {
	return parser.ParseFile(fset, "", src, 0) // mode 0: no comments
}
