package parser_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yourorg/symex/internal/parser"
)

// TestCrossPackageInterfaceSatisfaction is a direct regression test for
// the bug found via architecture.md section 9.1's fixture 5: a concrete
// type's ImplementedInterfaces only ever included interfaces declared in
// its OWN package. A type in one package implementing an interface
// declared in a different package — io.Writer, http.Handler, and here, a
// hand-rolled example — was silently invisible, because the old
// collectPackageInterfacesFromPkg only scanned the current package's own
// pkg.Syntax. Fixed by collectAllInterfaces, built once across every
// loaded package.
func TestCrossPackageInterfaceSatisfaction(t *testing.T) {
	dir := t.TempDir()
	const mod = "example.com/crosspkg"

	files := map[string]string{
		"go.mod": "module " + mod + "\n\ngo 1.21\n",
		"iface/iface.go": `package iface

type Greeter interface {
	Greet() string
}
`,
		"impl/impl.go": `package impl

type English struct{}

func (e *English) Greet() string { return "hello" }
`,
	}
	for rel, content := range files {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}

	table, err := parser.ParseRepo(dir, mod)
	if err != nil {
		t.Fatalf("ParseRepo: %v", err)
	}

	var found bool
	var implementedInterfaces []string
	for _, f := range table.Files {
		for _, sym := range f.Defined {
			if sym.Name == "English" {
				found = true
				implementedInterfaces = sym.ImplementedInterfaces
			}
		}
	}
	if !found {
		t.Fatal("English type not found in parsed output")
	}

	wantIface := mod + "/iface.Greeter"
	hasIface := false
	for _, iface := range implementedInterfaces {
		if iface == wantIface {
			hasIface = true
		}
	}
	if !hasIface {
		t.Errorf("expected English.ImplementedInterfaces to include %q (cross-package interface satisfaction), got %v",
			wantIface, implementedInterfaces)
	}
}
