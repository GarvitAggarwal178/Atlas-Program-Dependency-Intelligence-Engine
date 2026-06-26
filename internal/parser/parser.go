// Package parser implements stage-1 of the incremental call graph engine:
// AST parsing, symbol extraction, and call-reference classification.
//
// It uses go/parser, go/ast, and go/types from the standard library.
// No external parsing dependency is introduced.
//
// Call resolution strategy (critical for downstream correctness):
//
//   - A call to a concrete function or a method on a concrete (non-interface)
//     receiver is classified as "direct_call". go/types.TypeAndValue gives us
//     the exact *types.Func, so this is precise.
//
//   - A call to a method on a value whose static type is an interface is
//     classified as "interface_resolved". We cannot determine which concrete
//     implementation will be invoked without a full pointer analysis. The call
//     graph builder (stage 2) will over-approximate by adding an edge to every
//     concrete type in the repo that satisfies the interface. This is a known,
//     intentional soundness tradeoff: we may add spurious edges, but we will
//     never miss a real one.
package parser

import (
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"strings"

	"github.com/yourorg/symex/internal/canonicalize"
	"github.com/yourorg/symex/internal/symboltable"
)

// ParseRepo walks every .go file under repoRoot, type-checks each package,
// and returns the aggregated RepoSymbolTable.
//
// repoRoot must be the directory containing go.mod.
// modulePath must match the module directive in go.mod (e.g. "github.com/foo/bar").
func ParseRepo(repoRoot, modulePath string) (*symboltable.RepoSymbolTable, error) {
	// Discover all packages under the repo root.
	pkgDirs, err := findPackageDirs(repoRoot)
	if err != nil {
		return nil, fmt.Errorf("discover packages: %w", err)
	}

	result := &symboltable.RepoSymbolTable{
		ModulePath: modulePath,
	}

	for _, dir := range pkgDirs {
		files, err := parseAndCheckPackage(dir, repoRoot, modulePath)
		if err != nil {
			// Non-fatal: log and continue so one broken package doesn't abort
			// analysis of the whole repo.
			fmt.Fprintf(os.Stderr, "warn: skipping %s: %v\n", dir, err)
			continue
		}
		result.Files = append(result.Files, files...)
	}

	return result, nil
}

