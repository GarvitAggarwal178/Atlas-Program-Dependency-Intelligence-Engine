// Package reachability implements BFS over the in-memory call graph starting
// from a changed symbol set, producing the full reachable set with
// per-symbol dependency paths and weakest-link provenance.
//
// # Weakest-link provenance — the core design decision
//
// Every edge in the call graph carries a provenance tag: either "direct_call"
// (precise — go/types resolved this to exactly one concrete target) or
// "interface_resolved" (over-approximated — the call was through an interface
// and every implementation was added as a candidate edge).
//
// When traversing a path from a changed symbol to a potentially-affected
// symbol, we track the weakest provenance seen on any edge along the way:
//
//	direct_call + direct_call + direct_call  →  "direct_call"  (high confidence)
//	direct_call + interface_resolved + ...   →  "interface_resolved" (reduced confidence)
//
// Why weakest-link instead of averaging or majority-vote?
//
// A path represents a causal chain: "if A changes, and A calls B, and B calls
// C, then C might be affected." The confidence in "C is affected" is bounded
// by the confidence in the weakest link in that chain. If even one edge on the
// path is an over-approximation (interface_resolved), the entire conclusion
// "C is reachable from A" rests on that over-approximation. Averaging would
// imply that many precise edges compensate for one imprecise one — but they
// don't. A chain with one uncertain link is an uncertain chain, regardless of
// how certain the other links are. This is equivalent to the rule that the
// probability of a conjunction is bounded by its least probable term.
//
// In practice this means: if you call through an interface once on the path
// from a changed function to a test, that test goes in the "consider running"
// tier, not "must run". This is intentionally conservative — it reflects the
// actual uncertainty in our static analysis.
package reachability

import (
	"github.com/yourorg/symex/internal/callgraph"
)

// Provenance constants mirror the store layer values.
const (
	ProvenanceDirect            = "direct_call"
	ProvenanceInterfaceResolved = "interface_resolved"
)

// provenanceRank assigns a numeric rank so we can find the "weakest" — lower
// rank means weaker (less confident).
var provenanceRank = map[string]int{
	ProvenanceInterfaceResolved: 0, // weakest
	ProvenanceDirect:            1, // strongest
}

// weakerOf returns whichever provenance is weaker (less confident).
// If either value is unknown, interface_resolved is returned as the safe default.
func weakerOf(a, b string) string {
	ra, aok := provenanceRank[a]
	rb, bok := provenanceRank[b]
	if !aok || !bok {
		return ProvenanceInterfaceResolved
	}
	if ra <= rb {
		return a
	}
	return b
}

// ReachedSymbol describes a single symbol that the BFS reached from the
// changed symbol frontier.
type ReachedSymbol struct {
	// Symbol is the fully-qualified name of the reachable symbol.
	Symbol string

	// Path is the ordered list of symbols traversed to reach this one,
	// starting at the changed symbol and ending at Symbol itself.
	// e.g. ["pkg.ChangedFunc", "pkg.CalledFunc", "pkg.ReachedFunc"]
	Path []string

	// Provenance is the weakest-link provenance along Path:
	// "direct_call" if every edge on the path was a direct call,
	// "interface_resolved" if any edge was through an interface.
	Provenance string

	// Depth is len(Path) - 1: the number of edges traversed.
	Depth int
}

// Result is the complete output of a reachability query.
type Result struct {
	// ChangedSymbols is the frontier the BFS started from.
	ChangedSymbols []string

	// Reached is every symbol reachable from the frontier (excluding the
	// frontier itself, which is always "must run" regardless).
	Reached []ReachedSymbol

	// TotalNodes is the count of distinct symbols reached.
	TotalNodes int

	// DirectCount is the count of symbols reached via all-direct paths.
	DirectCount int

	// InterfaceResolvedCount is the count reached via at least one
	// interface_resolved edge.
	InterfaceResolvedCount int
}

// bfsState tracks the in-progress state for a single symbol during BFS.
type bfsState struct {
	path       []string // path from frontier to this symbol (inclusive)
	provenance string   // weakest-link provenance so far
}

// Run performs BFS from the changed symbol set over the in-memory call graph,
// returning the full reachable set with paths and provenance.
//
// The BFS is forward: it follows outgoing edges (caller→callee direction).
// This correctly models impact propagation: a change in A can affect anything
// that A calls, transitively.
//
// Self-loops are handled: a symbol is only visited once (the first time it is
// reached, which gives the shortest path and therefore the most optimistic
// provenance — if a symbol is reachable via both a direct path and an
// interface-resolved path, we record the direct path since BFS finds shortest
// paths first).
func Run(graph *callgraph.InMemoryGraph, changedSymbols map[string]string) *Result {
	result := &Result{}

	for sym := range changedSymbols {
		result.ChangedSymbols = append(result.ChangedSymbols, sym)
	}

	// visited tracks symbols we've already enqueued, to prevent revisiting.
	// Value is the bfsState at first encounter (shortest path, best provenance).
	visited := make(map[string]bfsState)

	// Seed visited with the frontier so we don't loop back into it.
	for sym := range changedSymbols {
		visited[sym] = bfsState{
			path:       []string{sym},
			provenance: ProvenanceDirect, // frontier nodes have no edges yet
		}
	}

	// BFS queue: each entry is a (symbol, current path, current provenance).
	type qEntry struct {
		symbol     string
		path       []string
		provenance string
	}
	queue := make([]qEntry, 0, len(changedSymbols))
	for sym := range changedSymbols {
		queue = append(queue, qEntry{
			symbol:     sym,
			path:       []string{sym},
			provenance: ProvenanceDirect,
		})
	}

	for len(queue) > 0 {
		// Dequeue.
		cur := queue[0]
		queue = queue[1:]

		// Walk outgoing edges from cur.symbol.
		for _, edge := range graph.Edges[cur.symbol] {
			target := edge.TargetSymbol

			// Skip self-loops.
			if target == cur.symbol {
				continue
			}

			// Compute what provenance this edge contributes.
			edgeProvenance := edge.Provenance
			pathProvenance := weakerOf(cur.provenance, edgeProvenance)

			// Build the new path.
			newPath := make([]string, len(cur.path)+1)
			copy(newPath, cur.path)
			newPath[len(cur.path)] = target

			if _, seen := visited[target]; seen {
				// Already visited — BFS guarantees the first visit is shortest,
				// so we don't update the state. This also means provenance is
				// for the shortest path, which is the most optimistic
				// (fewest interface_resolved hops).
				continue
			}

			visited[target] = bfsState{
				path:       newPath,
				provenance: pathProvenance,
			}

			queue = append(queue, qEntry{
				symbol:     target,
				path:       newPath,
				provenance: pathProvenance,
			})
		}
	}

	// Build the result slice, excluding the frontier symbols themselves.
	for sym, state := range visited {
		if _, isFrontier := changedSymbols[sym]; isFrontier {
			continue
		}
		rs := ReachedSymbol{
			Symbol:     sym,
			Path:       state.path,
			Provenance: state.provenance,
			Depth:      len(state.path) - 1,
		}
		result.Reached = append(result.Reached, rs)
		result.TotalNodes++
		if state.provenance == ProvenanceDirect {
			result.DirectCount++
		} else {
			result.InterfaceResolvedCount++
		}
	}

	return result
}
