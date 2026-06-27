// Package classifier implements the semantic change classifier for stage 3.
//
// For each symbol in the changed symbol set produced by the differ, it
// compares the pre-change and post-change versions of the function and assigns
// one of three classifications:
//
//   - trivial:          Only comments, whitespace, or local identifier renames
//                       changed.  The canonical structural hash is identical
//                       between versions.  Zero blast radius: this symbol is
//                       excluded from the reachability frontier entirely.
//
//   - signature_change: The parameter types, return types, or receiver type
//                       differ between versions, regardless of what the body
//                       does.  Every caller is potentially affected regardless
//                       of body logic, so this is treated as a maximum-impact
//                       change.
//
//   - logic_change:     The canonical hash differs but the signature is
//                       unchanged.  Normal reachability from this symbol.
//
// # How canonicalization avoids false positives on renames
//
// The naive approach — compare raw source text — would flag a pure rename
// (total → sum) as a logic change because the text differs.  The canonical
// hash avoids this by alpha-renaming all locally-declared identifiers to
// positional placeholders (_v0, _v1, …) before hashing.  After renaming,
// `total := x+y; return total` and `sum := x+y; return sum` produce identical
// printed ASTs and therefore identical SHA-256 hashes.  Comments are stripped
// during the cloneBlock step (the body is reprinted without ParseComments).
// Together these two transformations mean the hash is invariant under exactly
// the changes we want to call "trivial."
//
// The critical test case: a logic change disguised as a rename.
// Consider renaming `total` to `sum` while simultaneously changing `+` to `*`.
// After alpha-renaming, both versions have `_v0 := _v1 * _v2` (changed) vs
// `_v0 := _v1 + _v2` (original).  The operator difference survives
// canonicalization and produces different hashes.  This is the failure mode
// the classifier is specifically designed to avoid.
package classifier

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"strings"

	"github.com/yourorg/symex/internal/canonicalize"
)

// ChangeKind is the result of classifying a single changed symbol.
type ChangeKind string

const (
	// Trivial: only comments, whitespace, or local identifier renames changed.
	// Canonical hash is identical.  Zero blast radius.
	Trivial ChangeKind = "trivial"

	// SignatureChange: parameter types, return types, or receiver type differ.
	// Affects every caller regardless of body logic.
	SignatureChange ChangeKind = "signature_change"

	// LogicChange: canonical hash differs, signature unchanged.
	// Normal reachability applies.
	LogicChange ChangeKind = "logic_change"
)

// Classification is the result of classifying one symbol's change.
type Classification struct {
	// Symbol is the fully-qualified name of the classified symbol.
	Symbol string `json:"symbol"`

	// Kind is the classification result.
	Kind ChangeKind `json:"kind"`

	// Reason is a human-readable explanation for the classification, suitable
	// for display in the analysis output and for debugging.
	Reason string `json:"reason"`

	// BeforeHash is the canonical structural hash of the pre-change version.
	// Empty if the symbol did not exist in the base commit (new function).
	BeforeHash string `json:"before_hash,omitempty"`

	// AfterHash is the canonical structural hash of the post-change version.
	// Empty if the symbol was deleted in the head commit.
	AfterHash string `json:"after_hash,omitempty"`

	// BeforeSignature is the printed parameter/return signature of the
	// pre-change version, e.g. "(amount int) error".
	BeforeSignature string `json:"before_signature,omitempty"`

	// AfterSignature is the printed parameter/return signature of the
	// post-change version.
	AfterSignature string `json:"after_signature,omitempty"`
}

// SourceFetcher retrieves the source of a file at a specific git commit.
// The returned bytes are the full file content at that commit.
// Implementing types: GitFetcher (real), MockFetcher (tests).
type SourceFetcher interface {
	FetchAt(commitSHA, repoRelativePath string) ([]byte, error)
}

