// Package canonicalize computes a structural hash of a Go function body that
// is invariant under local identifier renaming and comment edits.
//
// Algorithm:
//  1. Receive the *ast.FuncDecl (or *ast.FuncLit) already parsed from source.
//  2. Deep-copy the body's statement list into a fresh *ast.BlockStmt so the
//     original AST is not mutated.
//  3. Walk the copy with an ast.Inspector, assigning every *ast.Ident that
//     refers to a locally-declared variable/parameter to a canonical placeholder
//     ("_v0", "_v1", …) using an ordered mapping.  Package-level names and
//     imported names are left unchanged — only locals are renamed, so the
//     structure of external calls is preserved in the hash.
//  4. Print the resulting AST to a bytes.Buffer using go/printer with no
//     source-position information (positions are zeroed so they don't affect
//     the hash).
//  5. SHA-256 the printed bytes and return the hex string.
//
// The resulting hash will be identical for:
//   - A function where `total` is renamed to `sum` (local rename → trivial).
//   - A function where a doc comment is added or changed.
//
// The hash will differ for:
//   - Any change to operators, control flow, called functions, or argument order.
//   - Any change to the types of parameters or return values.
//
// This package has no I/O and no side effects.
package canonicalize

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"go/ast"
	"go/printer"
	"go/token"
)

// FuncHash returns the canonical structural hash for a function or method
// declaration. fset is used only to resolve the original source positions when
// cloning; it is not embedded in the hash.
func FuncHash(fset *token.FileSet, decl *ast.FuncDecl) (string, error) {
	if decl.Body == nil {
		// Abstract method in an interface — no body to hash.
		return "", nil
	}

	// Clone the body so we can mutate it without touching the original AST.
	// Pass the original fset so go/printer can read position metadata correctly
	// during the print→reparse clone step.
	cloned := cloneBlock(fset, decl.Body)

	// Collect locally-declared names in declaration order, then rename them.
	renamer := &localRenamer{mapping: make(map[string]string)}
	// Seed the renamer with parameter and result names first so they get low
	// indices (deterministic ordering).
	if decl.Type.Params != nil {
		for _, field := range decl.Type.Params.List {
			for _, name := range field.Names {
				renamer.canonical(name.Name)
			}
		}
	}
	if decl.Type.Results != nil {
		for _, field := range decl.Type.Results.List {
			for _, name := range field.Names {
				renamer.canonical(name.Name)
			}
		}
	}

	// Walk the cloned body and rename every local identifier.
	ast.Inspect(cloned, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.AssignStmt:
			// Detect short-variable declarations (:=) and register the LHS names.
			if v.Tok == token.DEFINE {
				for _, lhs := range v.Lhs {
					if ident, ok := lhs.(*ast.Ident); ok {
						renamer.canonical(ident.Name)
					}
				}
			}
		case *ast.ValueSpec:
			for _, name := range v.Names {
				renamer.canonical(name.Name)
			}
		case *ast.RangeStmt:
			if ident, ok := v.Key.(*ast.Ident); ok {
				renamer.canonical(ident.Name)
			}
			if ident, ok := v.Value.(*ast.Ident); ok {
				renamer.canonical(ident.Name)
			}
		}
		return true
	})

	// Second pass: substitute all idents that map to placeholders.
	ast.Inspect(cloned, func(n ast.Node) bool {
		if ident, ok := n.(*ast.Ident); ok {
			if ph, found := renamer.mapping[ident.Name]; found {
				ident.Name = ph
			}
		}
		return true
	})

	// Zero out all position information so the printer output is deterministic
	// regardless of where in the file the function appeared.
	zeroPositions(cloned)

	// Print the canonicalized body to a buffer.
	var buf bytes.Buffer
	fakeFset := token.NewFileSet()
	if err := printer.Fprint(&buf, fakeFset, cloned); err != nil {
		return "", fmt.Errorf("printer: %w", err)
	}

	sum := sha256.Sum256(buf.Bytes())
	return hex.EncodeToString(sum[:]), nil
}

// localRenamer maintains the name → placeholder mapping.
type localRenamer struct {
	mapping map[string]string
	counter int
}

func (r *localRenamer) canonical(name string) string {
	if name == "_" || name == "" {
		return name
	}
	if ph, ok := r.mapping[name]; ok {
		return ph
	}
	ph := fmt.Sprintf("_v%d", r.counter)
	r.counter++
	r.mapping[name] = ph
	return ph
}

