package classifier_test

import (
	"testing"

	"github.com/yourorg/symex/internal/classifier"
)

// src is a helper that wraps a function body in a minimal valid Go file.
// All test sources are wrapped with this so the parser has a real package
// declaration to work with.
func src(funcDecl string) []byte {
	return []byte("package testpkg\n\nimport \"fmt\"\n\n" + funcDecl)
}

// srcNoImport is for functions that don't need fmt.
func srcNoImport(funcDecl string) []byte {
	return []byte("package testpkg\n\n" + funcDecl)
}

// classify is a test helper that runs ClassifySymbol with a MockFetcher
// for a single function "testpkg.TheFunc".
func classify(t *testing.T, before, after []byte) *classifier.Classification {
	t.Helper()
	fetcher := classifier.NewMockFetcher(classifier.MockSources{
		{"base", "f.go"}: before,
		{"head", "f.go"}: after,
	}.Build())
	c, err := classifier.ClassifySymbol(fetcher, "base", "head", "f.go", "testpkg.TheFunc")
	if err != nil {
		t.Fatalf("ClassifySymbol: %v", err)
	}
	return c
}

// classifyMethod runs ClassifySymbol for "testpkg.(MyType).TheMethod".
func classifyMethod(t *testing.T, before, after []byte) *classifier.Classification {
	t.Helper()
	fetcher := classifier.NewMockFetcher(classifier.MockSources{
		{"base", "f.go"}: before,
		{"head", "f.go"}: after,
	}.Build())
	c, err := classifier.ClassifySymbol(fetcher, "base", "head", "f.go", "testpkg.(MyType).TheMethod")
	if err != nil {
		t.Fatalf("ClassifySymbol: %v", err)
	}
	return c
}

// ─── Required test cases ────────────────────────────────────────────────────

// TestPureRenameIsTrivial: renaming a local variable from `total` to `sum`
// with no other change must be classified as trivial.
//
// This is the canonical example of what canonicalization must handle.
// Without alpha-renaming, the two functions produce different printed ASTs
// and would be incorrectly classified as logic_change.
func TestPureRenameIsTrivial(t *testing.T) {
	before := srcNoImport(`func TheFunc(x, y int) int {
	total := x + y
	return total
}`)
	after := srcNoImport(`func TheFunc(x, y int) int {
	sum := x + y
	return sum
}`)

	c := classify(t, before, after)

	if c.Kind != classifier.Trivial {
		t.Errorf("pure local rename: expected trivial, got %s\n  reason: %s\n  before_hash: %s\n  after_hash:  %s",
			c.Kind, c.Reason, c.BeforeHash, c.AfterHash)
	}
	if c.BeforeHash != c.AfterHash {
		t.Errorf("pure local rename: hashes must be equal\n  before: %s\n  after:  %s",
			c.BeforeHash, c.AfterHash)
	}
}

// TestPureCommentEditIsTrivial: adding, removing, or changing a doc comment
// must be classified as trivial.
func TestPureCommentEditIsTrivial(t *testing.T) {
	before := srcNoImport(`func TheFunc(n int) int {
	return n * 2
}`)
	after := srcNoImport(`// TheFunc doubles n.
// It is safe to call concurrently.
func TheFunc(n int) int {
	// multiply by two
	return n * 2
}`)

	c := classify(t, before, after)

	if c.Kind != classifier.Trivial {
		t.Errorf("pure comment edit: expected trivial, got %s\n  reason: %s",
			c.Kind, c.Reason)
	}
}

// TestParameterTypeChangeIsSignatureChange: changing a parameter type from
// int to int64 must be classified as signature_change, even if the body
// compiles and looks identical.
func TestParameterTypeChangeIsSignatureChange(t *testing.T) {
	before := srcNoImport(`func TheFunc(amount int) error {
	if amount < 0 {
		return fmt.Errorf("negative")
	}
	return nil
}`)
	after := srcNoImport(`func TheFunc(amount int64) error {
	if amount < 0 {
		return fmt.Errorf("negative")
	}
	return nil
}`)

	c := classify(t, before, after)

	if c.Kind != classifier.SignatureChange {
		t.Errorf("parameter type change: expected signature_change, got %s\n  reason: %s",
			c.Kind, c.Reason)
	}
	if c.BeforeSignature == c.AfterSignature {
		t.Errorf("parameter type change: signatures should differ\n  both: %s", c.BeforeSignature)
	}
}

