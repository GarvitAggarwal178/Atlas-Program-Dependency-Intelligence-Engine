// measurement_10_1_test.go is architecture.md build-order step 7:
// "Measure section 10.1 against the frozen v2 engine." It runs a
// controlled scenario through v2's REAL, unmodified engine
// (internal/callgraph.BuildV2 + internal/incremental.Update, tagged
// v2-frozen) and through v3.1's real pipeline (this package), and
// compares "facts withdrawn" against "facts that actually differ under
// full rebuild" for both.
//
// The scenario reproduces the EXACT over-invalidation mechanism v2's own
// code comments describe (internal/incremental/engine.go's
// RetractInterfaceEdgesForCallSites: "we retract ALL interface_resolved
// edges from these callers... not just the ones for the changed
// interface"): a caller function dispatches through two independent
// interfaces I1 and I2. A commit changes I1's implementer set (removes A,
// adds B). I2 is completely unaffected. v2's step 2/4 retracts and
// reinserts BOTH interfaces' edges from that caller, because its
// cross-file invalidation is scoped to the CALLER, not the SPECIFIC
// interface that changed. v3.1's derivation-tracked diff only touches
// what actually changed.
//
// A real Postgres trigger (internal/store/audit.go) observes v2's actual
// DELETE volume without a single line of v2's frozen Go source being
// touched — net before/after diffing can't see internal churn
// (delete-then-reinsert-the-same-thing), only observing the actual
// statements can.
package index_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/yourorg/symex/internal/callgraph"
	"github.com/yourorg/symex/internal/incremental"
	"github.com/yourorg/symex/internal/index"
	"github.com/yourorg/symex/internal/parser"
)

const measGoMod = "module example.com/section10-1\n\ngo 1.21\n"

const measIfaceGo = `package main

type I1 interface{ M1() }
type I2 interface{ M2() }
`

// commit 1: I1={A}, I2={X}. Caller dispatches through both.
const measImplsV1 = `package main

type A struct{}
func (a *A) M1() {}

type X struct{}
func (x *X) M2() {}

func Caller(i1 I1, i2 I2) {
	i1.M1()
	i2.M2()
}
`

// commit 2: I1={B} (A removed, B added). I2={X}, UNCHANGED.
const measImplsV2 = `package main

type B struct{}
func (b *B) M1() {}

type X struct{}
func (x *X) M2() {}

func Caller(i1 I1, i2 I2) {
	i1.M1()
	i2.M2()
}
`

