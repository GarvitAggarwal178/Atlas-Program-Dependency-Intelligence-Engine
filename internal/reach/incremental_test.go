package reach_test

import (
	"fmt"
	"math/rand"
	"sort"
	"testing"

	"github.com/yourorg/symex/internal/reach"
)

// TestIncrementalUpdate_Fixture8 is the same section 4.4 fixture, run
// through IncrementalUpdate instead of a full recompute — the self-
// supporting cycle case is exactly what phase 1's pessimistic downstream
// marking exists to get right (A and B are BOTH downstream of the deleted
// main->A edge in the OLD graph, so both get marked dead, and phase 2's
// rederive correctly finds neither has any OTHER live incoming edge from
// a confirmed-reachable node — they stay dead).
func TestIncrementalUpdate_Fixture8(t *testing.T) {
	entryPoints := []string{"main"}
	oldEdges := []reach.Edge{
		{Source: "main", Target: "A"},
		{Source: "A", Target: "B"},
		{Source: "B", Target: "A"},
	}
	oldReachable := reach.Reachable(oldEdges, entryPoints)

	newEdges := []reach.Edge{
		{Source: "A", Target: "B"},
		{Source: "B", Target: "A"},
	}

	got := reach.IncrementalUpdate(entryPoints, oldEdges, newEdges, oldReachable)
	if got["A"] {
		t.Error("A must not be reachable after IncrementalUpdate — self-supporting cycle, sole external edge deleted")
	}
	if got["B"] {
		t.Error("B must not be reachable after IncrementalUpdate")
	}
	if !got["main"] {
		t.Error("main itself must still be reachable")
	}
}

// TestIncrementalUpdate_DifferentialFuzz is the actual safety net for
// trusting this algorithm at all: generate random graphs, apply random
// edge mutations across a SEQUENCE of deltas (feeding the incremental
// result forward as the next delta's starting point, not resetting to
// ground truth — this is what actually happens in production, and it's
// exactly the scenario where small errors would compound and get caught),
// and assert IncrementalUpdate matches Reachable (full recompute, already
// proven correct) after every single delta.
func TestIncrementalUpdate_DifferentialFuzz(t *testing.T) {
	type profile struct {
		name        string
		nodes       int
		density     float64
		churn       float64
		entryCount  int
		graphs      int
		deltasEach  int
	}
	profiles := []profile{
		{"sparse-single-entry", 15, 0.08, 0.2, 1, 200, 30},
		{"dense-single-entry", 15, 0.35, 0.2, 1, 100, 30},
		{"sparse-multi-entry", 25, 0.05, 0.25, 4, 150, 30},
		{"dense-multi-entry", 20, 0.3, 0.3, 3, 100, 30},
		{"tiny-high-churn", 6, 0.3, 0.6, 1, 100, 40},
	}

	for _, p := range profiles {
		t.Run(p.name, func(t *testing.T) {
			for g := 0; g < p.graphs; g++ {
				seed := int64(g)*7919 + int64(len(p.name))
				rng := rand.New(rand.NewSource(seed))

				nodes := make([]string, p.nodes)
				for i := range nodes {
					nodes[i] = fmt.Sprintf("n%d", i)
				}
				var entryPoints []string
				for i := 0; i < p.entryCount; i++ {
					entryPoints = append(entryPoints, fmt.Sprintf("n%d", i))
				}

				edges := randomEdgeSet(rng, nodes, p.density)
				reachableIncremental := reach.Reachable(edges, entryPoints)

				for d := 0; d < p.deltasEach; d++ {
					newEdges := mutateEdges(rng, nodes, edges, p.churn)

					groundTruth := reach.Reachable(newEdges, entryPoints)
					incremental := reach.IncrementalUpdate(entryPoints, edges, newEdges, reachableIncremental)

					if !sameSet(groundTruth, incremental) {
						t.Fatalf("profile %s graph %d (seed %d) delta %d: MISMATCH\n  entry points: %v\n  old edges: %v\n  new edges: %v\n  old reachable (incremental's view): %v\n  ground truth (full recompute):      %v\n  incremental result:                 %v",
							p.name, g, seed, d, entryPoints, edges, newEdges,
							sortedKeys(reachableIncremental), sortedKeys(groundTruth), sortedKeys(incremental))
					}

					edges = newEdges
					reachableIncremental = incremental // feed forward, don't reset to ground truth
				}
			}
		})
	}
}

