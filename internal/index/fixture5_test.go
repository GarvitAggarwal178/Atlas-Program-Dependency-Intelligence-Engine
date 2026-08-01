package index_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/yourorg/symex/internal/index"
	"github.com/yourorg/symex/internal/store"
)

// TestFixture5_MoveTypeAcrossPackageBoundary is section 9.1's fixture 5:
// "move a type across a package boundary." A concrete type implementing a
// dispatched interface moves from one package (different import path) to
// another, in one commit. The dispatch edge must end up pointing at the
// NEW import path, and the old one must close -- not both being live
// simultaneously, and not the edge silently vanishing.
func TestFixture5_MoveTypeAcrossPackageBoundary(t *testing.T) {
	db := openIndexTestDB(t)
	repo := uniqueIndexTestRepo(t)
	dir := t.TempDir()
	const mod = "example.com/fixture5"

	writeModule(t, dir, map[string]string{
		"go.mod": "module " + mod + "\n\ngo 1.21\n",
		"iface/iface.go": `package iface

type Greeter interface {
	Greet() string
}
`,
		"foo/foo.go": `package foo

type Impl struct{}

func (i *Impl) Greet() string { return "hi" }
`,
		"main.go": `package main

import (
	"` + mod + `/foo"
	"` + mod + `/iface"
)

func UseGreeter(g iface.Greeter) string { return g.Greet() }

func main() {
	UseGreeter(&foo.Impl{})
}
`,
	})

	if _, err := index.IndexCommitFromRepo(context.Background(), db, dir, mod, repo, 0, "fp-0"); err != nil {
		t.Fatalf("index seq=0: %v", err)
	}

	before := liveEdgeSet(t, db, repo)
	oldEdge := mod + ".UseGreeter -(interface_resolved)-> " + mod + "/foo.(Impl).Greet"
	if !before[oldEdge] {
		t.Fatalf("seq=0: expected %s to be live, got %v", oldEdge, before)
	}

	// Move Impl from foo to bar: delete foo/foo.go, add bar/bar.go with
	// the same content under a new package name, update main.go's import
	// and constructor call to match.
	if err := os.Remove(filepath.Join(dir, "foo", "foo.go")); err != nil {
		t.Fatalf("remove foo.go: %v", err)
	}
	// also remove the now-empty foo dir so go/packages doesn't see a
	// package directory with zero files (harmless either way, but keeps
	// the fixture honest about what "moved" means)
	_ = os.Remove(filepath.Join(dir, "foo"))

	writeModule(t, dir, map[string]string{
		"bar/bar.go": `package bar

type Impl struct{}

func (i *Impl) Greet() string { return "hi" }
`,
		"main.go": `package main

import (
	"` + mod + `/bar"
	"` + mod + `/iface"
)

func UseGreeter(g iface.Greeter) string { return g.Greet() }

func main() {
	UseGreeter(&bar.Impl{})
}
`,
	})

	if _, err := index.IndexCommitFromRepo(context.Background(), db, dir, mod, repo, 1, "fp-1"); err != nil {
		t.Fatalf("index seq=1: %v", err)
	}

	after := liveEdgeSet(t, db, repo)
	newEdge := mod + ".UseGreeter -(interface_resolved)-> " + mod + "/bar.(Impl).Greet"

	if after[oldEdge] {
		t.Errorf("seq=1: OLD edge (foo.(Impl).Greet) must be withdrawn after the type moved, still found in %v", after)
	}
	if !after[newEdge] {
		t.Errorf("seq=1: expected NEW edge %s after the type moved to bar, got %v", newEdge, after)
	}

	// Double check at the fact level: no live fact should still reference
	// the foo import path at all.
	live, err := store.QueryLiveFacts(context.Background(), db.RawDB(), repo)
	if err != nil {
		t.Fatalf("QueryLiveFacts: %v", err)
	}
	for _, f := range live {
		if f.SourceFile == "foo/foo.go" {
			t.Errorf("found a live fact still sourced from the deleted foo/foo.go: %+v", f)
		}
	}
}
