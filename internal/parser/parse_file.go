package parser

import (
	"go/ast"
	goparser "go/parser"
	"go/token"
)

// parseGoFile parses a single Go source file with ParseComments mode.
// It exists as a separate function because our package is named "parser",
// which shadows the standard library "go/parser" package. We import
// go/parser under the alias "goparser" here to avoid the collision.
//
// src may be nil, in which case the file is read from disk (the behaviour
// go/packages expects when it calls the ParseFile hook with nil src).
func parseGoFile(fset *token.FileSet, filename string, src []byte) (*ast.File, error) {
	var srcArg interface{}
	if src != nil {
		srcArg = src
	}
	return goparser.ParseFile(fset, filename, srcArg, goparser.ParseComments)
}
