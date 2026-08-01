package reach

// IncrementalUpdate implements architecture.md section 4.3's actual DRed
// mechanism — over-delete, rederive, insert — instead of Reachable's full
// BFS recompute. Given the OLD edge set and reachable set (from before
// this delta) and the NEW edge set (after), it computes the new reachable
// set while only doing work proportional to the AFFECTED region of the
// graph, not the whole thing.
//
// # Why a naive translation of the three phases isn't actually incremental
//
// It's tempting to write "phase 2/3 = BFS from (old reachable minus dead)
// over the new edges" and call it done. That is NOT incremental — a
// multi-source BFS over a graph costs the same O(V+E) as a single-source
// one, so re-expanding every surviving node's neighbors touches the same
// amount of graph as recomputing from entry points directly. The actual
// efficiency gain requires a sharper claim:
//
//   - A node NOT marked dead by phase 1 is reachable via a path that
//     provably does not pass through any removed edge (phase 1's
//     propagation marks everything downstream of a removed edge, so if a
//     node survives, none of its old support chain was touched). It does
//     NOT need to be re-verified — it can be accepted as still reachable
//     without re-walking its neighbors.
//   - Phase 2 only needs to ask, for each node phase 1 pessimistically
//     killed: does it have some OTHER live incoming edge from a node
//     that's confirmed reachable? This is a fixpoint restricted to the
//     dead set (bounded by |dead|), not the whole graph.
//   - Phase 3 only needs to seed from the TARGETS of genuinely new edges
//     (not present before) whose source is confirmed reachable, then
//     expand forward from there — bounded by the newly-reachable region,
//     not by re-touching everything already known reachable.
//
// Phases 1 and 2 together are the classic over-delete/rederive; phase 3 is
// insert. See internal/index.MaintainReachabilityIncremental for how this
// is driven from the store, and reach_test.go's differential fuzz test for
// why this is trusted to match Reachable's full recompute exactly.
func IncrementalUpdate(entryPoints []string, oldEdges, newEdges []Edge, oldReachable map[string]bool) map[string]bool {
	oldAdj := buildAdj(oldEdges)
	newAdj := buildAdj(newEdges)
	newRevAdj := buildRevAdj(newEdges)
	oldEdgeSet := edgeSet(oldEdges)
	newEdgeSet := edgeSet(newEdges)
	entrySet := toSet(entryPoints)

	// Phase 1: over-delete. Pessimistically mark everything downstream (in
	// the OLD graph) of a removed edge's target as dead, propagating
	// transitively. Entry points are never dead — they're reachable by
	// definition, not by support from another node.
	dead := make(map[string]bool)
	var deadQueue []string
	for _, e := range oldEdges {
		if newEdgeSet[e] {
			continue // edge survives, not a deletion
		}
		if oldReachable[e.Target] && !entrySet[e.Target] && !dead[e.Target] {
			dead[e.Target] = true
			deadQueue = append(deadQueue, e.Target)
		}
	}
	for len(deadQueue) > 0 {
		n := deadQueue[0]
		deadQueue = deadQueue[1:]
		for _, m := range oldAdj[n] {
			if oldReachable[m] && !entrySet[m] && !dead[m] {
				dead[m] = true
				deadQueue = append(deadQueue, m)
			}
		}
	}

	// Nodes that survived phase 1 unmarked are accepted as still reachable
	// WITHOUT re-derivation — this is the load-bearing efficiency claim
	// above.
	result := make(map[string]bool, len(oldReachable))
	for _, ep := range entryPoints {
		result[ep] = true
	}
	for r := range oldReachable {
		if !dead[r] {
			result[r] = true
		}
	}

	// Phase 2: rederive. A dead node recovers if it has a live incoming
	// edge (in the NEW graph) from anything already confirmed — including
	// another dead node that recovered earlier in this same fixpoint.
	// Bounded by |dead|: each dead node is examined repeatedly only while
	// the fixpoint is still making progress, not against the whole graph.
	changed := true
	for changed {
		changed = false
		for d := range dead {
			if result[d] {
				continue
			}
			for _, in := range newRevAdj[d] {
				if result[in] {
					result[d] = true
					changed = true
					break
				}
			}
		}
	}

	// Phase 3: insert. Seed only from targets of genuinely NEW edges
	// (absent from the old edge set) whose source is confirmed reachable
	// — not from the whole confirmed set, which would re-touch everything
	// already known-good. From each newly-discovered node, expand forward
	// over the new graph — unavoidable cost proportional to how much NEW
	// graph actually became reachable, not to the graph's total size.
	var insertQueue []string
	for _, e := range newEdges {
		if oldEdgeSet[e] {
			continue // not a new edge
		}
		if result[e.Source] && !result[e.Target] {
			result[e.Target] = true
			insertQueue = append(insertQueue, e.Target)
		}
	}
	for len(insertQueue) > 0 {
		n := insertQueue[0]
		insertQueue = insertQueue[1:]
		for _, m := range newAdj[n] {
			if !result[m] {
				result[m] = true
				insertQueue = append(insertQueue, m)
			}
		}
	}

	return result
}

func buildAdj(edges []Edge) map[string][]string {
	adj := make(map[string][]string)
	for _, e := range edges {
		adj[e.Source] = append(adj[e.Source], e.Target)
	}
	return adj
}

func buildRevAdj(edges []Edge) map[string][]string {
	adj := make(map[string][]string)
	for _, e := range edges {
		adj[e.Target] = append(adj[e.Target], e.Source)
	}
	return adj
}

func edgeSet(edges []Edge) map[Edge]bool {
	s := make(map[Edge]bool, len(edges))
	for _, e := range edges {
		s[e] = true
	}
	return s
}

func toSet(items []string) map[string]bool {
	s := make(map[string]bool, len(items))
	for _, it := range items {
		s[it] = true
	}
	return s
}