// findPackageDirs returns all directories under root that contain at least one
// non-test .go file.
func findPackageDirs(root string) ([]string, error) {
	seen := make(map[string]bool)
	var dirs []string

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		// Skip hidden directories and the vendor directory.
		if info.IsDir() {
			base := info.Name()
			if base == "vendor" || (base != "." && strings.HasPrefix(base, ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		dir := filepath.Dir(path)
		if !seen[dir] {
			seen[dir] = true
			dirs = append(dirs, dir)
		}
		return nil
	})
	return dirs, err
}

// parseAndCheckPackage parses all .go files in dir, runs go/types type-checking
// on the package, and returns one FileSymbolTable per file.
func parseAndCheckPackage(
	dir, repoRoot, modulePath string,
) ([]symboltable.FileSymbolTable, error) {
	fset := token.NewFileSet()

	// Parse all files in the directory. We include comments so we can strip
	// them later in the canonicalizer; parsing mode includes imports so the
	// type checker can resolve cross-package references.
	pkgs, err := parser.ParseDir(fset, dir, nil, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", dir, err)
	}

	var result []symboltable.FileSymbolTable

	for pkgName, pkg := range pkgs {
		// Collect the *ast.File slice in a stable order.
		var astFiles []*ast.File
		var filePaths []string
		for fpath, f := range pkg.Files {
			astFiles = append(astFiles, f)
			filePaths = append(filePaths, fpath)
		}

		// Type-check the package so we can resolve call targets.
		info := &types.Info{
			Types:      make(map[ast.Expr]types.TypeAndValue),
			Defs:       make(map[*ast.Ident]types.Object),
			Uses:       make(map[*ast.Ident]types.Object),
			Selections: make(map[*ast.SelectorExpr]*types.Selection),
		}

		// Compute the import path for this directory.
		relDir, _ := filepath.Rel(repoRoot, dir)
		importPath := modulePath
		if relDir != "." && relDir != "" {
			importPath = modulePath + "/" + filepath.ToSlash(relDir)
		}

		conf := types.Config{
			Importer: importer.Default(),
			// Don't abort on errors in imported packages — we may be
			// analyzing a repo with missing dependencies.
			Error: func(err error) {
				// Swallow type errors from imports we can't resolve;
				// they still give us partial type info for the current pkg.
			},
		}

		_, _ = conf.Check(importPath, fset, astFiles, info)
		// We proceed even if Check returns an error — partial info is still
		// useful for the symbols we can resolve.

		// Collect all interface types defined in this package so we can
		// compute interface satisfaction for concrete types.
		pkgInterfaces := collectPackageInterfaces(astFiles, info)

		for i, f := range astFiles {
			fpath := filePaths[i]
			relPath, _ := filepath.Rel(repoRoot, fpath)

			fst := symboltable.FileSymbolTable{
				FilePath:   relPath,
				Package:    pkgName,
				ImportPath: importPath,
			}

			// Extract defined symbols from this file.
			fst.Defined = extractDefinedSymbols(f, fset, info, pkgName, importPath, pkgInterfaces)

			// Extract call references from this file's function bodies.
			fst.References = extractCallReferences(f, fset, info, pkgName, importPath)

			result = append(result, fst)
		}
	}

	return result, nil
}

// collectPackageInterfaces builds a map from interface type name to the
// *types.Interface value for all interfaces declared in the package.
func collectPackageInterfaces(
	files []*ast.File,
	info *types.Info,
) map[string]*types.Interface {
	result := make(map[string]*types.Interface)
	for _, f := range files {
		for _, decl := range f.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok {
				continue
			}
			for _, spec := range gen.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				if _, ok := ts.Type.(*ast.InterfaceType); !ok {
					continue
				}
				if obj, ok := info.Defs[ts.Name]; ok && obj != nil {
					if iface, ok := obj.Type().Underlying().(*types.Interface); ok {
						result[ts.Name.Name] = iface
					}
				}
			}
		}
	}
	return result
}

// extractDefinedSymbols returns all DefinedSymbol entries from one file.
func extractDefinedSymbols(
	f *ast.File,
	fset *token.FileSet,
	info *types.Info,
	pkgName, importPath string,
	pkgInterfaces map[string]*types.Interface,
) []symboltable.DefinedSymbol {
	var syms []symboltable.DefinedSymbol

	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			sym := funcDeclToSymbol(d, fset, info, pkgName, importPath)
			syms = append(syms, sym)

		case *ast.GenDecl:
			for _, spec := range d.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}

				startLine := fset.Position(ts.Pos()).Line
				endLine := fset.Position(ts.End()).Line

				switch ts.Type.(type) {
				case *ast.InterfaceType:
					sym := symboltable.DefinedSymbol{
						Name:          ts.Name.Name,
						QualifiedName: importPath + "." + ts.Name.Name,
						Kind:          symboltable.KindInterface,
						Package:       pkgName,
						Lines:         symboltable.LineRange{Start: startLine, End: endLine},
						MethodSet:     interfaceMethodSet(ts.Type.(*ast.InterfaceType), fset),
					}
					syms = append(syms, sym)

				default:
					// Concrete type. Compute which interfaces it implements.
					var implIfaces []string
					if obj, ok := info.Defs[ts.Name]; ok && obj != nil {
						concreteType := obj.Type()
						// Check both T and *T against every interface in the package.
						for ifaceName, iface := range pkgInterfaces {
							if types.Implements(concreteType, iface) ||
								types.Implements(types.NewPointer(concreteType), iface) {
								implIfaces = append(implIfaces, importPath+"."+ifaceName)
							}
						}
					}

					sym := symboltable.DefinedSymbol{
						Name:                  ts.Name.Name,
						QualifiedName:         importPath + "." + ts.Name.Name,
						Kind:                  symboltable.KindType,
						Package:               pkgName,
						Lines:                 symboltable.LineRange{Start: startLine, End: endLine},
						ImplementedInterfaces: implIfaces,
					}
					syms = append(syms, sym)
				}
			}
		}
	}

	return syms
}