// ClassifySymbol compares the pre-change and post-change versions of a named
// symbol and returns its Classification.
//
// Parameters:
//   - fetcher:     provides file source at arbitrary commits
//   - baseCommit:  the commit before the change (the "before" version)
//   - headCommit:  the commit after the change (the "after" version)
//   - filePath:    repo-relative path to the file containing the symbol
//   - symbolName:  fully-qualified symbol name, e.g.
//                  "pkg/path.FuncName" or "pkg/path.(TypeName).MethodName"
//
// New functions (present only in head) are classified as logic_change.
// Deleted functions (present only in base) are classified as logic_change.
func ClassifySymbol(
	fetcher SourceFetcher,
	baseCommit, headCommit string,
	filePath, symbolName string,
) (*Classification, error) {
	// Fetch source at both commits.
	beforeSrc, err := fetcher.FetchAt(baseCommit, filePath)
	if err != nil {
		// File did not exist at base commit — new file, treat as logic_change.
		return &Classification{
			Symbol: symbolName,
			Kind:   LogicChange,
			Reason: "file is new (not present at base commit)",
		}, nil
	}

	afterSrc, err := fetcher.FetchAt(headCommit, filePath)
	if err != nil {
		// File deleted at head — treat as logic_change (blast radius still applies).
		return &Classification{
			Symbol: symbolName,
			Kind:   LogicChange,
			Reason: "file was deleted at head commit",
		}, nil
	}

	// Parse both versions.
	beforeDecl, beforeFset, err := findFuncDecl(beforeSrc, symbolName)
	if err != nil {
		return nil, fmt.Errorf("parse before: %w", err)
	}

	afterDecl, afterFset, err := findFuncDecl(afterSrc, symbolName)
	if err != nil {
		return nil, fmt.Errorf("parse after: %w", err)
	}

	// Handle symbol appearing/disappearing between versions.
	if beforeDecl == nil && afterDecl == nil {
		return &Classification{
			Symbol: symbolName,
			Kind:   LogicChange,
			Reason: "symbol not found in either version",
		}, nil
	}
	if beforeDecl == nil {
		hash, _ := canonicalize.FuncHash(afterFset, afterDecl)
		return &Classification{
			Symbol:    symbolName,
			Kind:      LogicChange,
			AfterHash: hash,
			Reason:    "symbol is new (not present at base commit)",
		}, nil
	}
	if afterDecl == nil {
		hash, _ := canonicalize.FuncHash(beforeFset, beforeDecl)
		return &Classification{
			Symbol:     symbolName,
			Kind:       LogicChange,
			BeforeHash: hash,
			Reason:     "symbol was deleted at head commit",
		}, nil
	}

	// Both versions exist — compute canonical hashes and signatures.
	beforeHash, err := canonicalize.FuncHash(beforeFset, beforeDecl)
	if err != nil {
		return nil, fmt.Errorf("hash before: %w", err)
	}
	afterHash, err := canonicalize.FuncHash(afterFset, afterDecl)
	if err != nil {
		return nil, fmt.Errorf("hash after: %w", err)
	}

	beforeSig := printSignature(beforeFset, beforeDecl)
	afterSig := printSignature(afterFset, afterDecl)

	c := &Classification{
		Symbol:          symbolName,
		BeforeHash:      beforeHash,
		AfterHash:       afterHash,
		BeforeSignature: beforeSig,
		AfterSignature:  afterSig,
	}

	// Classification order:
	//  1. Signature change takes priority — even if the body hash happens to
	//     be identical (body copy-paste with new types), callers are affected.
	//  2. Trivial: hashes identical, signature identical.
	//  3. Logic change: hashes differ, signature identical.
	if beforeSig != afterSig {
		c.Kind = SignatureChange
		c.Reason = fmt.Sprintf("signature changed: %q → %q", beforeSig, afterSig)
		return c, nil
	}

	if beforeHash == afterHash {
		c.Kind = Trivial
		c.Reason = "canonical hash identical (comment/whitespace/rename only)"
		return c, nil
	}

	c.Kind = LogicChange
	c.Reason = fmt.Sprintf("body changed: hash %s → %s", shortHash(beforeHash), shortHash(afterHash))
	return c, nil
}

// ClassifyAll classifies every symbol in the given map (symbol → filePath).
// Returns a map from symbol name to Classification and a filtered frontier
// that excludes trivial changes.
func ClassifyAll(
	fetcher SourceFetcher,
	baseCommit, headCommit string,
	changedSymbols map[string]string, // symbol → filePath
) (
	classifications map[string]*Classification,
	filteredFrontier map[string]string,
	err error,
) {
	classifications = make(map[string]*Classification, len(changedSymbols))
	filteredFrontier = make(map[string]string)

	for symbol, filePath := range changedSymbols {
		c, classErr := ClassifySymbol(fetcher, baseCommit, headCommit, filePath, symbol)
		if classErr != nil {
			// Non-fatal: treat unclassifiable as logic_change (safe default).
			c = &Classification{
				Symbol: symbol,
				Kind:   LogicChange,
				Reason: fmt.Sprintf("classification error (defaulting to logic_change): %v", classErr),
			}
		}
		classifications[symbol] = c

		// Trivial changes are excluded from the frontier.
		if c.Kind != Trivial {
			filteredFrontier[symbol] = filePath
		}
	}

	return classifications, filteredFrontier, nil
}