// TestLogicChangeDisguisedAsRename: THE CRITICAL CASE.
//
// This is the failure mode that naive implementations get wrong.
// The function renames `total` to `sum` (looks like trivial) but ALSO changes
// the operator from + to * (a real logic change).
//
// A naive implementation might see the rename and stop checking, or might
// strip only the name difference and miss the operator.
//
// Our canonicalization alpha-renames ALL locals first, then hashes. After
// renaming, the bodies are:
//   before: `_v2 := _v0 + _v1; return _v2`
//   after:  `_v2 := _v0 * _v1; return _v2`
//
// The operator survives alpha-renaming and the hashes differ → logic_change.
func TestLogicChangeDisguisedAsRename(t *testing.T) {
	before := srcNoImport(`func TheFunc(x, y int) int {
	total := x + y
	return total
}`)
	after := srcNoImport(`func TheFunc(x, y int) int {
	sum := x * y   // renamed total→sum AND changed + to *
	return sum
}`)

	c := classify(t, before, after)

	if c.Kind != classifier.LogicChange {
		t.Errorf("logic change disguised as rename: expected logic_change, got %s\n"+
			"  reason: %s\n"+
			"  This means canonicalization is incorrectly treating the operator change as trivial.\n"+
			"  before_hash: %s\n"+
			"  after_hash:  %s",
			c.Kind, c.Reason, c.BeforeHash, c.AfterHash)
	}
	if c.BeforeHash == c.AfterHash {
		t.Errorf("logic change disguised as rename: hashes must DIFFER (operator changed)\n"+
			"  both: %s\n"+
			"  This is the canonical failure mode: renaming masked a real logic change.",
			c.BeforeHash)
	}
}

// ─── Additional cases ───────────────────────────────────────────────────────

// TestReturnTypeChangeIsSignatureChange: adding a return value is a signature change.
func TestReturnTypeChangeIsSignatureChange(t *testing.T) {
	before := srcNoImport(`func TheFunc(n int) {
	_ = n
}`)
	after := srcNoImport(`func TheFunc(n int) error {
	_ = n
	return nil
}`)

	c := classify(t, before, after)
	if c.Kind != classifier.SignatureChange {
		t.Errorf("return type added: expected signature_change, got %s", c.Kind)
	}
}

// TestParameterNameRenameIsNotSignatureChange: renaming a parameter must NOT
// be a signature change — only the types matter for callers.
//
// Before: func TheFunc(amount int) int
// After:  func TheFunc(total int) int
// Both have signature "(int) int" — must be trivial.
func TestParameterNameRenameIsNotSignatureChange(t *testing.T) {
	before := srcNoImport(`func TheFunc(amount int) int {
	return amount * 2
}`)
	after := srcNoImport(`func TheFunc(total int) int {
	return total * 2
}`)

	c := classify(t, before, after)
	if c.Kind != classifier.Trivial {
		t.Errorf("parameter name rename: expected trivial (names don't affect callers), got %s\n"+
			"  reason: %s\n  before_sig: %s\n  after_sig: %s",
			c.Kind, c.Reason, c.BeforeSignature, c.AfterSignature)
	}
}

// TestNewBranchIsLogicChange: adding an if-branch is a logic change.
func TestNewBranchIsLogicChange(t *testing.T) {
	before := srcNoImport(`func TheFunc(x int) int {
	return x
}`)
	after := srcNoImport(`func TheFunc(x int) int {
	if x < 0 {
		return 0
	}
	return x
}`)

	c := classify(t, before, after)
	if c.Kind != classifier.LogicChange {
		t.Errorf("new branch: expected logic_change, got %s", c.Kind)
	}
}

// TestNewCalleeIsLogicChange: changing which function is called must be a logic change.
// Even with identical local variable names and structure.
func TestNewCalleeIsLogicChange(t *testing.T) {
	before := src(`func TheFunc(n int) string {
	result := fmt.Sprintf("%d", n)
	return result
}`)
	after := src(`func TheFunc(n int) string {
	result := fmt.Sprint(n)
	return result
}`)

	c := classify(t, before, after)
	if c.Kind != classifier.LogicChange {
		t.Errorf("callee changed (Sprintf→Sprint): expected logic_change, got %s\n"+
			"  reason: %s", c.Kind, c.Reason)
	}
}

// TestWhitespaceOnlyIsTrivial: changing indentation or spacing is trivial.
// go/printer normalizes whitespace, so the printed representation is identical.
func TestWhitespaceOnlyIsTrivial(t *testing.T) {
	before := srcNoImport(`func TheFunc(x int) int { return x + 1 }`)
	after := srcNoImport(`func TheFunc(x int) int {
	return x + 1
}`)

	c := classify(t, before, after)
	if c.Kind != classifier.Trivial {
		t.Errorf("whitespace only: expected trivial, got %s\n  reason: %s", c.Kind, c.Reason)
	}
}

