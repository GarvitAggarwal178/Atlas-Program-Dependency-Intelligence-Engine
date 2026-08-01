package index_test

import (
	"testing"

	"github.com/yourorg/symex/internal/index"
	"github.com/yourorg/symex/internal/parser"
)

func TestDefaultEntryPoints_BinaryMode(t *testing.T) {
	dir := t.TempDir()
	const mod = "example.com/binmode"
	writeModule(t, dir, map[string]string{
		"go.mod": "module " + mod + "\n\ngo 1.21\n",
		"main.go": `package main

func Helper() {}

func main() { Helper() }
`,
	})

	pkgs, fset, err := parser.LoadPackages(dir)
	if err != nil {
		t.Fatalf("LoadPackages: %v", err)
	}
	table := parser.BuildSymbolTable(pkgs, fset, dir, mod)

	got := index.DefaultEntryPoints(table)
	want := mod + ".main"
	if len(got) != 1 || got[0] != want {
		t.Errorf("expected exactly [%s] for a binary, got %v", want, got)
	}
}

func TestDefaultEntryPoints_LibraryMode(t *testing.T) {
	dir := t.TempDir()
	const mod = "example.com/libmode"
	writeModule(t, dir, map[string]string{
		"go.mod": "module " + mod + "\n\ngo 1.21\n",
		"lib.go": `package lib

func Exported() { unexported() }

func unexported() {}
`,
	})

	pkgs, fset, err := parser.LoadPackages(dir)
	if err != nil {
		t.Fatalf("LoadPackages: %v", err)
	}
	table := parser.BuildSymbolTable(pkgs, fset, dir, mod)

	got := index.DefaultEntryPoints(table)
	foundExported, foundUnexported := false, false
	for _, e := range got {
		if e == mod+"/lib.Exported" || e == mod+".Exported" {
			foundExported = true
		}
		if e == mod+"/lib.unexported" || e == mod+".unexported" {
			foundUnexported = true
		}
	}
	if !foundExported {
		t.Errorf("expected Exported to be an entry point in library mode, got %v", got)
	}
	if foundUnexported {
		t.Errorf("unexported must NOT be an entry point, got %v", got)
	}
}
