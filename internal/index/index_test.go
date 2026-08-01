package index_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/yourorg/symex/internal/index"
	"github.com/yourorg/symex/internal/store"
)

// uniqueIndexTestRepo returns a repo identifier that is unique not just
// within one test binary invocation but across separate `go test` runs
// against the same shared Postgres instance — a plain t.Name() collided
// with leftover data from a prior manual run and produced a confusing
// CHECK-constraint failure (real constraint, wrong root cause: stale data,
// not a product bug). See docs/DECISIONS.md.
func uniqueIndexTestRepo(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("index-e2e:%s:%d", t.Name(), time.Now().UnixNano())
}

func fixtureRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
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

// copyFixture makes a throwaway, mutable copy of testdata/fixture so tests
// can add/modify files across "commits" without touching the shared
// fixture other tests depend on.
func copyFixture(t *testing.T) string {
	t.Helper()
	dst := t.TempDir()
	if err := os.CopyFS(dst, os.DirFS(fixtureRoot(t))); err != nil {
		t.Fatalf("copy fixture: %v", err)
	}
	return dst
}

func testDSN() string {
	if dsn := os.Getenv("SYMEX_TEST_DSN"); dsn != "" {
		return dsn
	}
	return "postgres://symex:symex@localhost:5434/symex?sslmode=disable"
}

func openIndexTestDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(testDSN())
	if err != nil {
		t.Skipf("skipping: postgres unreachable at %s: %v", testDSN(), err)
	}
	ctx := context.Background()
	if err := db.ApplySchemaV3(ctx); err != nil {
		t.Fatalf("apply schema v3: %v", err)
	}
	if err := db.ApplySchemaV4(ctx); err != nil {
		t.Fatalf("apply schema v4: %v", err)
	}
	if err := db.ApplySchemaV5(ctx); err != nil {
		t.Fatalf("apply schema v5: %v", err)
	}
	if err := db.ApplySchemaV8(ctx); err != nil {
		t.Fatalf("apply schema v8: %v", err)
	}
	if err := db.ApplySchemaV9(ctx); err != nil {
		t.Fatalf("apply schema v9: %v", err)
	}
	return db
}

func debitTargets(t *testing.T, db *store.DB, repo string) map[string]bool {
	t.Helper()
	live, err := store.QueryLiveFacts(context.Background(), db.RawDB(), repo)
	if err != nil {
		t.Fatalf("QueryLiveFacts: %v", err)
	}
	targets := make(map[string]bool)
	for _, f := range live {
		if f.SourceSymbol == "github.com/example/fixture/payment.(Processor).ProcessPayment" &&
			f.Provenance == "interface_resolved" {
			targets[f.TargetSymbol] = true
		}
	}
	return targets
}

const cacheLedgerSource = `package billing

// CacheLedger is a third implementation of Ledger, added between "commits"
// to exercise the exact architecture.md section 2.2 scenario: a new
// implementer of an ALREADY-DISPATCHED interface, appearing in a file that
// no existing dispatch site's FILE derivation depends on.
type CacheLedger struct {
	balance int
}

func NewCacheLedger() *CacheLedger { return &CacheLedger{} }

func (l *CacheLedger) Debit(amount int) error {
	l.balance -= amount
	return nil
}

func (l *CacheLedger) Credit(amount int) error {
	l.balance += amount
	return nil
}

func (l *CacheLedger) Balance() int {
	return l.balance
}
`