// TestSection10_1_Measurement runs the scenario through both engines and
// reports the withdrawn/differed ratio for each — the narrow, honest form
// architecture.md section 10.1 requires: "Measured that my own rule-based
// v2 engine over-invalidated by K at p90 versus derivation-tracked
// maintenance over the same commit sequence." N=1 controlled scenario,
// stated as such — not a large-sample statistical claim.
func TestSection10_1_Measurement(t *testing.T) {
	db := openIndexTestDB(t)
	ctx := context.Background()
	if err := db.ApplyAuditSchema(ctx); err != nil {
		t.Fatalf("apply audit schema: %v", err)
	}

	dir := t.TempDir()
	const mod = "example.com/section10-1"
	writeModule(t, dir, map[string]string{
		"go.mod":  measGoMod,
		"iface.go": measIfaceGo,
		"impls.go": measImplsV1,
	})

	// ---- Ground truth: what actually differs under a full rebuild ----
	// Independent of both engines — computed directly from ComputeFacts at
	// each commit, in-memory, no DB involved. This is the section 10.1
	// denominator for BOTH v2 and v3.1: it does not know or care which
	// engine produced it.
	pkgs1, fset1, err := parser.LoadPackages(dir)
	if err != nil {
		t.Fatalf("LoadPackages v1: %v", err)
	}
	table1 := parser.BuildSymbolTable(pkgs1, fset1, dir, mod)
	facts1, _, err := index.ComputeFacts(dir, mod, table1)
	if err != nil {
		t.Fatalf("ComputeFacts v1: %v", err)
	}

	writeModule(t, dir, map[string]string{"impls.go": measImplsV2})

	pkgs2, fset2, err := parser.LoadPackages(dir)
	if err != nil {
		t.Fatalf("LoadPackages v2: %v", err)
	}
	table2 := parser.BuildSymbolTable(pkgs2, fset2, dir, mod)
	facts2, _, err := index.ComputeFacts(dir, mod, table2)
	if err != nil {
		t.Fatalf("ComputeFacts v2: %v", err)
	}

	removed := symmetricDiffRemoved(facts1, facts2)
	t.Logf("ground truth: %d edge(s) genuinely removed between commits: %v", len(removed), removed)
	if len(removed) == 0 {
		t.Fatal("test setup error: expected at least one genuinely-removed edge (A's I1 dispatch)")
	}

	// ---- v2: real engine, real audit trigger ----
	v2Repo := "meas-v2:" + uniqueIndexTestRepo(t)
	if err := db.ClearAuditLog(ctx, v2Repo); err != nil {
		t.Fatalf("clear audit log: %v", err)
	}

	// Rewind to commit 1 on disk, build v2's graph for baseCommit.
	writeModule(t, dir, map[string]string{"impls.go": measImplsV1})
	table1b, err := parser.ParseRepo(dir, mod) // v2's own frontend path
	if err != nil {
		t.Fatalf("v2 ParseRepo (base): %v", err)
	}
	if _, err := callgraph.BuildV2(ctx, db, table1b, v2Repo, "base"); err != nil {
		t.Fatalf("v2 BuildV2 (base): %v", err)
	}

	// Advance to commit 2 on disk, run v2's incremental Update.
	writeModule(t, dir, map[string]string{"impls.go": measImplsV2})
	if _, err := incremental.Update(ctx, db, dir, mod, v2Repo, "base", "head", []string{"impls.go"}); err != nil {
		t.Fatalf("v2 incremental.Update: %v", err)
	}

	auditLog, err := db.AuditLog(ctx, v2Repo)
	if err != nil {
		t.Fatalf("AuditLog: %v", err)
	}
	v2Deletes := 0
	for _, e := range auditLog {
		if e.Op == "DELETE" && e.CommitHash == "head" {
			v2Deletes++
		}
	}
	t.Logf("v2 (real engine, audited): %d fact row(s) actually DELETEd applying base->head", v2Deletes)
	for _, e := range auditLog {
		if e.Op == "DELETE" {
			t.Logf("  v2 DELETE: %s -> %s [%s] (commit=%s)", e.SourceSymbol, e.TargetSymbol, e.Provenance, e.CommitHash)
		}
	}

	// ---- v3.1: real engine ----
	writeModule(t, dir, map[string]string{"impls.go": measImplsV1})
	v3Repo := "meas-v3:" + uniqueIndexTestRepo(t)
	if _, err := index.IndexCommitFromRepo(ctx, db, dir, mod, v3Repo, 0, "fp-0"); err != nil {
		t.Fatalf("v3.1 IndexCommitFromRepo seq=0: %v", err)
	}
	writeModule(t, dir, map[string]string{"impls.go": measImplsV2})
	stats, err := index.IndexCommitFromRepo(ctx, db, dir, mod, v3Repo, 1, "fp-1")
	if err != nil {
		t.Fatalf("v3.1 IndexCommitFromRepo seq=1: %v", err)
	}
	t.Logf("v3.1 (real engine): Closed=%d Opened=%d Unchanged=%d", stats.Closed, stats.Opened, stats.Unchanged)

	// ---- The comparison ----
	needed := len(removed)
	v2Ratio := float64(v2Deletes) / float64(needed)
	v3Ratio := float64(stats.Closed) / float64(needed)

	t.Logf("=== section 10.1 result (N=1 controlled scenario) ===")
	t.Logf("facts that actually needed withdrawal (ground truth): %d", needed)
	t.Logf("v2 (hand-written rules):        %d withdrawn, ratio %.2fx", v2Deletes, v2Ratio)
	t.Logf("v3.1 (derivation-tracked):      %d withdrawn, ratio %.2fx", stats.Closed, v3Ratio)

	// I10, asserted SEPARATELY per architecture.md: a ratio below 1.0 is a
	// SOUNDNESS failure (under-withdrawal), not efficiency. Must never be
	// silently conflated with "efficient."
	if v2Ratio < 1.0 {
		t.Errorf("I10 VIOLATED for v2: ratio %.2f < 1.0 means v2 under-withdrew — a soundness bug, not efficiency", v2Ratio)
	}
	if v3Ratio < 1.0 {
		t.Errorf("I10 VIOLATED for v3.1: ratio %.2f < 1.0 means v3.1 under-withdrew — a soundness bug, not efficiency", v3Ratio)
	}

	// The actual claim this scenario demonstrates: v2 over-invalidates
	// relative to v3.1 on a case its own code comments predict it will.
	if v2Deletes <= stats.Closed {
		t.Errorf("expected this scenario to demonstrate v2 over-invalidating MORE than v3.1 (that's the whole point of the I2-untouched-interface case) — got v2=%d v3.1=%d",
			v2Deletes, stats.Closed)
	}
}

// symmetricDiffRemoved returns the natural keys present in `before` but
// absent from `after` — the ground-truth "facts that actually needed
// withdrawal" for a commit transition, independent of either engine.
func symmetricDiffRemoved(before, after []index.FactWithDerivations) []string {
	afterKeys := make(map[string]bool, len(after))
	for _, f := range after {
		afterKeys[factNaturalKeyForMeasurement(f)] = true
	}
	var removed []string
	for _, f := range before {
		k := factNaturalKeyForMeasurement(f)
		if !afterKeys[k] {
			removed = append(removed, k)
		}
	}
	return removed
}

func factNaturalKeyForMeasurement(f index.FactWithDerivations) string {
	return fmt.Sprintf("%s|%s|%s|%s", f.Fact.SourceSymbol, f.Fact.TargetSymbol, f.Fact.Provenance, f.Fact.CallSite)
}
