package reach_test

import (
	"testing"

	"github.com/yourorg/symex/internal/reach"
)

// TestFixture8_SelfSupportingCycle is architecture.md section 4.4's
// fixture, the most important test in the DRed section: "a mutually
// recursive pair reachable through exactly one external edge. Delete that
// edge. Assert both symbols leave the reachable set." Section 9.1 lists
// this as fixture 8 ("delete the sole external edge into a mutually
// recursive component").
//
// Written to verify it FAILS against naive support counting first — a
// test that has never failed proves nothing — then confirms the actual
// mechanism (Reachable, a BFS recompute of the least fixpoint) gets it
// right.
func TestFixture8_SelfSupportingCycle(t *testing.T) {
	entryPoints := []string{"main"}

	// The setup: main -> A, A -> B, B -> A. A and B are mutually
	// recursive, reachable only via the single edge main->A.
	fullEdges := []reach.Edge{
		{Source: "main", Target: "A"},
		{Source: "A", Target: "B"},
		{Source: "B", Target: "A"},
	}

	t.Run("naive support counting WRONGLY keeps the cycle alive", func(t *testing.T) {
		s := reach.NewNaiveSupportCounting(entryPoints)
		s.InsertEdge("main", "A") // support[A] = 1, A reachable
		s.InsertEdge("A", "B")    // support[B] = 1, B reachable
		s.InsertEdge("B", "A")    // support[A] = 2

		// Sanity: both reachable before the delete.
		before := s.Reachable()
		if !before["A"] || !before["B"] {
			t.Fatalf("test setup error: expected A and B reachable before delete, got %v", before)
		}

		// Delete the sole EXTERNAL edge into the cycle.
		s.DeleteEdge("main", "A") // support[A] = 1 (still has support from B) -- WRONG, stays "reachable"

		after := s.Reachable()
		if !after["A"] || !after["B"] {
			t.Fatalf("expected this test to demonstrate naive support counting's bug (A and B WRONGLY still reachable), but got %v — "+
				"if this fails, the naive simulation itself is broken, not fixed", after)
		}
		t.Logf("confirmed: naive support counting leaves A and B reachable forever after their only external edge is deleted (got %v)", after)
	})

	t.Run("Reachable (BFS recompute) gets it right", func(t *testing.T) {
		// Before deletion: everything reachable.
		before := reach.Reachable(fullEdges, entryPoints)
		if !before["A"] || !before["B"] {
			t.Fatalf("expected A and B reachable before delete, got %v", before)
		}

		// After deleting main->A: only the cycle edges remain, with no
		// path from any entry point.
		afterEdges := []reach.Edge{
			{Source: "A", Target: "B"},
			{Source: "B", Target: "A"},
		}
		after := reach.Reachable(afterEdges, entryPoints)

		if after["A"] {
			t.Error("A must NOT be reachable after its only external edge is deleted (least fixpoint, not naive support counting)")
		}
		if after["B"] {
			t.Error("B must NOT be reachable after its only external edge is deleted")
		}
		if !after["main"] {
			t.Error("main itself (the entry point) must still be reachable")
		}
	})
}

func TestReachable_Basic(t *testing.T) {
	edges := []reach.Edge{
		{Source: "main", Target: "A"},
		{Source: "A", Target: "B"},
		{Source: "B", Target: "C"},
	}
	got := reach.Reachable(edges, []string{"main"})
	want := map[string]bool{"main": true, "A": true, "B": true, "C": true}
	for k := range want {
		if !got[k] {
			t.Errorf("expected %s reachable, got %v", k, got)
		}
	}
	if len(got) != len(want) {
		t.Errorf("got %v, want exactly %v", got, want)
	}
}

func TestReachable_UnreachableNodeExcluded(t *testing.T) {
	edges := []reach.Edge{
		{Source: "main", Target: "A"},
		{Source: "Orphan", Target: "Zombie"}, // not reachable from main
	}
	got := reach.Reachable(edges, []string{"main"})
	if got["Orphan"] || got["Zombie"] {
		t.Errorf("Orphan/Zombie must not be reachable, got %v", got)
	}
	if !got["A"] {
		t.Errorf("A must be reachable, got %v", got)
	}
}

func TestReachable_MultipleEntryPoints(t *testing.T) {
	edges := []reach.Edge{
		{Source: "main1", Target: "Shared"},
		{Source: "main2", Target: "Other"},
	}
	got := reach.Reachable(edges, []string{"main1", "main2"})
	for _, want := range []string{"main1", "main2", "Shared", "Other"} {
		if !got[want] {
			t.Errorf("expected %s reachable, got %v", want, got)
		}
	}
}