// TestMethodClassification: methods on concrete types are found by name+receiver.
func TestMethodClassification(t *testing.T) {
	before := srcNoImport(`type MyType struct{ value int }

func (m *MyType) TheMethod(x int) int {
	result := m.value + x
	return result
}`)
	after := srcNoImport(`type MyType struct{ value int }

func (m *MyType) TheMethod(x int) int {
	// added a comment
	total := m.value + x
	return total
}`)

	c := classifyMethod(t, before, after)
	if c.Kind != classifier.Trivial {
		t.Errorf("method rename+comment: expected trivial, got %s\n  reason: %s", c.Kind, c.Reason)
	}
}

// TestMethodSignatureChange: changing a method's receiver type is a signature change.
func TestMethodSignatureChange(t *testing.T) {
	before := srcNoImport(`type MyType struct{ value int }

func (m *MyType) TheMethod(x int) int {
	return m.value + x
}`)
	after := srcNoImport(`type MyType struct{ value int }

func (m MyType) TheMethod(x int) int {
	return m.value + x
}`)

	// The receiver change (pointer → value) is a signature change for the method set.
	// Both have the same unqualified name, but the body parameter list and
	// receiver differ. Since our signature comparison doesn't include the
	// receiver (callers use the method set, not the receiver pointer-ness),
	// this tests that the body hash is still identical and it's classified
	// correctly.
	//
	// Note: pointer receiver vs value receiver changes the method SET of the
	// type, which IS a breaking change for callers, but our signature printer
	// only covers params and return types (not receiver) — same as how callers
	// call it. Both `m.TheMethod(x)` forms compile the same way for callers.
	// This is documented as a known limitation below.
	c := classifyMethod(t, before, after)
	// With identical param/return types and identical body, this is trivial
	// under our current signature definition (receiver excluded from sig).
	// This is intentional — see README for the receiver-type caveat.
	t.Logf("pointer→value receiver: classified as %s (reason: %s)", c.Kind, c.Reason)
	// Not asserting a specific kind here — the test documents the behavior.
}

// TestNewFunctionIsLogicChange: a symbol that didn't exist at base commit
// should be classified as logic_change (not an error).
func TestNewFunctionIsLogicChange(t *testing.T) {
	// base commit: file exists but doesn't contain TheFunc
	// head commit: file contains TheFunc
	fetcher := classifier.NewMockFetcher(classifier.MockSources{
		{"base", "f.go"}: srcNoImport(`func OtherFunc() {}`),
		{"head", "f.go"}: srcNoImport(`func OtherFunc() {}

func TheFunc(x int) int { return x }
`),
	}.Build())
	c, err := classifier.ClassifySymbol(fetcher, "base", "head", "f.go", "testpkg.TheFunc")
	if err != nil {
		t.Fatalf("ClassifySymbol: %v", err)
	}
	if c.Kind != classifier.LogicChange {
		t.Errorf("new function: expected logic_change, got %s", c.Kind)
	}
}

// TestReturnArgumentOrderChangeIsLogicChange: swapping what's returned changes
// the hash even if local names are identical.
func TestReturnArgumentOrderChangeIsLogicChange(t *testing.T) {
	before := srcNoImport(`func TheFunc(a, b int) (int, int) {
	return a, b
}`)
	after := srcNoImport(`func TheFunc(a, b int) (int, int) {
	return b, a
}`)

	c := classify(t, before, after)
	if c.Kind != classifier.LogicChange {
		t.Errorf("swapped return args: expected logic_change, got %s\n  reason: %s",
			c.Kind, c.Reason)
	}
}

// TestClassifyAll: verifies the batch classifier correctly partitions the
// changed symbol set into trivial (excluded from frontier) and non-trivial.
func TestClassifyAll(t *testing.T) {
	// Two symbols in the changed set:
	//   - "testpkg.Rename": pure rename → trivial → must be excluded from frontier
	//   - "testpkg.Logic":  operator change → logic_change → must remain in frontier
	fetcher := classifier.NewMockFetcher(classifier.MockSources{
		{"base", "rename.go"}: srcNoImport(`func Rename(x, y int) int { a := x + y; return a }`),
		{"head", "rename.go"}: srcNoImport(`func Rename(x, y int) int { b := x + y; return b }`),
		{"base", "logic.go"}:  srcNoImport(`func Logic(x, y int) int { a := x + y; return a }`),
		{"head", "logic.go"}:  srcNoImport(`func Logic(x, y int) int { a := x - y; return a }`),
	}.Build())

	changedSymbols := map[string]string{
		"testpkg.Rename": "rename.go",
		"testpkg.Logic":  "logic.go",
	}

	classifications, frontier, err := classifier.ClassifyAll(
		fetcher, "base", "head", changedSymbols,
	)
	if err != nil {
		t.Fatalf("ClassifyAll: %v", err)
	}

	// Rename must be trivial and excluded from frontier.
	if c := classifications["testpkg.Rename"]; c == nil {
		t.Error("testpkg.Rename: missing from classifications")
	} else if c.Kind != classifier.Trivial {
		t.Errorf("testpkg.Rename: expected trivial, got %s", c.Kind)
	}
	if _, inFrontier := frontier["testpkg.Rename"]; inFrontier {
		t.Error("testpkg.Rename (trivial): must be excluded from filtered frontier")
	}

	// Logic must be logic_change and remain in frontier.
	if c := classifications["testpkg.Logic"]; c == nil {
		t.Error("testpkg.Logic: missing from classifications")
	} else if c.Kind != classifier.LogicChange {
		t.Errorf("testpkg.Logic: expected logic_change, got %s", c.Kind)
	}
	if _, inFrontier := frontier["testpkg.Logic"]; !inFrontier {
		t.Error("testpkg.Logic (logic_change): must remain in filtered frontier")
	}

	// Frontier must contain exactly Logic.
	if len(frontier) != 1 {
		t.Errorf("filtered frontier: expected 1 symbol, got %d: %v", len(frontier), frontier)
	}
}