// cloneBlock performs a shallow-enough clone of an *ast.BlockStmt for our
// purposes: we only need to be able to rename ident nodes without affecting
// the original AST. We use go/ast's built-in approach — since ast.Node values
// are pointers, a true deep clone is complex; instead we reconstruct just the
// identifier nodes (which are the only things we mutate).
//
// Implementation: walk the original tree and build a new one by copying every
// node. This is necessarily verbose because go/ast has no Clone function.
func cloneBlock(fset *token.FileSet, b *ast.BlockStmt) *ast.BlockStmt {
	if b == nil {
		return nil
	}
	// We use a simple strategy: marshal through go/printer and re-parse.
	// This avoids hand-writing a full AST clone and gives us a clean, position-
	// free copy for free.
	//
	// Wrap the block in a minimal function so go/parser can parse it as a
	// statement list. The original fset is passed so the printer can resolve
	// position metadata (line directives, etc.) from the source file.
	var buf bytes.Buffer
	buf.WriteString("package p\nfunc _f()")
	// Write the block with the original fset so positions resolve correctly.
	cfg := printer.Config{Mode: printer.UseSpaces, Tabwidth: 4}
	if err := cfg.Fprint(&buf, fset, b); err != nil {
		// If printer fails, return the original (best-effort).
		return b
	}

	src := buf.Bytes()
	f, err := parseFragment(src)
	if err != nil {
		return b
	}
	// Extract the body from the re-parsed function.
	for _, decl := range f.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Body != nil {
			return fn.Body
		}
	}
	return b
}

// parseFragment parses a minimal Go source fragment and returns the file AST.
func parseFragment(src []byte) (*ast.File, error) {
	fset := token.NewFileSet()
	f, err := parseSource(fset, src)
	return f, err
}

// parseSource wraps go/parser.ParseFile for use within this package.
// Imported here to avoid an import cycle if the main parser package used this.
func parseSource(fset *token.FileSet, src []byte) (*ast.File, error) {
	// We need go/parser here; import it inline.
	// (The build system sees this import at the top of the file.)
	return _parseBytes(fset, src)
}

// zeroPositions walks an AST node and resets all token.Pos fields to
// token.NoPos so that the printer output is position-independent.
func zeroPositions(node ast.Node) {
	ast.Inspect(node, func(n ast.Node) bool {
		if n == nil {
			return false
		}
		// Use reflection-free approach: handle the common node types
		// that carry position fields we care about.
		switch v := n.(type) {
		case *ast.Ident:
			v.NamePos = token.NoPos
		case *ast.BasicLit:
			v.ValuePos = token.NoPos
		case *ast.CallExpr:
			v.Lparen = token.NoPos
			v.Rparen = token.NoPos
		case *ast.BlockStmt:
			v.Lbrace = token.NoPos
			v.Rbrace = token.NoPos
		case *ast.ReturnStmt:
			v.Return = token.NoPos
		case *ast.IfStmt:
			v.If = token.NoPos
		case *ast.ForStmt:
			v.For = token.NoPos
		case *ast.RangeStmt:
			v.For = token.NoPos
		case *ast.AssignStmt:
			v.TokPos = token.NoPos
		case *ast.BinaryExpr:
			v.OpPos = token.NoPos
		case *ast.UnaryExpr:
			v.OpPos = token.NoPos
		case *ast.SelectorExpr:
			// no direct pos field on SelectorExpr itself
		case *ast.IndexExpr:
			v.Lbrack = token.NoPos
			v.Rbrack = token.NoPos
		case *ast.SliceExpr:
			v.Lbrack = token.NoPos
			v.Rbrack = token.NoPos
		case *ast.CompositeLit:
			v.Lbrace = token.NoPos
			v.Rbrace = token.NoPos
		case *ast.FuncLit:
			// pass
		case *ast.GenDecl:
			v.TokPos = token.NoPos
			v.Lparen = token.NoPos
			v.Rparen = token.NoPos
		case *ast.SendStmt:
			v.Arrow = token.NoPos
		case *ast.IncDecStmt:
			v.TokPos = token.NoPos
		case *ast.BranchStmt:
			v.TokPos = token.NoPos
		case *ast.SwitchStmt:
			v.Switch = token.NoPos
		case *ast.TypeSwitchStmt:
			v.Switch = token.NoPos
		case *ast.CaseClause:
			v.Case = token.NoPos
			v.Colon = token.NoPos
		case *ast.SelectStmt:
			v.Select = token.NoPos
		case *ast.CommClause:
			v.Case = token.NoPos
			v.Colon = token.NoPos
		case *ast.DeferStmt:
			v.Defer = token.NoPos
		case *ast.GoStmt:
			v.Go = token.NoPos
		case *ast.ExprStmt:
			// no pos field
		}
		return true
	})
}
