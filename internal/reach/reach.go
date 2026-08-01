// Package reach implements architecture.md section 4's least-fixpoint
// reachability: which symbols are reachable from an entry-point set,
// forward over the call graph.
//
// Section 4.2's point, stated precisely: the semantics is the LEAST
// fixpoint (Knaster-Tarski, since the program is monotone — no negation in
// the recursive cycle), and naive support counting maintains A fixpoint
// that is not the least one. A mutually recursive pair (A calls B, B calls
// A) reachable from main only through main->A: delete that edge, and
// support counting keeps both A and B "reachable" forever, because each
// still has support from the other. The least fixpoint has neither
// reachable — there's no path from any entry point once main->A is gone.
//
// Reachable below computes the least fixpoint directly (BFS from entry
// points visits exactly what's actually reachable, full stop — a
// self-supporting cycle with no live entry edge is simply never visited).
// NaiveSupportCounting is the REJECTED mechanism, kept only so a test can
// demonstrate it fails on exactly this case before anything in the real
// path uses the correct one.
package reach

// Edge is a directed call-graph edge for reachability purposes. Only the
// endpoints matter here — provenance, source file, etc. belong to the
// caller's fact model, not this package.
type Edge struct {
	Source string
	Target string
}

// Reachable returns every symbol reachable from entryPoints by following
// edges forward, including the entry points themselves. This is the least
// fixpoint of the reachability relation over edges — computed directly via
// BFS, not maintained incrementally (see NaiveSupportCounting for the
// mechanism that tries to maintain it incrementally and gets it wrong).
func Reachable(edges []Edge, entryPoints []string) map[string]bool {
	adj := make(map[string][]string)
	for _, e := range edges {
		adj[e.Source] = append(adj[e.Source], e.Target)
	}

	visited := make(map[string]bool)
	queue := make([]string, 0, len(entryPoints))
	for _, ep := range entryPoints {
		if !visited[ep] {
			visited[ep] = true
			queue = append(queue, ep)
		}
	}

	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, next := range adj[cur] {
			if !visited[next] {
				visited[next] = true
				queue = append(queue, next)
			}
		}
	}
	return visited
}

// SupportCounts returns, for every symbol in reachable, the number of live
// edges into it from another reachable symbol (entry points get an
// implicit +1 of baseline support so they never read as unsupported).
// This is purely descriptive — populates reachable_symbols.support_count
// for diagnostic/debugging value — Reachable's BFS result is what actually
// decides membership, not this count.
func SupportCounts(edges []Edge, reachable map[string]bool, entryPoints []string) map[string]int {
	counts := make(map[string]int, len(reachable))
	for sym := range reachable {
		counts[sym] = 0
	}
	for _, ep := range entryPoints {
		if reachable[ep] {
			counts[ep]++
		}
	}
	for _, e := range edges {
		if reachable[e.Source] && reachable[e.Target] {
			counts[e.Target]++
		}
	}
	return counts
}

// NaiveSupportCounting simulates the REJECTED mechanism architecture.md
// section 4.2 warns against and section 4.4's fixture is built to catch:
// maintaining reachability incrementally by counting supporting edges per
// node, decrementing on delete, and only dropping a node when its count
// hits zero. This does not exist anywhere in the real maintenance path —
// internal/index's reachability wiring uses Reachable (a correct BFS
// recompute), never this. Kept ONLY so a test can demonstrate the failure
// mode directly rather than taking architecture.md's word for it.
type NaiveSupportCounting struct {
	support   map[string]int
	reachable map[string]bool
}

// NewNaiveSupportCounting seeds the simulation with entryPoints as always
// reachable, with baseline support so they never get evicted by a delete.
func NewNaiveSupportCounting(entryPoints []string) *NaiveSupportCounting {
	s := &NaiveSupportCounting{
		support:   make(map[string]int),
		reachable: make(map[string]bool),
	}
	for _, ep := range entryPoints {
		s.reachable[ep] = true
		s.support[ep] = 1
	}
	return s
}

// InsertEdge adds source->target. If source is reachable, target gains
// support and becomes reachable (this part matches the correct semantics
// — insertion is monotone and naive counting gets insertion right; it's
// deletion that's wrong).
func (s *NaiveSupportCounting) InsertEdge(source, target string) {
	if !s.reachable[source] {
		return
	}
	s.support[target]++
	s.reachable[target] = true
}

// DeleteEdge removes source->target and decrements target's support. If
// target's support hits zero, naive counting marks it unreachable — but
// does NOT transitively re-check whatever target itself was supporting.
// That's the bug: if target was propping up a cycle partner that in turn
// props target back up, the cycle's members keep each other's count above
// zero forever, even with no path from any entry point.
func (s *NaiveSupportCounting) DeleteEdge(source, target string) {
	if !s.reachable[source] {
		return
	}
	s.support[target]--
	if s.support[target] <= 0 {
		delete(s.reachable, target)
	}
}

// Reachable returns the current (possibly wrong) reachable set.
func (s *NaiveSupportCounting) Reachable() map[string]bool {
	result := make(map[string]bool, len(s.reachable))
	for k := range s.reachable {
		result[k] = true
	}
	return result
}
