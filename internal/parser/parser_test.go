package parser_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/yourorg/symex/internal/parser"
	"github.com/yourorg/symex/internal/symboltable"
)

// fixtureRoot returns the absolute path to testdata/fixture.
func fixtureRoot(t *testing.T) string {
	t.Helper()
	// __FILE__ is internal/parser/parser_test.go — go up two levels.
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// internal/parser → project root
	root := filepath.Join(filepath.Dir(file), "..", "..", "testdata", "fixture")
	abs, err := filepath.Abs(root)
	if err != nil {
		t.Fatalf("abs path: %v", err)
	}
	if _, err := os.Stat(abs); err != nil {
		t.Fatalf("fixture not found at %s: %v", abs, err)
	}
	return abs
}

func TestParseFixture(t *testing.T) {
	root := fixtureRoot(t)
	table, err := parser.ParseRepo(root, "github.com/example/fixture")
	if err != nil {
		t.Fatalf("ParseRepo: %v", err)
	}

	if len(table.Files) == 0 {
		t.Fatal("expected at least one file in output")
	}

	// Dump JSON to a temp file for inspection.
	out, _ := json.MarshalIndent(table, "", "  ")
	tmpFile := filepath.Join(t.TempDir(), "symtable.json")
	os.WriteFile(tmpFile, out, 0644)
	t.Logf("symbol table written to: %s", tmpFile)

	// --- Structural assertions ---

	// 1. The Ledger interface must be found with 3 methods.
	ledger := findDefinedSymbol(table, "github.com/example/fixture/billing.Ledger")
	if ledger == nil {
		t.Error("billing.Ledger interface not found in symbol table")
	} else {
		if ledger.Kind != symboltable.KindInterface {
			t.Errorf("billing.Ledger: expected kind=interface, got %s", ledger.Kind)
		}
		if len(ledger.MethodSet) != 3 {
			t.Errorf("billing.Ledger: expected 3 methods, got %d: %v", len(ledger.MethodSet), ledger.MethodSet)
		}
	}

	// 2. SQLLedger and InMemoryLedger must be found as types that implement Ledger.
	sqlLedger := findDefinedSymbol(table, "github.com/example/fixture/billing.SQLLedger")
	if sqlLedger == nil {
		t.Error("billing.SQLLedger type not found")
	} else {
		if sqlLedger.Kind != symboltable.KindType {
			t.Errorf("billing.SQLLedger: expected kind=type, got %s", sqlLedger.Kind)
		}
		if !containsString(sqlLedger.ImplementedInterfaces, "github.com/example/fixture/billing.Ledger") {
			t.Errorf("billing.SQLLedger: expected to implement billing.Ledger, got: %v",
				sqlLedger.ImplementedInterfaces)
		}
	}

	// 3. ProcessPayment method calls must be classified as interface_resolved.
	ifaceCalls := findReferencesByKind(table, symboltable.CallInterfaceResolved)
	if len(ifaceCalls) == 0 {
		t.Error("expected at least one interface_resolved call, found none")
	}
	t.Logf("interface_resolved calls: %d", len(ifaceCalls))
	for _, ref := range ifaceCalls {
		t.Logf("  %s -> %s (line %d)", ref.Caller, ref.Callee, ref.Line)
	}

	// 4. Direct calls must be found (NewSQLLedger, formatAmount, etc.)
	directCalls := findReferencesByKind(table, symboltable.CallDirect)
	if len(directCalls) == 0 {
		t.Error("expected at least one direct_call, found none")
	}
	t.Logf("direct_call calls: %d", len(directCalls))

	// 5. Canonical hash invariants on the store.TrivialRenameExample* pair.
	e1 := findDefinedSymbol(table, "github.com/example/fixture/store.TrivialRenameExample")
	e2 := findDefinedSymbol(table, "github.com/example/fixture/store.TrivialRenameExample2")
	e3 := findDefinedSymbol(table, "github.com/example/fixture/store.LogicChangeExample")

	if e1 == nil || e2 == nil || e3 == nil {
		t.Errorf("could not find all three TrivialRename*/LogicChange* symbols — "+
			"e1=%v e2=%v e3=%v", e1 != nil, e2 != nil, e3 != nil)
	} else {
		if e1.CanonicalHash == "" {
			t.Error("TrivialRenameExample: canonical hash is empty")
		}
		if e1.CanonicalHash != e2.CanonicalHash {
			t.Errorf("TrivialRenameExample and TrivialRenameExample2 should have equal hashes"+
				" (pure rename); got:\n  e1=%s\n  e2=%s", e1.CanonicalHash, e2.CanonicalHash)
		}
		if e1.CanonicalHash == e3.CanonicalHash {
			t.Errorf("TrivialRenameExample and LogicChangeExample should have DIFFERENT hashes"+
				" (* vs +); both gave: %s", e1.CanonicalHash)
		}
		t.Logf("hash(TrivialRenameExample)  = %s", e1.CanonicalHash)
		t.Logf("hash(TrivialRenameExample2) = %s", e2.CanonicalHash)
		t.Logf("hash(LogicChangeExample)    = %s", e3.CanonicalHash)
	}

	// 6. Line ranges must be non-zero for all defined functions.
	for _, f := range table.Files {
		for _, sym := range f.Defined {
			if sym.Kind == symboltable.KindFunction || sym.Kind == symboltable.KindMethod {
				if sym.Lines.Start == 0 || sym.Lines.End == 0 {
					t.Errorf("%s: line range is zero (%v)", sym.QualifiedName, sym.Lines)
				}
				if sym.Lines.Start > sym.Lines.End {
					t.Errorf("%s: start line %d > end line %d",
						sym.QualifiedName, sym.Lines.Start, sym.Lines.End)
				}
			}
		}
	}
}

// --- helpers ---

func findDefinedSymbol(table *symboltable.RepoSymbolTable, qname string) *symboltable.DefinedSymbol {
	for i := range table.Files {
		for j := range table.Files[i].Defined {
			if table.Files[i].Defined[j].QualifiedName == qname {
				return &table.Files[i].Defined[j]
			}
		}
	}
	return nil
}

func findReferencesByKind(table *symboltable.RepoSymbolTable, kind symboltable.CallKind) []symboltable.CallReference {
	var result []symboltable.CallReference
	for _, f := range table.Files {
		for _, ref := range f.References {
			if ref.Kind == kind {
				result = append(result, ref)
			}
		}
	}
	return result
}

func containsString(slice []string, s string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}
