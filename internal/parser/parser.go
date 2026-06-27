// Package parser implements stage-1 of the incremental call graph engine:
// AST parsing, symbol extraction, and call-reference classification.
//
// It uses golang.org/x/tools/go/packages for module-aware loading so that
// types.Info.Selections is fully populated across local module boundaries
// (e.g. github.com/example/fixture/billing → github.com/example/fixture/payment).
// The importer.Default() approach only resolves stdlib and compiled packages;
// it cannot resolve local module imports, leaving Selections incomplete and
// causing all cross-package method calls to be missed as interface_resolved.
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
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/tools/go/packages"

	"github.com/yourorg/symex/internal/canonicalize"
	"github.com/yourorg/symex/internal/symboltable"
)

// ParseRepo loads every package under repoRoot using go/packages (module-aware),
// type-checks them all together so cross-package Selections are populated,
// and returns the aggregated RepoSymbolTable.
//
// repoRoot must be the directory containing go.mod.
// modulePath must match the module directive in go.mod.
func ParseRepo(repoRoot, modulePath string) (*symboltable.RepoSymbolTable, error) {
	cfg := &packages.Config{
		Mode: packages.NeedName |
			packages.NeedFiles |
			packages.NeedCompiledGoFiles |
			packages.NeedImports |
			packages.NeedTypes |
			packages.NeedSyntax |
			packages.NeedTypesInfo |
			packages.NeedTypesSizes,
		Dir:  repoRoot,
		Fset: token.NewFileSet(),
		// ParseFile with ParseComments so the canonicalizer can strip them.
		ParseFile: func(fset *token.FileSet, filename string, src []byte) (*ast.File, error) {
			return parseFileWithComments(fset, filename, src)
		},
	}

	// "./..." loads all packages in the module rooted at cfg.Dir.
	pkgs, err := packages.Load(cfg, "./...")
	if err != nil {
		return nil, fmt.Errorf("packages.Load: %w", err)
	}

	result := &symboltable.RepoSymbolTable{
		ModulePath: modulePath,
	}

	for _, pkg := range pkgs {
		// Log load errors per-package but continue — partial type info is
		// still useful for packages that do load cleanly.
		for _, e := range pkg.Errors {
			fmt.Fprintf(os.Stderr, "warn: %s: %v\n", pkg.PkgPath, e)
		}

		files, err := extractPackageSymbols(pkg, repoRoot, cfg.Fset)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warn: extracting %s: %v\n", pkg.PkgPath, err)
			continue
		}
		result.Files = append(result.Files, files...)
	}

	return result, nil
}

// parseFileWithComments is the ParseFile hook passed to packages.Config.
// It parses with ParseComments so comment nodes survive into the AST,
// allowing the canonicalizer to strip them during hashing.
//
// Note: our package is named "parser" which shadows go/parser, so we
// delegate to parseGoFile (in parse_file.go) which imports go/parser
// under an alias.
func parseFileWithComments(fset *token.FileSet, filename string, src []byte) (*ast.File, error) {
	return parseGoFile(fset, filename, src)
}

// extractPackageSymbols converts one loaded *packages.Package into
// FileSymbolTable entries, one per source file.
func extractPackageSymbols(
	pkg *packages.Package,
	repoRoot string,
	fset *token.FileSet,
) ([]symboltable.FileSymbolTable, error) {
	if pkg.TypesInfo == nil {
		// Package failed to type-check entirely; skip.
		return nil, nil
	}

	info := pkg.TypesInfo
	importPath := pkg.PkgPath

	// Build the per-package interface map for implementor detection.
	// We need to do this across all files in the package.
	pkgInterfaces := collectPackageInterfacesFromPkg(pkg, info)

	var result []symboltable.FileSymbolTable

	for _, f := range pkg.Syntax {
		pos := fset.Position(f.Pos())
		absPath := pos.Filename
		if absPath == "" {
			continue
		}

		relPath, err := filepath.Rel(repoRoot, absPath)
		if err != nil || strings.HasPrefix(relPath, "..") {
			// File is outside repoRoot (e.g. a dependency in the module cache).
			continue
		}
		relPath = filepath.ToSlash(relPath)

		pkgName := pkg.Name

		fst := symboltable.FileSymbolTable{
			FilePath:   relPath,
			Package:    pkgName,
			ImportPath: importPath,
		}

		fst.Defined = extractDefinedSymbols(f, fset, info, pkgName, importPath, pkgInterfaces)
		fst.References = extractCallReferences(f, fset, info, pkgName, importPath)

		result = append(result, fst)
	}

	return result, nil
}