// TestIndexCommitFromRepo_EndToEndSection2_2 is the real-pipeline version
// of TestSection2_2Fixture (internal/store) and the store-level
// TestImplementsProbe: instead of hand-constructing facts, it runs the
// actual parser -> ComputeFacts -> ApplyFacts pipeline against a real
// (copied, mutable) fixture repo across two "commits," and checks the same
// invariant architecture.md section 8's build order calls out as fixture 3:
// "add an implementer of an already-dispatched interface."
func TestIndexCommitFromRepo_EndToEndSection2_2(t *testing.T) {
	db := openIndexTestDB(t)
	repo := uniqueIndexTestRepo(t)
	dir := copyFixture(t)
	const modulePath = "github.com/example/fixture"

	stats0, err := index.IndexCommitFromRepo(context.Background(), db, dir, modulePath, repo, 0, "fp-0")
	if err != nil {
		t.Fatalf("IndexCommitFromRepo seq=0: %v", err)
	}
	if stats0 == nil {
		t.Fatal("expected a clean fixture to not be gated as poison")
	}
	if stats0.Opened == 0 {
		t.Fatal("expected seq=0 to open at least one fact")
	}
	if stats0.Closed != 0 || stats0.Unchanged != 0 {
		t.Errorf("expected seq=0 (first commit) to have Closed=0 Unchanged=0, got %+v", stats0)
	}

	before := debitTargets(t, db, repo)
	wantBefore := map[string]bool{
		"github.com/example/fixture/billing.(SQLLedger).Debit":     true,
		"github.com/example/fixture/billing.(InMemoryLedger).Debit": true,
	}
	if len(before) != len(wantBefore) {
		t.Fatalf("seq=0 Debit dispatch targets = %v, want %v", before, wantBefore)
	}
	for k := range wantBefore {
		if !before[k] {
			t.Errorf("seq=0: missing expected Debit target %s (got %v)", k, before)
		}
	}

	// "Commit" seq=1: add a new implementer in a brand new file. Nothing
	// about payment/processor.go (the dispatch site's own file) changes.
	if err := os.WriteFile(filepath.Join(dir, "billing", "cache_ledger.go"), []byte(cacheLedgerSource), 0644); err != nil {
		t.Fatalf("write cache_ledger.go: %v", err)
	}

	stats1, err := index.IndexCommitFromRepo(context.Background(), db, dir, modulePath, repo, 1, "fp-1")
	if err != nil {
		t.Fatalf("IndexCommitFromRepo seq=1: %v", err)
	}
	if stats1 == nil {
		t.Fatal("expected seq=1 to not be gated as poison")
	}
	if stats1.Opened == 0 {
		t.Fatal("expected seq=1 to open at least one new fact (the new CacheLedger edges)")
	}

	after := debitTargets(t, db, repo)
	wantAfter := map[string]bool{
		"github.com/example/fixture/billing.(SQLLedger).Debit":      true,
		"github.com/example/fixture/billing.(InMemoryLedger).Debit": true,
		"github.com/example/fixture/billing.(CacheLedger).Debit":    true,
	}
	if len(after) != len(wantAfter) {
		t.Fatalf("seq=1 Debit dispatch targets = %v, want %v", after, wantAfter)
	}
	for k := range wantAfter {
		if !after[k] {
			t.Errorf("seq=1: missing expected Debit target %s after adding CacheLedger (got %v) — this IS the section 2.2 fix, end to end", k, after)
		}
	}

	// The pre-existing edges must still be present (opening the new one
	// must not have closed the others).
	for k := range wantBefore {
		if !after[k] {
			t.Errorf("seq=1: pre-existing Debit target %s was lost — should only gain, not lose, edges here", k)
		}
	}
}

// TestIndexCommitFromRepo_PoisonCommitIsSkippedNotIndexed proves the
// poison-input gate (architecture.md section 3.2) is actually wired into
// the real pipeline, not just tested at the parser level in isolation.
func TestIndexCommitFromRepo_PoisonCommitIsSkippedNotIndexed(t *testing.T) {
	db := openIndexTestDB(t)
	repo := uniqueIndexTestRepo(t)
	dir := copyFixture(t)
	const modulePath = "github.com/example/fixture"

	// Break the build: reference an undefined symbol.
	brokenFile := filepath.Join(dir, "billing", "broken.go")
	if err := os.WriteFile(brokenFile, []byte("package billing\n\nfunc broken() { thisSymbolDoesNotExist() }\n"), 0644); err != nil {
		t.Fatalf("write broken.go: %v", err)
	}

	stats, err := index.IndexCommitFromRepo(context.Background(), db, dir, modulePath, repo, 0, "fp-0")
	if err != nil {
		t.Fatalf("IndexCommitFromRepo on a poison commit should not return an error (it should SKIP, not fail): %v", err)
	}
	if stats != nil {
		t.Fatalf("expected nil Stats for a poison-gated commit, got %+v", stats)
	}

	skipped, total, err := db.SkipRate(context.Background(), repo)
	if err != nil {
		t.Fatalf("SkipRate: %v", err)
	}
	if skipped != 1 {
		t.Errorf("expected 1 skipped commit recorded, got %d (total=%d)", skipped, total)
	}

	live, err := store.QueryLiveFacts(context.Background(), db.RawDB(), repo)
	if err != nil {
		t.Fatalf("QueryLiveFacts: %v", err)
	}
	if len(live) != 0 {
		t.Errorf("a poison commit must not have any facts indexed, got %d live facts", len(live))
	}
}