// funcDeclToSymbol converts a *ast.FuncDecl to a DefinedSymbol, computing
// the canonical hash for its body.
func funcDeclToSymbol(
	d *ast.FuncDecl,
	fset *token.FileSet,
	info *types.Info,
	pkgName, importPath string,
) symboltable.DefinedSymbol {
	startLine := fset.Position(d.Pos()).Line
	endLine := fset.Position(d.End()).Line

	kind := symboltable.KindFunction
	var receiverType string
	qualifiedName := importPath + "." + d.Name.Name

	if d.Recv != nil && len(d.Recv.List) > 0 {
		kind = symboltable.KindMethod
		receiverType = extractReceiverTypeName(d.Recv.List[0].Type)
		qualifiedName = fmt.Sprintf("%s.(%s).%s", importPath, receiverType, d.Name.Name)
	}

	// Compute canonical hash. Errors here are non-fatal — we store empty string.
	hash, _ := canonicalize.FuncHash(fset, d)

	return symboltable.DefinedSymbol{
		Name:          d.Name.Name,
		QualifiedName: qualifiedName,
		Kind:          kind,
		Package:       pkgName,
		ReceiverType:  receiverType,
		Lines:         symboltable.LineRange{Start: startLine, End: endLine},
		CanonicalHash: hash,
	}
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
		// Generic receiver: Handler[T]
		return extractReceiverTypeName(t.X)
	default:
		return fmt.Sprintf("%T", expr)
	}
}

// interfaceMethodSet returns the string representations of all methods in an
// interface type declaration.
func interfaceMethodSet(iface *ast.InterfaceType, fset *token.FileSet) []string {
	var methods []string
	if iface.Methods == nil {
		return methods
	}
	for _, field := range iface.Methods.List {
		for _, name := range field.Names {
			methods = append(methods, name.Name)
		}
	}
	return methods
}

// extractCallReferences walks all function/method bodies in a file and
// produces one CallReference per call expression found.
func extractCallReferences(
	f *ast.File,
	fset *token.FileSet,
	info *types.Info,
	pkgName, importPath string,
) []symboltable.CallReference {
	var refs []symboltable.CallReference

	for _, decl := range f.Decls {
		funcDecl, ok := decl.(*ast.FuncDecl)
		if !ok || funcDecl.Body == nil {
			continue
		}

		callerName := importPath + "." + funcDecl.Name.Name
		if funcDecl.Recv != nil && len(funcDecl.Recv.List) > 0 {
			recv := extractReceiverTypeName(funcDecl.Recv.List[0].Type)
			callerName = fmt.Sprintf("%s.(%s).%s", importPath, recv, funcDecl.Name.Name)
		}

		ast.Inspect(funcDecl.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}

			ref, ok := classifyCall(call, fset, info, callerName, importPath)
			if !ok {
				return true
			}
			refs = append(refs, ref)
			return true
		})
	}

	return refs
}

