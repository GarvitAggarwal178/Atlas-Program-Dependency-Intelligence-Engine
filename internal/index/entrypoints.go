package index

import (
	"unicode"

	"github.com/yourorg/symex/internal/symboltable"
)

// DefaultEntryPoints implements architecture.md section 6's entry-point
// model: "Binary -> main.main and everything reachable. Library -> every
// exported top-level function across all packages, because you do not
// control the caller."
//
// Binary mode wins if any package literally named "main" has a function
// literally named "main" — the standard shape of a Go binary's entry
// point. Otherwise every exported (capitalized name) top-level function or
// method is treated as a potential caller, matching the library case.
func DefaultEntryPoints(table *symboltable.RepoSymbolTable) []string {
	for _, f := range table.Files {
		if f.Package != "main" {
			continue
		}
		for _, sym := range f.Defined {
			if sym.Kind == symboltable.KindFunction && sym.Name == "main" {
				return []string{sym.QualifiedName}
			}
		}
	}

	var entries []string
	for _, f := range table.Files {
		for _, sym := range f.Defined {
			if sym.Kind != symboltable.KindFunction && sym.Kind != symboltable.KindMethod {
				continue
			}
			if isExported(sym.Name) {
				entries = append(entries, sym.QualifiedName)
			}
		}
	}
	return entries
}

func isExported(name string) bool {
	if name == "" {
		return false
	}
	r := []rune(name)[0]
	return unicode.IsUpper(r)
}
