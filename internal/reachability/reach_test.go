package reachability_test

import (
	"testing"

	"github.com/yourorg/symex/internal/callgraph"
	"github.com/yourorg/symex/internal/reachability"
	"github.com/yourorg/symex/internal/store"
)

// buildGraph is a test helper that constructs an InMemoryGraph from a slice of
// (source, target, provenance) triples without needing Postgres.
func buildGraph(edges []store.Edge) *callgraph.InMemoryGraph {
	g := &callgraph.InMemoryGraph{
		Edges:        make(map[string][]store.Edge),
		ReverseEdges: make(map[string][]store.Edge),
	}
	for _, e := range edges {
		g.Edges[e.SourceSymbol] = append(g.Edges[e.SourceSymbol], e)
		g.ReverseEdges[e.TargetSymbol] = append(g.ReverseEdges[e.TargetSymbol], e)
	}
	return g
}

func edge(src, tgt, prov string) store.Edge {
	return store.Edge{SourceSymbol: src, TargetSymbol: tgt, Provenance: prov}
}

// findReached is a test helper that returns the ReachedSymbol for a given
// symbol name, or nil if not found.
func findReached(result *reachability.Result, symbol string) *reachability.ReachedSymbol {
	for i := range result.Reached {
		if result.Reached[i].Symbol == symbol {
			return &result.Reached[i]
		}
	}
	return nil
}

// TestAllDirectPathIsDirectCall: a chain of all direct edges should produce
// a "direct_call" provenance for all reached symbols.
func TestAllDirectPathIsDirectCall(t *testing.T) {
	// A → B → C → D (all direct_call)
	g := buildGraph([]store.Edge{
		edge("A", "B", "direct_call"),
		edge("B", "C", "direct_call"),
		edge("C", "D", "direct_call"),
	})
	changed := map[string]string{"A": "a.go"}

	result := reachability.Run(g, changed)

	for _, sym := range []string{"B", "C", "D"} {
		rs := findReached(result, sym)
		if rs == nil {
			t.Errorf("expected %s to be reachable", sym)
			continue
		}
		if rs.Provenance != "direct_call" {
			t.Errorf("%s: expected provenance direct_call, got %s", sym, rs.Provenance)
		}
	}
}

// TestWeakestLinkPropagates: a single interface_resolved edge anywhere on the
// path degrades the entire path's confidence, even if subsequent edges are direct.
//
// This is the core invariant of the reachability engine.
func TestWeakestLinkPropagates(t *testing.T) {
	// A --direct--> B --interface_resolved--> C --direct--> D
	//
	// B should be direct_call (reached via one direct edge).
	// C should be interface_resolved (the edge from B to C is interface_resolved).
	// D should be interface_resolved (path goes through the weak B→C edge).
	g := buildGraph([]store.Edge{
		edge("A", "B", "direct_call"),
		edge("B", "C", "interface_resolved"),
		edge("C", "D", "direct_call"),
	})
	changed := map[string]string{"A": "a.go"}

	result := reachability.Run(g, changed)

	cases := []struct {
		symbol  string
		wantProv string
	}{
		{"B", "direct_call"},
		{"C", "interface_resolved"},
		{"D", "interface_resolved"}, // degraded by B→C edge
	}

	for _, tc := range cases {
		rs := findReached(result, tc.symbol)
		if rs == nil {
			t.Errorf("expected %s to be reachable", tc.symbol)
			continue
		}
		if rs.Provenance != tc.wantProv {
			t.Errorf("%s: expected provenance %s, got %s (path: %v)",
				tc.symbol, tc.wantProv, rs.Provenance, rs.Path)
		}
	}
}

// TestPathIsRecordedCorrectly: the Path field must be the full chain from
// the frontier symbol to the reached symbol.
func TestPathIsRecordedCorrectly(t *testing.T) {
	// Changed: A. Graph: A→B→C.
	g := buildGraph([]store.Edge{
		edge("A", "B", "direct_call"),
		edge("B", "C", "direct_call"),
	})
	changed := map[string]string{"A": "a.go"}

	result := reachability.Run(g, changed)

	c := findReached(result, "C")
	if c == nil {
		t.Fatal("C not in reachable set")
	}

	wantPath := []string{"A", "B", "C"}
	if len(c.Path) != len(wantPath) {
		t.Fatalf("path length: want %d, got %d: %v", len(wantPath), len(c.Path), c.Path)
	}
	for i, seg := range wantPath {
		if c.Path[i] != seg {
			t.Errorf("path[%d]: want %q, got %q", i, seg, c.Path[i])
		}
	}
	if c.Depth != 2 {
		t.Errorf("depth: want 2, got %d", c.Depth)
	}
}

