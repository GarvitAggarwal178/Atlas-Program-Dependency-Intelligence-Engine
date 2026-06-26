package differ_test

import (
	"testing"

	"github.com/yourorg/symex/internal/differ"
	"github.com/yourorg/symex/internal/store"
)

// rawDiff is a minimal unified diff patch for testing, touching two Go files.
// billing/ledger.go: hunk at lines 31-33 (inside Debit method, line 30-35)
// payment/processor.go: hunk at lines 60-60 (inside GetBalance method)
const rawDiff = `diff --git a/billing/ledger.go b/billing/ledger.go
index abc1234..def5678 100644
--- a/billing/ledger.go
+++ b/billing/ledger.go
@@ -31,3 +31,4 @@
 	if amount > l.balance {
-		return fmt.Errorf("insufficient funds: have %d, need %d", l.balance, amount)
+		return fmt.Errorf("insufficient funds: have %d need %d", l.balance, amount)
 	}
+	// added comment
 	l.balance -= amount
diff --git a/payment/processor.go b/payment/processor.go
index 1111111..2222222 100644
--- a/payment/processor.go
+++ b/payment/processor.go
@@ -60,1 +60,1 @@
-	return p.ledger.Balance()
+	return p.ledger.Balance() + 0
diff --git a/README.md b/README.md
index 3333333..4444444 100644
--- a/README.md
+++ b/README.md
@@ -1,1 +1,1 @@
-# Old Title
+# New Title
`

func TestFromPatchParsesGoFilesOnly(t *testing.T) {
	diff, err := differ.FromPatch([]byte(rawDiff), "abc", "def")
	if err != nil {
		t.Fatalf("FromPatch: %v", err)
	}

	// README.md must be excluded — only .go files matter.
	if len(diff.Files) != 2 {
		t.Errorf("expected 2 Go files, got %d: %v", len(diff.Files),
			func() []string {
				var ps []string
				for _, f := range diff.Files {
					ps = append(ps, f.Path)
				}
				return ps
			}())
	}
}

func TestHunkRangesAreParsedCorrectly(t *testing.T) {
	diff, err := differ.FromPatch([]byte(rawDiff), "abc", "def")
	if err != nil {
		t.Fatalf("FromPatch: %v", err)
	}

	// billing/ledger.go should have hunk starting at line 31, count 4 → end 34.
	var ledgerFile *differ.ChangedFile
	for i := range diff.Files {
		if diff.Files[i].Path == "billing/ledger.go" {
			ledgerFile = &diff.Files[i]
		}
	}
	if ledgerFile == nil {
		t.Fatal("billing/ledger.go not found in diff")
	}
	if len(ledgerFile.HunkRanges) != 1 {
		t.Fatalf("expected 1 hunk for billing/ledger.go, got %d", len(ledgerFile.HunkRanges))
	}
	hunk := ledgerFile.HunkRanges[0]
	if hunk[0] != 31 {
		t.Errorf("hunk start: want 31, got %d", hunk[0])
	}
	// newCount=4, so end = 31 + 4 - 1 = 34
	if hunk[1] != 34 {
		t.Errorf("hunk end: want 34, got %d", hunk[1])
	}
}

func TestMapToSymbolsFindsOverlappingFunctions(t *testing.T) {
	diff, err := differ.FromPatch([]byte(rawDiff), "abc", "def")
	if err != nil {
		t.Fatalf("FromPatch: %v", err)
	}

	// Symbol rows simulating what Postgres would return for these files.
	// billing/ledger.go: Debit method spans lines 30–35, Credit spans 37–40.
	// payment/processor.go: GetBalance spans lines 56–60.
	symbols := []store.SymbolRow{
		{
			FilePath:   "billing/ledger.go",
			SymbolName: "github.com/example/billing.(SQLLedger).Debit",
			Kind:       "method",
			LineStart:  30,
			LineEnd:    35,
		},
		{
			FilePath:   "billing/ledger.go",
			SymbolName: "github.com/example/billing.(SQLLedger).Credit",
			Kind:       "method",
			LineStart:  37,
			LineEnd:    40,
		},
		{
			FilePath:   "payment/processor.go",
			SymbolName: "github.com/example/payment.(Processor).GetBalance",
			Kind:       "method",
			LineStart:  56,
			LineEnd:    60,
		},
		{
			FilePath:   "payment/processor.go",
			SymbolName: "github.com/example/payment.(Processor).Refund",
			Kind:       "method",
			LineStart:  47,
			LineEnd:    53,
		},
	}

	changed := differ.MapToSymbols(diff, symbols)

	// billing/ledger.go hunk is lines 31–34 → overlaps Debit (30–35), NOT Credit (37–40).
	// payment/processor.go hunk is line 60 → overlaps GetBalance (56–60), NOT Refund (47–53).
	want := map[string]bool{
		"github.com/example/billing.(SQLLedger).Debit":      true,
		"github.com/example/payment.(Processor).GetBalance": true,
	}

	if len(changed.Symbols) != len(want) {
		t.Errorf("expected %d changed symbols, got %d: %v",
			len(want), len(changed.Symbols), changed.Symbols)
	}
	for sym := range changed.Symbols {
		if !want[sym] {
			t.Errorf("unexpected symbol in changed set: %q", sym)
		}
	}
	for sym := range want {
		if _, ok := changed.Symbols[sym]; !ok {
			t.Errorf("missing expected symbol from changed set: %q", sym)
		}
	}
}

func TestMapToSymbolsExcludesInterfaceAndTypeDecls(t *testing.T) {
	// A diff that touches lines inside an interface declaration block should
	// NOT add the interface to the changed symbol set, because interface
	// declarations have no executable body.
	const ifaceDiff = `diff --git a/billing/iface.go b/billing/iface.go
index abc..def 100644
--- a/billing/iface.go
+++ b/billing/iface.go
@@ -5,1 +5,2 @@
 	Debit(amount int) error
+	Audit() string
`
	diff, _ := differ.FromPatch([]byte(ifaceDiff), "a", "b")
	symbols := []store.SymbolRow{
		{
			FilePath:   "billing/iface.go",
			SymbolName: "github.com/example/billing.Ledger",
			Kind:       "interface", // must be excluded
			LineStart:  3,
			LineEnd:    8,
		},
	}

	changed := differ.MapToSymbols(diff, symbols)
	if len(changed.Symbols) != 0 {
		t.Errorf("interface declaration should not appear in changed symbol set, got: %v",
			changed.Symbols)
	}
}

func TestMapToSymbolsNoOverlapProducesEmpty(t *testing.T) {
	diff, _ := differ.FromPatch([]byte(rawDiff), "a", "b")
	// Symbols that don't overlap with any hunk.
	symbols := []store.SymbolRow{
		{
			FilePath:   "billing/ledger.go",
			SymbolName: "github.com/example/billing.NewSQLLedger",
			Kind:       "function",
			LineStart:  26,
			LineEnd:    28, // hunk is at 31-34, no overlap
		},
	}
	changed := differ.MapToSymbols(diff, symbols)
	if len(changed.Symbols) != 0 {
		t.Errorf("expected empty changed set, got: %v", changed.Symbols)
	}
}