// findFuncDecl parses src and locates the *ast.FuncDecl matching symbolName.
// symbolName is in the form "importPath.FuncName" or
// "importPath.(ReceiverType).MethodName".
// Returns (nil, fset, nil) if the symbol is not found in the file (not an error).
func findFuncDecl(src []byte, symbolName string) (*ast.FuncDecl, *token.FileSet, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", src, parser.ParseComments)
	if err != nil {
		// Partial parse is acceptable — go/parser returns a partial AST even
		// on error.  We proceed with what we got.
		if f == nil {
			return nil, fset, fmt.Errorf("parse: %w", err)
		}
	}

	// Extract the unqualified function name and optional receiver type from
	// the fully-qualified symbolName.
	funcName, receiverType := parseSymbolName(symbolName)

	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		if fd.Name.Name != funcName {
			continue
		}

		// For methods, also check receiver type.
		if receiverType != "" {
			if fd.Recv == nil || len(fd.Recv.List) == 0 {
				continue
			}
			recv := extractReceiverTypeName(fd.Recv.List[0].Type)
			if recv != receiverType {
				continue
			}
		} else {
			// Looking for a package-level function — skip methods.
			if fd.Recv != nil && len(fd.Recv.List) > 0 {
				continue
			}
		}

		return fd, fset, nil
	}

	return nil, fset, nil // symbol not found in this file — not an error
}

// parseSymbolName splits a fully-qualified symbol name into (funcName, receiverType).
//
// Input formats:
//   - "pkg/path.FuncName"              → ("FuncName", "")
//   - "pkg/path.(ReceiverType).Method" → ("Method", "ReceiverType")
func parseSymbolName(qualifiedName string) (funcName, receiverType string) {
	// Check for method form: contains "(ReceiverType)"
	if idx := strings.Index(qualifiedName, ".("); idx >= 0 {
		// "pkg/path.(ReceiverType).MethodName"
		rest := qualifiedName[idx+2:] // "ReceiverType).MethodName"
		closeIdx := strings.Index(rest, ")")
		if closeIdx >= 0 {
			receiverType = rest[:closeIdx]
			// rest[closeIdx:] is ").MethodName"
			methodPart := rest[closeIdx+2:] // "MethodName"
			return methodPart, receiverType
		}
	}

	// Plain function: last dot-separated segment is the function name.
	lastDot := strings.LastIndex(qualifiedName, ".")
	if lastDot >= 0 {
		return qualifiedName[lastDot+1:], ""
	}
	return qualifiedName, ""
}

// extractReceiverTypeName returns the base type name from a receiver type
// expression, stripping pointer stars: "*Handler" → "Handler".
func extractReceiverTypeName(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.StarExpr:
		return extractReceiverTypeName(t.X)
	case *ast.Ident:
		return t.Name
	case *ast.IndexExpr:
		return extractReceiverTypeName(t.X)
	default:
		return fmt.Sprintf("%T", expr)
	}
}

// printSignature returns a normalized string representation of a function's
// parameter and return type signature — without the function name, receiver,
// or body.
//
// Examples:
//   - func Foo(x int, y string) error  → "(int, string) error"
//   - func (r *Handler) Get(id string) (int, error) → "(string) (int, error)"
//
// The output is used for signature comparison.  We print the types only
// (not parameter names) to avoid false positives on parameter renames.
// A rename like `func F(amount int)` → `func F(total int)` must NOT be
// classified as a signature change; only the type matters for callers.
func printSignature(fset *token.FileSet, decl *ast.FuncDecl) string {
	if decl.Type == nil {
		return ""
	}

	// Build a stripped FuncType with no parameter names — only types.
	stripped := stripParamNames(decl.Type)

	var buf strings.Builder
	cfg := printer.Config{Mode: printer.UseSpaces, Tabwidth: 4}

	// Print params.
	buf.WriteString("(")
	if stripped.Params != nil {
		for i, field := range stripped.Params.List {
			if i > 0 {
				buf.WriteString(", ")
			}
			cfg.Fprint(&buf, token.NewFileSet(), field.Type)
		}
	}
	buf.WriteString(")")

	// Print results.
	if stripped.Results != nil && len(stripped.Results.List) > 0 {
		buf.WriteString(" ")
		if len(stripped.Results.List) == 1 {
			cfg.Fprint(&buf, token.NewFileSet(), stripped.Results.List[0].Type)
		} else {
			buf.WriteString("(")
			for i, field := range stripped.Results.List {
				if i > 0 {
					buf.WriteString(", ")
				}
				cfg.Fprint(&buf, token.NewFileSet(), field.Type)
			}
			buf.WriteString(")")
		}
	}

	return buf.String()
}

// stripParamNames returns a copy of a FuncType with all parameter and result
// names removed — only their types remain.  This ensures that a rename of a
// parameter (e.g. `amount` → `total`) does not look like a signature change.
func stripParamNames(ft *ast.FuncType) *ast.FuncType {
	return &ast.FuncType{
		Params:  stripFieldListNames(ft.Params),
		Results: stripFieldListNames(ft.Results),
	}
}

// stripFieldListNames returns a copy of a FieldList with all Names removed.
func stripFieldListNames(fl *ast.FieldList) *ast.FieldList {
	if fl == nil {
		return nil
	}
	result := &ast.FieldList{}
	for _, field := range fl.List {
		result.List = append(result.List, &ast.Field{
			Type: field.Type,
			// Deliberately omit Names, Tag, Comment.
		})
	}
	return result
}

func shortHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}