// TestFrontierNotInReachableSet: the changed symbols themselves must not
// appear in the Reached slice (they're always in the "must run" tier).
func TestFrontierNotInReachableSet(t *testing.T) {
	g := buildGraph([]store.Edge{
		edge("A", "B", "direct_call"),
	})
	changed := map[string]string{"A": "a.go"}

	result := reachability.Run(g, changed)

	for _, rs := range result.Reached {
		if rs.Symbol == "A" {
			t.Errorf("frontier symbol A must not appear in Reached set")
		}
	}
}

// TestSelfLoopDoesNotHang: a self-loop (A→A) must not cause infinite BFS.
func TestSelfLoopDoesNotHang(t *testing.T) {
	g := buildGraph([]store.Edge{
		edge("A", "A", "direct_call"), // self-loop
		edge("A", "B", "direct_call"),
	})
	changed := map[string]string{"A": "a.go"}

	result := reachability.Run(g, changed)

	if findReached(result, "B") == nil {
		t.Error("B should be reachable from A")
	}
	if findReached(result, "A") != nil {
		t.Error("A (frontier) should not be in Reached set")
	}
}

// TestCycleDoesNotHang: a cycle (A→B→C→A) must not cause infinite BFS.
func TestCycleDoesNotHang(t *testing.T) {
	g := buildGraph([]store.Edge{
		edge("A", "B", "direct_call"),
		edge("B", "C", "direct_call"),
		edge("C", "A", "direct_call"), // cycle back to frontier
	})
	changed := map[string]string{"A": "a.go"}

	result := reachability.Run(g, changed)

	// B and C must be reached. A must not appear in Reached (it's the frontier).
	if findReached(result, "B") == nil {
		t.Error("B should be reachable")
	}
	if findReached(result, "C") == nil {
		t.Error("C should be reachable")
	}
	if findReached(result, "A") != nil {
		t.Error("A (frontier) must not appear in Reached")
	}
}

// TestMultipleFrontierSymbols: when multiple symbols are in the changed set,
// all their transitive callees are in the reachable set.
func TestMultipleFrontierSymbols(t *testing.T) {
	// Changed: {A, X}
	// A → B, X → Y, B → Y (diamond merge)
	g := buildGraph([]store.Edge{
		edge("A", "B", "direct_call"),
		edge("X", "Y", "interface_resolved"),
		edge("B", "Y", "direct_call"),
	})
	changed := map[string]string{
		"A": "a.go",
		"X": "x.go",
	}

	result := reachability.Run(g, changed)

	// B must be reachable (from A, direct).
	b := findReached(result, "B")
	if b == nil {
		t.Fatal("B should be reachable from frontier A")
	}
	if b.Provenance != "direct_call" {
		t.Errorf("B provenance: want direct_call, got %s", b.Provenance)
	}

	// Y is reachable from both X (interface_resolved) and via A→B→Y (direct).
	// BFS processes in FIFO order; X and A are both in the initial queue.
	// Whichever path reaches Y first is recorded. Since queue order is
	// non-deterministic for map iteration, we only assert Y IS reachable.
	if findReached(result, "Y") == nil {
		t.Error("Y should be reachable")
	}
}

// TestUnreachableSymbolsAreAbsent: symbols with no path from the frontier
// must not appear in the result.
func TestUnreachableSymbolsAreAbsent(t *testing.T) {
	// Changed: A. Graph has a disconnected component X→Y.
	g := buildGraph([]store.Edge{
		edge("A", "B", "direct_call"),
		edge("X", "Y", "direct_call"), // not reachable from A
	})
	changed := map[string]string{"A": "a.go"}

	result := reachability.Run(g, changed)

	if findReached(result, "X") != nil {
		t.Error("X should not be reachable from A")
	}
	if findReached(result, "Y") != nil {
		t.Error("Y should not be reachable from A")
	}
	if findReached(result, "B") == nil {
		t.Error("B should be reachable from A")
	}
}

// TestCounters: DirectCount and InterfaceResolvedCount must be correct.
func TestCounters(t *testing.T) {
	// A --direct--> B
	// A --interface_resolved--> C
	// C --direct--> D
	g := buildGraph([]store.Edge{
		edge("A", "B", "direct_call"),
		edge("A", "C", "interface_resolved"),
		edge("C", "D", "direct_call"),
	})
	changed := map[string]string{"A": "a.go"}

	result := reachability.Run(g, changed)

	// B: direct. C: interface_resolved. D: interface_resolved (via A→C).
	if result.DirectCount != 1 {
		t.Errorf("DirectCount: want 1, got %d", result.DirectCount)
	}
	if result.InterfaceResolvedCount != 2 {
		t.Errorf("InterfaceResolvedCount: want 2, got %d", result.InterfaceResolvedCount)
	}
	if result.TotalNodes != 3 {
		t.Errorf("TotalNodes: want 3, got %d", result.TotalNodes)
	}
}