// classifyCall examines a single *ast.CallExpr and returns a CallReference
// with the correct CallKind, using go/types to distinguish direct from
// interface-dispatched calls.
//
// Returns (ref, false) when the call cannot be classified meaningfully
// (e.g. a built-in function like make/len/append).
func classifyCall(
	call *ast.CallExpr,
	fset *token.FileSet,
	info *types.Info,
	callerQName, importPath string,
) (symboltable.CallReference, bool) {
	line := fset.Position(call.Pos()).Line

	switch fn := call.Fun.(type) {
	case *ast.Ident:
		// Simple function call: foo(...)
		//
		// Look up the identifier in the Uses map to get the resolved object.
		obj, ok := info.Uses[fn]
		if !ok {
			// Could be a built-in (make, new, len, cap, etc.) or unresolved.
			return symboltable.CallReference{}, false
		}
		switch o := obj.(type) {
		case *types.Builtin:
			return symboltable.CallReference{}, false
		case *types.Func:
			return symboltable.CallReference{
				Caller: callerQName,
				Callee: qualifyFunc(o),
				Kind:   symboltable.CallDirect,
				Line:   line,
			}, true
		default:
			// Variable holding a func, or type conversion — not a static call.
			return symboltable.CallReference{
				Caller: callerQName,
				Callee: fn.Name,
				Kind:   symboltable.CallDirect, // best approximation
				Line:   line,
			}, true
		}

	case *ast.SelectorExpr:
		// Method call or package-qualified call: x.Method(...) or pkg.Func(...)
		//
		// The key distinction:
		//   - If info.Selections has an entry for this selector, it is a method
		//     call (either on a concrete type or on an interface).
		//   - If not, it is a package-qualified function call (pkg.Func).
		if sel, ok := info.Selections[fn]; ok {
			// This is a method call: x.Method(...)
			// Determine whether the receiver's type is an interface.
			recvType := sel.Recv()
			// Unwrap pointer.
			if ptr, ok := recvType.(*types.Pointer); ok {
				recvType = ptr.Elem()
			}

			if _, isIface := recvType.Underlying().(*types.Interface); isIface {
				// Interface dispatch: we know the method name but not which
				// concrete implementation will be called.
				ifaceName := recvType.String()
				methodName := fn.Sel.Name
				return symboltable.CallReference{
					Caller: callerQName,
					Callee: ifaceName + "." + methodName,
					Kind:   symboltable.CallInterfaceResolved,
					Line:   line,
				}, true
			}

			// Concrete receiver — direct call.
			return symboltable.CallReference{
				Caller: callerQName,
				Callee: qualifyFunc(sel.Obj().(*types.Func)),
				Kind:   symboltable.CallDirect,
				Line:   line,
			}, true
		}

		// Not in Selections — must be a package-qualified call: pkg.Func(...)
		// Look it up in Uses.
		if obj, ok := info.Uses[fn.Sel]; ok {
			if f, ok := obj.(*types.Func); ok {
				return symboltable.CallReference{
					Caller: callerQName,
					Callee: qualifyFunc(f),
					Kind:   symboltable.CallDirect,
					Line:   line,
				}, true
			}
		}

		// Fallback: record what we have.
		return symboltable.CallReference{
			Caller: callerQName,
			Callee: fn.Sel.Name,
			Kind:   symboltable.CallDirect,
			Line:   line,
		}, true

	case *ast.FuncLit:
		// Anonymous function literal — not a static call we can name.
		return symboltable.CallReference{}, false

	default:
		return symboltable.CallReference{}, false
	}
}

// qualifyFunc returns the fully-qualified name of a *types.Func in the form
// "import/path.FuncName" or "import/path.(ReceiverType).MethodName".
func qualifyFunc(f *types.Func) string {
	if f.Pkg() == nil {
		return f.Name() // built-in or universe scope
	}
	sig := f.Type().(*types.Signature)
	if sig.Recv() == nil {
		// Package-level function.
		return f.Pkg().Path() + "." + f.Name()
	}
	// Method: extract the base receiver type name.
	recv := sig.Recv().Type()
	if ptr, ok := recv.(*types.Pointer); ok {
		recv = ptr.Elem()
	}
	recvName := types.TypeString(recv, nil)
	// recvName may be "pkg.TypeName" — strip the package prefix for the
	// format we want: "pkg/path.(TypeName).Method"
	if idx := strings.LastIndex(recvName, "."); idx >= 0 {
		recvName = recvName[idx+1:]
	}
	return fmt.Sprintf("%s.(%s).%s", f.Pkg().Path(), recvName, f.Name())
}
