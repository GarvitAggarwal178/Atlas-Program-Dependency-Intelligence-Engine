package canonicalize_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"github.com/yourorg/symex/internal/canonicalize"
)

// hashFunc parses src, finds the first *ast.FuncDecl, and returns its
// canonical hash. Fails the test on any parse/hash error.
func hashFunc(t *testing.T, src string) string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", "package p\n"+src, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		h, err := canonicalize.FuncHash(fset, fd)
		if err != nil {
			t.Fatalf("FuncHash: %v", err)
		}
		return h
	}
	t.Fatal("no function declaration found in src")
	return ""
}

// TestPureRenameIsTrivial: renaming a local variable must not change the hash.
func TestPureRenameIsTrivial(t *testing.T) {
	original := `func Add(x, y int) int { total := x + y; return total }`
	renamed := `func Add(x, y int) int { sum := x + y; return sum }`

	h1 := hashFunc(t, original)
	h2 := hashFunc(t, renamed)

	if h1 != h2 {
		t.Errorf("pure local rename should produce equal hashes:\n  original: %s\n  renamed:  %s", h1, h2)
	}
}

// TestCommentChangeIsTrivial: adding or changing a comment must not change the hash.
func TestCommentChangeIsTrivial(t *testing.T) {
	noComment := `func Compute(n int) int { result := n * 2; return result }`
	withComment := `
// Compute doubles n.
func Compute(n int) int {
	// multiply by two
	result := n * 2
	return result
}`

	h1 := hashFunc(t, noComment)
	h2 := hashFunc(t, withComment)

	if h1 != h2 {
		t.Errorf("comment change should produce equal hashes:\n  no-comment: %s\n  with-comment: %s", h1, h2)
	}
}

// TestOperatorChangeIsNotTrivial: changing + to * must change the hash.
func TestOperatorChangeIsNotTrivial(t *testing.T) {
	add := `func Op(x, y int) int { total := x + y; return total }`
	mul := `func Op(x, y int) int { total := x * y; return total }`

	h1 := hashFunc(t, add)
	h2 := hashFunc(t, mul)

	if h1 == h2 {
		t.Errorf("operator change (+ vs *) must produce different hashes, both gave: %s", h1)
	}
}

// TestCalleChangeIsNotTrivial: changing which function is called must change the hash.
func TestCalleeChangeIsNotTrivial(t *testing.T) {
	callFoo := `func Wrap(n int) int { return foo(n) }`
	callBar := `func Wrap(n int) int { return bar(n) }`

	h1 := hashFunc(t, callFoo)
	h2 := hashFunc(t, callBar)

	if h1 == h2 {
		t.Errorf("callee change (foo vs bar) must produce different hashes, both gave: %s", h1)
	}
}

// TestControlFlowChangeIsNotTrivial: adding a branch must change the hash.
func TestControlFlowChangeIsNotTrivial(t *testing.T) {
	noBranch := `func Safe(x int) int { return x }`
	withBranch := `func Safe(x int) int { if x < 0 { return 0 }; return x }`

	h1 := hashFunc(t, noBranch)
	h2 := hashFunc(t, withBranch)

	if h1 == h2 {
		t.Errorf("control flow change must produce different hashes, both gave: %s", h1)
	}
}

// TestWhitespaceChangeIsTrivial: indentation/whitespace changes must not
// affect the hash (the printer normalizes these).
func TestWhitespaceChangeIsTrivial(t *testing.T) {
	compact := `func F(x int) int { return x + 1 }`
	spaced := `
func F(x int) int {
	return x + 1
}
`

	h1 := hashFunc(t, compact)
	h2 := hashFunc(t, spaced)

	if h1 != h2 {
		t.Errorf("whitespace change should produce equal hashes:\n  compact: %s\n  spaced:  %s", h1, h2)
	}
}

// TestParameterRenameIsTrivial: renaming parameters is also a trivial change.
func TestParameterRenameIsTrivial(t *testing.T) {
	original := `func Multiply(a, b int) int { return a * b }`
	renamed := `func Multiply(x, y int) int { return x * y }`

	h1 := hashFunc(t, original)
	h2 := hashFunc(t, renamed)

	if h1 != h2 {
		t.Errorf("parameter rename should produce equal hashes:\n  original: %s\n  renamed:  %s", h1, h2)
	}
}

// TestReturnValueOrderChangeIsNotTrivial: swapping returned values must change the hash.
func TestReturnValueOrderChangeIsNotTrivial(t *testing.T) {
	original := `func Swap(a, b int) (int, int) { return a, b }`
	swapped := `func Swap(a, b int) (int, int) { return b, a }`

	h1 := hashFunc(t, original)
	h2 := hashFunc(t, swapped)

	if h1 == h2 {
		t.Errorf("return value swap must produce different hashes, both gave: %s", h1)
	}
}

// TestDeterminism: hashing the same function twice must produce the same result.
func TestDeterminism(t *testing.T) {
	src := `func Complex(x, y, z int) int {
	a := x + y
	b := a * z
	if b > 100 {
		return b - 100
	}
	return b
}`

	h1 := hashFunc(t, src)
	h2 := hashFunc(t, src)

	if h1 != h2 {
		t.Errorf("same function hashed twice gave different results: %s vs %s", h1, h2)
	}
}
