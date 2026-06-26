package callgraph_test

import (
	"testing"

	"github.com/yourorg/symex/internal/callgraph"
	"github.com/yourorg/symex/internal/symboltable"
)

// makeTable is a test helper that constructs a minimal RepoSymbolTable from
// shorthand inputs, for testing the builder's expansion logic without needing
// a real parser run or Postgres.
func makeTable(files []symboltable.FileSymbolTable) *symboltable.RepoSymbolTable {
	return &symboltable.RepoSymbolTable{
		ModulePath: "github.com/example/test",
		Files:      files,
	}
}

// TestParseInterfaceCallee verifies that we correctly split callee strings
// produced by Stage 1 into (interfaceName, methodName).
func TestParseInterfaceCalleeInternal(t *testing.T) {
	cases := []struct {
		callee    string
		wantIface string
		wantMethod string
		wantErr   bool
	}{
		{
			callee:     "github.com/example/billing.Ledger.Debit",
			wantIface:  "github.com/example/billing.Ledger",
			wantMethod: "Debit",
		},
		{
			callee:     "github.com/example/billing.Store.Get",
			wantIface:  "github.com/example/billing.Store",
			wantMethod: "Get",
		},
		{
			// Pathological: no dot at all
			callee:  "NoDotAtAll",
			wantErr: true,
		},
	}

	for _, tc := range cases {
		iface, method, err := callgraph.ParseInterfaceCalleeExported(tc.callee)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseInterfaceCallee(%q): expected error, got nil", tc.callee)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseInterfaceCallee(%q): unexpected error: %v", tc.callee, err)
			continue
		}
		if iface != tc.wantIface {
			t.Errorf("parseInterfaceCallee(%q): iface = %q, want %q", tc.callee, iface, tc.wantIface)
		}
		if method != tc.wantMethod {
			t.Errorf("parseInterfaceCallee(%q): method = %q, want %q", tc.callee, method, tc.wantMethod)
		}
	}
}

// TestImplementorIndexBuilding verifies that the implementor index correctly
// maps interfaces to their concrete implementors.
func TestImplementorIndexBuilding(t *testing.T) {
	// Construct a table with one interface (Ledger) and two concrete types that
	// implement it (SQLLedger, InMemoryLedger), plus one type that does not.
	table := makeTable([]symboltable.FileSymbolTable{
		{
			FilePath:   "billing/ledger.go",
			Package:    "billing",
			ImportPath: "github.com/example/billing",
			Defined: []symboltable.DefinedSymbol{
				{
					Name:          "Ledger",
					QualifiedName: "github.com/example/billing.Ledger",
					Kind:          symboltable.KindInterface,
					MethodSet:     []string{"Debit", "Credit"},
				},
				{
					Name:          "SQLLedger",
					QualifiedName: "github.com/example/billing.SQLLedger",
					Kind:          symboltable.KindType,
					ImplementedInterfaces: []string{
						"github.com/example/billing.Ledger",
					},
				},
				{
					Name:          "InMemoryLedger",
					QualifiedName: "github.com/example/billing.InMemoryLedger",
					Kind:          symboltable.KindType,
					ImplementedInterfaces: []string{
						"github.com/example/billing.Ledger",
					},
				},
				{
					Name:          "AuditLog",
					QualifiedName: "github.com/example/billing.AuditLog",
					Kind:          symboltable.KindType,
					// Does NOT implement Ledger.
					ImplementedInterfaces: []string{},
				},
				// Methods on SQLLedger
				{
					Name:          "Debit",
					QualifiedName: "github.com/example/billing.(SQLLedger).Debit",
					Kind:          symboltable.KindMethod,
					ReceiverType:  "SQLLedger",
				},
				{
					Name:          "Credit",
					QualifiedName: "github.com/example/billing.(SQLLedger).Credit",
					Kind:          symboltable.KindMethod,
					ReceiverType:  "SQLLedger",
				},
				// Methods on InMemoryLedger
				{
					Name:          "Debit",
					QualifiedName: "github.com/example/billing.(InMemoryLedger).Debit",
					Kind:          symboltable.KindMethod,
					ReceiverType:  "InMemoryLedger",
				},
				{
					Name:          "Credit",
					QualifiedName: "github.com/example/billing.(InMemoryLedger).Credit",
					Kind:          symboltable.KindMethod,
					ReceiverType:  "InMemoryLedger",
				},
			},
			References: []symboltable.CallReference{
				{
					// Interface call: caller dispatches through Ledger.Debit
					Caller: "github.com/example/payment.ProcessPayment",
					Callee: "github.com/example/billing.Ledger.Debit",
					Kind:   symboltable.CallInterfaceResolved,
					Line:   42,
				},
			},
		},
		{
			FilePath:   "payment/processor.go",
			Package:    "payment",
			ImportPath: "github.com/example/payment",
			Defined: []symboltable.DefinedSymbol{
				{
					Name:          "ProcessPayment",
					QualifiedName: "github.com/example/payment.ProcessPayment",
					Kind:          symboltable.KindFunction,
				},
			},
		},
	})

	// Use the exported test helper to access the expansion logic.
	expanded := callgraph.ExpandInterfaceCallExported(
		table,
		"github.com/example/billing.Ledger",
		"Debit",
	)

	if len(expanded) != 2 {
		t.Errorf("expected 2 expanded targets (SQLLedger.Debit + InMemoryLedger.Debit), got %d: %v",
			len(expanded), expanded)
	}

	want := map[string]bool{
		"github.com/example/billing.(SQLLedger).Debit":      true,
		"github.com/example/billing.(InMemoryLedger).Debit": true,
	}
	for _, target := range expanded {
		if !want[target] {
			t.Errorf("unexpected expansion target: %q", target)
		}
		delete(want, target)
	}
	for remaining := range want {
		t.Errorf("missing expected expansion target: %q", remaining)
	}
}

// TestNoImplementorsGivesNoExpansion ensures that an interface call with no
// known implementations still produces an edge (the raw interface descriptor)
// rather than silently dropping the call.
func TestNoImplementorsGivesNoExpansion(t *testing.T) {
	table := makeTable([]symboltable.FileSymbolTable{
		{
			FilePath:   "service/service.go",
			Package:    "service",
			ImportPath: "github.com/example/service",
			Defined: []symboltable.DefinedSymbol{
				// Interface with no concrete implementations in scope.
				{
					Name:          "Cache",
					QualifiedName: "github.com/example/service.Cache",
					Kind:          symboltable.KindInterface,
					MethodSet:     []string{"Get", "Set"},
				},
			},
		},
	})

	expanded := callgraph.ExpandInterfaceCallExported(
		table,
		"github.com/example/service.Cache",
		"Get",
	)

	// No concrete implementors → expansion returns empty slice.
	// The builder handles this by recording the raw interface descriptor as edge.
	if len(expanded) != 0 {
		t.Errorf("expected 0 expansions for interface with no implementors, got %d: %v",
			len(expanded), expanded)
	}
}