// TestIncrementalUpdate_DegenerateCases covers hand-picked edge cases a
// random fuzzer might rarely hit: empty graphs, self-loops, an entry
// point with no edges at all, and a node that becomes fully disconnected.
func TestIncrementalUpdate_DegenerateCases(t *testing.T) {
	cases := []struct {
		name         string
		entryPoints  []string
		oldEdges     []reach.Edge
		newEdges     []reach.Edge
	}{
		{
			name:        "empty graph, no edges ever",
			entryPoints: []string{"main"},
			oldEdges:    nil,
			newEdges:    nil,
		},
		{
			name:        "self-loop on entry point",
			entryPoints: []string{"main"},
			oldEdges:    []reach.Edge{{Source: "main", Target: "main"}},
			newEdges:    []reach.Edge{{Source: "main", Target: "main"}},
		},
		{
			name:        "self-loop on a non-entry node, then its only inbound edge removed",
			entryPoints: []string{"main"},
			oldEdges: []reach.Edge{
				{Source: "main", Target: "A"},
				{Source: "A", Target: "A"},
			},
			newEdges: []reach.Edge{
				{Source: "A", Target: "A"},
			},
		},
		{
			name:        "all edges removed at once",
			entryPoints: []string{"main"},
			oldEdges: []reach.Edge{
				{Source: "main", Target: "A"},
				{Source: "A", Target: "B"},
				{Source: "B", Target: "C"},
			},
			newEdges: nil,
		},
		{
			name:        "diamond: two independent paths, only one removed",
			entryPoints: []string{"main"},
			oldEdges: []reach.Edge{
				{Source: "main", Target: "A"},
				{Source: "main", Target: "B"},
				{Source: "A", Target: "C"},
				{Source: "B", Target: "C"},
			},
			newEdges: []reach.Edge{
				{Source: "main", Target: "B"},
				{Source: "A", Target: "C"}, // A itself is now unreachable (main->A gone) but this edge still exists in the fact set
				{Source: "B", Target: "C"},
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			oldReachable := reach.Reachable(c.oldEdges, c.entryPoints)
			groundTruth := reach.Reachable(c.newEdges, c.entryPoints)
			got := reach.IncrementalUpdate(c.entryPoints, c.oldEdges, c.newEdges, oldReachable)
			if !sameSet(groundTruth, got) {
				t.Fatalf("mismatch:\n  ground truth: %v\n  incremental:  %v", sortedKeys(groundTruth), sortedKeys(got))
			}
		})
	}
}

func randomEdgeSet(rng *rand.Rand, nodes []string, density float64) []reach.Edge {
	var edges []reach.Edge
	for _, s := range nodes {
		for _, t := range nodes {
			if s == t {
				continue
			}
			if rng.Float64() < density {
				edges = append(edges, reach.Edge{Source: s, Target: t})
			}
		}
	}
	return edges
}

// mutateEdges returns a new edge slice: each existing edge survives with
// probability (1-churn), and new random edges are added with probability
// churn per possible (source,target) pair not already present.
func mutateEdges(rng *rand.Rand, nodes []string, edges []reach.Edge, churn float64) []reach.Edge {
	present := make(map[reach.Edge]bool, len(edges))
	for _, e := range edges {
		present[e] = true
	}

	var result []reach.Edge
	for _, e := range edges {
		if rng.Float64() >= churn { // survives
			result = append(result, e)
		}
	}
	for _, s := range nodes {
		for _, t := range nodes {
			if s == t {
				continue
			}
			e := reach.Edge{Source: s, Target: t}
			if present[e] {
				continue
			}
			if rng.Float64() < churn*0.3 {
				result = append(result, e)
			}
		}
	}
	return result
}

func sameSet(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