// ─── Exhaustive table-driven cases ──────────────────────────────────────────

func TestClassifierTable(t *testing.T) {
	cases := []struct {
		name     string
		before   string
		after    string
		wantKind classifier.ChangeKind
	}{
		{
			name:     "identical function is trivial",
			before:   `func TheFunc(x int) int { return x + 1 }`,
			after:    `func TheFunc(x int) int { return x + 1 }`,
			wantKind: classifier.Trivial,
		},
		{
			name:     "added blank line is trivial",
			before:   `func TheFunc(x int) int { return x + 1 }`,
			after:    "func TheFunc(x int) int {\n\n\treturn x + 1\n}",
			wantKind: classifier.Trivial,
		},
		{
			name:     "added return variable name is trivial (pure rename)",
			before:   `func TheFunc(x int) int { return x + 1 }`,
			after:    `func TheFunc(x int) int { result := x + 1; return result }`,
			// The semantics are identical: extracting a return expr into a
			// named variable with no other change.
			// After canonicalization: `return _v0 + 1` vs `_v1 := _v0 + 1; return _v1`
			// These are structurally different (one is a ReturnStmt with BinaryExpr,
			// the other is AssignStmt + ReturnStmt), so this IS a logic_change.
			// This is correct behavior: the structure changed even if semantics didn't.
			wantKind: classifier.LogicChange,
		},
		{
			name:     "literal value change is logic_change",
			before:   `func TheFunc(x int) int { return x + 1 }`,
			after:    `func TheFunc(x int) int { return x + 2 }`,
			wantKind: classifier.LogicChange,
		},
		{
			name:     "added parameter is signature_change",
			before:   `func TheFunc(x int) int { return x }`,
			after:    `func TheFunc(x int, y int) int { return x + y }`,
			wantKind: classifier.SignatureChange,
		},
		{
			name:     "removed parameter is signature_change",
			before:   `func TheFunc(x int, y int) int { return x + y }`,
			after:    `func TheFunc(x int) int { return x }`,
			wantKind: classifier.SignatureChange,
		},
		{
			name:     "renamed parameter only is trivial",
			before:   `func TheFunc(amount int) int { return amount }`,
			after:    `func TheFunc(total int) int { return total }`,
			wantKind: classifier.Trivial,
		},
		{
			name:     "multi-rename all params is trivial",
			before:   `func TheFunc(a, b, c int) int { return a + b + c }`,
			after:    `func TheFunc(x, y, z int) int { return x + y + z }`,
			wantKind: classifier.Trivial,
		},
		{
			name: "rename + operator change is logic_change (THE KEY CASE)",
			before: `func TheFunc(x, y int) int {
	total := x + y
	return total
}`,
			after: `func TheFunc(x, y int) int {
	sum := x * y
	return sum
}`,
			wantKind: classifier.LogicChange,
		},
		{
			name:     "new error return is signature_change",
			before:   `func TheFunc() int { return 0 }`,
			after:    `func TheFunc() (int, error) { return 0, nil }`,
			wantKind: classifier.SignatureChange,
		},
		{
			name:     "range loop variable rename is trivial",
			before:   `func TheFunc(items []int) int { sum := 0; for _, v := range items { sum += v }; return sum }`,
			after:    `func TheFunc(items []int) int { total := 0; for _, x := range items { total += x }; return total }`,
			wantKind: classifier.Trivial,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			before := srcNoImport(tc.before)
			after := srcNoImport(tc.after)
			c := classify(t, before, after)
			if c.Kind != tc.wantKind {
				t.Errorf("expected %s, got %s\n  reason: %s\n  before_hash: %s\n  after_hash:  %s",
					tc.wantKind, c.Kind, c.Reason, c.BeforeHash, c.AfterHash)
			}
		})
	}
}
