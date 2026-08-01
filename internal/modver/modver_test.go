package modver_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yourorg/symex/internal/modver"
)

func TestParseRequires_Basic(t *testing.T) {
	dir := t.TempDir()
	content := `module example.com/test

go 1.21

require (
	github.com/foo/bar v1.2.3
	github.com/baz/qux v0.1.0
)
`
	path := filepath.Join(dir, "go.mod")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}

	got, err := modver.ParseRequires(path)
	if err != nil {
		t.Fatalf("ParseRequires: %v", err)
	}

	want := map[string]string{
		"github.com/foo/bar": "v1.2.3",
		"github.com/baz/qux": "v0.1.0",
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for mod, ver := range want {
		if got[mod] != ver {
			t.Errorf("%s: got version %q, want %q", mod, got[mod], ver)
		}
	}
}

func TestParseRequires_DetectsVersionBump(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "go.mod")

	v1 := `module example.com/test

go 1.21

require github.com/foo/bar v1.2.3
`
	if err := os.WriteFile(path, []byte(v1), 0644); err != nil {
		t.Fatalf("write go.mod v1: %v", err)
	}
	before, err := modver.ParseRequires(path)
	if err != nil {
		t.Fatalf("ParseRequires v1: %v", err)
	}
	if before["github.com/foo/bar"] != "v1.2.3" {
		t.Fatalf("expected v1.2.3, got %q", before["github.com/foo/bar"])
	}

	v2 := `module example.com/test

go 1.21

require github.com/foo/bar v1.3.0
`
	if err := os.WriteFile(path, []byte(v2), 0644); err != nil {
		t.Fatalf("write go.mod v2: %v", err)
	}
	after, err := modver.ParseRequires(path)
	if err != nil {
		t.Fatalf("ParseRequires v2: %v", err)
	}
	if after["github.com/foo/bar"] != "v1.3.0" {
		t.Fatalf("expected v1.3.0, got %q", after["github.com/foo/bar"])
	}
	if before["github.com/foo/bar"] == after["github.com/foo/bar"] {
		t.Fatal("test setup error: versions should differ")
	}
}

// TestParseRequires_RealGoMod is a sanity check against this actual
// project's own go.mod, not just synthetic content.
func TestParseRequires_RealGoMod(t *testing.T) {
	got, err := modver.ParseRequires("../../go.mod")
	if err != nil {
		t.Fatalf("ParseRequires: %v", err)
	}
	if _, ok := got["golang.org/x/tools"]; !ok {
		t.Errorf("expected golang.org/x/tools in this project's own go.mod requires, got %v", got)
	}
	if _, ok := got["github.com/lib/pq"]; !ok {
		t.Errorf("expected github.com/lib/pq in this project's own go.mod requires, got %v", got)
	}
}
