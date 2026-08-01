package parser_test

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/yourorg/symex/internal/parser"
)

func testdataDir(t *testing.T, name string) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root := filepath.Join(filepath.Dir(file), "..", "..", "testdata", name)
	abs, err := filepath.Abs(root)
	if err != nil {
		t.Fatalf("abs path: %v", err)
	}
	if _, err := os.Stat(abs); err != nil {
		t.Fatalf("fixture not found at %s: %v", abs, err)
	}
	return abs
}

// TestCheckPoison_CleanRepoPasses is the negative control: a repo that
// type-checks cleanly must never be gated out.
func TestCheckPoison_CleanRepoPasses(t *testing.T) {
	root := testdataDir(t, "fixture")
	pkgs, _, err := parser.LoadPackages(root)
	if err != nil {
		t.Fatalf("LoadPackages: %v", err)
	}
	result := parser.CheckPoison(pkgs)
	if !result.Clean {
		t.Fatalf("expected clean fixture to pass the poison gate, got Clean=false reason=%q detail=%q",
			result.Reason, result.Detail)
	}
}

// TestCheckPoison_TypeErrorIsCaught is the case architecture.md section 3.2
// exists to fix: go/packages.Load succeeds (does not itself error), but the
// package it returns has a genuine type error. The old ParseRepo path
// would silently derive facts from this anyway (see its doc comment); the
// gate must refuse it.
func TestCheckPoison_TypeErrorIsCaught(t *testing.T) {
	root := testdataDir(t, "broken_fixture")
	pkgs, _, err := parser.LoadPackages(root)
	if err != nil {
		t.Fatalf("LoadPackages: %v", err)
	}
	result := parser.CheckPoison(pkgs)
	if result.Clean {
		t.Fatal("expected broken_fixture to be caught by the poison gate, got Clean=true")
	}
	if result.Reason != parser.ReasonTypeErrors {
		t.Errorf("expected reason=%q, got %q (detail=%q)", parser.ReasonTypeErrors, result.Reason, result.Detail)
	}
	if result.Detail == "" {
		t.Error("expected non-empty detail describing the type error")
	}
}

// TestCheckPoison_NoPackagesLoadedIsCaught covers the case where
// packages.Load returns zero packages for "./..." — e.g. an unresolvable
// module. This must not be silently treated as "nothing to index" and
// silently pass the gate as vacuously clean.
func TestCheckPoison_NoPackagesLoadedIsCaught(t *testing.T) {
	result := parser.CheckPoison(nil)
	if result.Clean {
		t.Fatal("expected zero packages to be caught by the poison gate, got Clean=true")
	}
	if result.Reason != parser.ReasonModuleUnavailable {
		t.Errorf("expected reason=%q, got %q", parser.ReasonModuleUnavailable, result.Reason)
	}
}

// TestParseRepo_StillWarnAndContinueOnBrokenInput locks in that the legacy
// ParseRepo path (used by v2, frozen at tag v2-frozen) is unchanged by the
// LoadPackages refactor: it still extracts what it can rather than
// refusing, which is exactly the behavior CheckPoison exists to be used
// INSTEAD OF by any new, sound code path.
func TestParseRepo_StillWarnAndContinueOnBrokenInput(t *testing.T) {
	root := testdataDir(t, "broken_fixture")
	table, err := parser.ParseRepo(root, "github.com/example/broken")
	if err != nil {
		t.Fatalf("ParseRepo unexpectedly returned an error (it should warn-and-continue): %v", err)
	}
	if table == nil {
		t.Fatal("expected a non-nil table even for a broken package")
	}
}