// collectPackageInterfacesFromPkg builds the interface name → *types.Interface
// map from a fully type-checked *packages.Package.
func collectPackageInterfacesFromPkg(
	pkg *packages.Package,
	info *types.Info,
) map[string]*types.Interface {
	return collectPackageInterfaces(pkg.Syntax, info)
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
// go/types.Info.Selections contains three kinds of selector expressions:
//
//   - types.MethodVal  — x.Method() where x has a concrete or interface type.
//     Obj() is *types.Func.
//   - types.MethodExpr — T.Method or (*T).Method used as a value.
//     Obj() is *types.Func.
//   - types.FieldVal   — x.field where field is a struct field.
//     Obj() is *types.Var. If the field has a func type and is then called
//     as x.field(...), this appears as a CallExpr with a SelectorExpr whose
//     Selection is FieldVal. This is NOT a static method dispatch.
//
// The original code assumed Selections always yields *types.Func, causing a
// panic on FieldVal selections. We now check sel.Kind() explicitly.
//
// Returns (ref, false) when the call cannot be classified meaningfully
// (e.g. a built-in, a func-typed variable call, or an anonymous literal).
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
		obj, ok := info.Uses[fn]
		if !ok {
			return symboltable.CallReference{}, false
		}
		switch o := obj.(type) {
		case *types.Builtin:
			// make, len, cap, new, etc. — not a user-defined call.
			return symboltable.CallReference{}, false
		case *types.Func:
			return symboltable.CallReference{
				Caller: callerQName,
				Callee: qualifyFunc(o),
				Kind:   symboltable.CallDirect,
				Line:   line,
			}, true
		case *types.TypeName:
			// Type conversion: T(x) — not a function call.
			return symboltable.CallReference{}, false
		default:
			// *types.Var: a func-typed variable called as f().
			// We cannot statically resolve which function this is.
			// Record it as a direct call with the variable name as a best-effort
			// callee (no concrete target, but we don't drop it entirely so the
			// caller's outgoing edges aren't silently empty).
			_ = o
			return symboltable.CallReference{
				Caller: callerQName,
				Callee: fn.Name,
				Kind:   symboltable.CallDirect,
				Line:   line,
			}, true
		}

	case *ast.SelectorExpr:
		// x.Method(...) or pkg.Func(...)
		//
		// Check info.Selections first. A hit means this is a field or method
		// access on a value (not a package-qualified name).
		if sel, ok := info.Selections[fn]; ok {
			switch sel.Kind() {
			case types.MethodVal, types.MethodExpr:
				// Static method dispatch. Obj() is guaranteed *types.Func here.
				fn_obj := sel.Obj().(*types.Func) // safe: MethodVal/MethodExpr always have *types.Func

				// Determine whether the receiver's static type is an interface.
				recvType := sel.Recv()
				if ptr, ok := recvType.(*types.Pointer); ok {
					recvType = ptr.Elem()
				}

				if _, isIface := recvType.Underlying().(*types.Interface); isIface {
					// Interface dispatch — over-approximate to all implementations.
					return symboltable.CallReference{
						Caller: callerQName,
						Callee: recvType.String() + "." + fn.Sel.Name,
						Kind:   symboltable.CallInterfaceResolved,
						Line:   line,
					}, true
				}

				// Concrete receiver — precise direct call.
				return symboltable.CallReference{
					Caller: callerQName,
					Callee: qualifyFunc(fn_obj),
					Kind:   symboltable.CallDirect,
					Line:   line,
				}, true

			case types.FieldVal:
				// x.field(...) where field is a func-typed struct field.
				// This is a dynamic dispatch through a stored function value,
				// not a static method call. We cannot resolve the target
				// statically, so we skip it rather than record a wrong edge.
				// Concretely: chi uses patterns like mux.handler(w, r) where
				// handler is a field of type http.HandlerFunc.
				return symboltable.CallReference{}, false
			}
		}

		// Not in Selections → package-qualified call: pkg.Func(...)
		// Uses[fn.Sel] resolves the identifier to the package-level object.
		if obj, ok := info.Uses[fn.Sel]; ok {
			switch o := obj.(type) {
			case *types.Func:
				return symboltable.CallReference{
					Caller: callerQName,
					Callee: qualifyFunc(o),
					Kind:   symboltable.CallDirect,
					Line:   line,
				}, true
			case *types.TypeName:
				// pkg.Type(x) — type conversion, not a call.
				return symboltable.CallReference{}, false
			case *types.Builtin:
				return symboltable.CallReference{}, false
			default:
				// Func-typed variable accessed via package path — rare, skip.
				_ = o
				return symboltable.CallReference{}, false
			}
		}

		// Completely unresolved selector — record best-effort.
		return symboltable.CallReference{
			Caller: callerQName,
			Callee: fn.Sel.Name,
			Kind:   symboltable.CallDirect,
			Line:   line,
		}, true

	case *ast.FuncLit:
		// Anonymous function literal invoked immediately: func(){...}()
		// No static callee to record.
		return symboltable.CallReference{}, false

	default:
		// IndexExpr (generic instantiation calls), ParenExpr, etc.
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
	// recvName may be "pkg.TypeName" — strip the package prefix.
	if idx := strings.LastIndex(recvName, "."); idx >= 0 {
		recvName = recvName[idx+1:]
	}
	return fmt.Sprintf("%s.(%s).%s", f.Pkg().Path(), recvName, f.Name())
}
