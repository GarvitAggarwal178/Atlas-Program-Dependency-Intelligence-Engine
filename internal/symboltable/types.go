// Package symboltable defines the data model produced by stage-1 AST analysis.
// Nothing in this package does I/O or type-checking; it is pure data.
package symboltable

// SymbolKind identifies what a symbol declaration is.
type SymbolKind string

const (
	KindFunction  SymbolKind = "function"
	KindMethod    SymbolKind = "method"
	KindInterface SymbolKind = "interface"
	KindType      SymbolKind = "type"
)

// CallKind distinguishes how a call target was resolved.
//
// direct_call   — the callee is a concrete function or method on a concrete
//                 (non-interface) type. go/types resolved it to exactly one
//                 *types.Func. This is precise.
//
// interface_resolved — the callee is a method invoked on a value whose static
//                 type is an interface. We cannot determine the concrete
//                 implementation at static-analysis time without a full points-
//                 to / escape analysis. In later stages the call graph builder
//                 will over-approximate this by adding edges to every concrete
//                 type in the repo that satisfies the interface — a known,
//                 intentional soundness-over-precision tradeoff documented here
//                 so it is never mistaken for a gap.
type CallKind string

const (
	CallDirect            CallKind = "direct_call"
	CallInterfaceResolved CallKind = "interface_resolved"
)

// LineRange is a 1-based inclusive line range inside a source file.
type LineRange struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

// DefinedSymbol is one declaration extracted from a file.
type DefinedSymbol struct {
	// Name is the unqualified identifier as written in source.
	Name string `json:"name"`

	// QualifiedName is "pkg.Name" for functions/types, "pkg.(ReceiverType).Name"
	// for methods. This is the canonical node key used by the call graph.
	QualifiedName string `json:"qualified_name"`

	Kind SymbolKind `json:"kind"`

	// Package is the Go package name (not import path) declared in the file.
	Package string `json:"package"`

	// ReceiverType is non-empty only when Kind == KindMethod.
	// It is the base type name, without pointer star, e.g. "Handler".
	ReceiverType string `json:"receiver_type,omitempty"`

	// Line range of the entire declaration (including body for
	// functions/methods, or the full type block for types/interfaces).
	Lines LineRange `json:"lines"`

	// CanonicalHash is a SHA-256 hex digest of the canonicalized function/
	// method body (comments stripped, local identifiers alpha-renamed to
	// positional placeholders _v0, _v1, …). Empty for interface and type
	// symbols that have no executable body.
	//
	// Two functions with identical CanonicalHash are structurally equivalent
	// modulo local naming and comments — a diff that changes only those things
	// is classified as "trivial" and produces zero blast radius.
	CanonicalHash string `json:"canonical_hash,omitempty"`

	// MethodSet is non-empty only when Kind == KindInterface. It lists the
	// method signatures as strings.
	MethodSet []string `json:"method_set,omitempty"`

	// ImplementedInterfaces lists interface names (qualified) that this
	// concrete type satisfies, computed via go/types. Empty unless
	// Kind == KindType.
	ImplementedInterfaces []string `json:"implemented_interfaces,omitempty"`
}

// CallReference is a single call expression found inside a function/method body.
type CallReference struct {
	// Caller is the QualifiedName of the enclosing function/method.
	Caller string `json:"caller"`

	// Callee is the best name we can produce for the call target.
	// For direct calls this is the fully-qualified name resolved by go/types.
	// For interface calls it is "InterfaceType.MethodName".
	Callee string `json:"callee"`

	// Kind states whether we resolved this to a single concrete target
	// (direct_call) or only to an interface method (interface_resolved).
	Kind CallKind `json:"kind"`

	// Line is the line number of the call expression in the file.
	Line int `json:"line"`
}

// FileSymbolTable is the complete output for one source file.
type FileSymbolTable struct {
	// FilePath is the path as supplied to the parser (repo-relative or absolute).
	FilePath string `json:"file_path"`

	// Package is the package name declared in this file.
	Package string `json:"package"`

	// ImportPath is the fully-qualified import path derived from the module root.
	ImportPath string `json:"import_path"`

	// Defined holds every symbol declared in this file.
	Defined []DefinedSymbol `json:"defined"`

	// References holds every call expression found in this file's function bodies.
	References []CallReference `json:"references"`
}

// RepoSymbolTable is the aggregate output across an entire repository.
type RepoSymbolTable struct {
	// ModulePath is the module path declared in go.mod.
	ModulePath string `json:"module_path"`

	// Files is one entry per parsed .go file (test files included).
	Files []FileSymbolTable `json:"files"`
}
