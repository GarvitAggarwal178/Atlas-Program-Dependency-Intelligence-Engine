package index

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/yourorg/symex/internal/reach"
	"github.com/yourorg/symex/internal/store"
)

// CallEdges extracts CALL-kind edges from a live fact set, filtering out
// IMPLEMENTS facts (not traversal edges) — shared by both the full-recompute
// and incremental reachability paths.
func CallEdges(facts []store.Fact) []reach.Edge {
	var edges []reach.Edge
	for _, f := range facts {
		if f.Kind != store.FactKindCall {
			continue
		}
		edges = append(edges, reach.Edge{Source: f.SourceSymbol, Target: f.TargetSymbol})
	}
	return edges
}

// SnapshotReachabilityState reads the current live CALL edges and live
// reachable_symbols for repo — the "before" picture MaintainReachabilityIncremental
// needs, which must be captured BEFORE ApplyFacts mutates the fact set
// within the same transaction.
func SnapshotReachabilityState(ctx context.Context, tx *sql.Tx, repo string) (edges []reach.Edge, reachable map[string]bool, err error) {
	liveFacts, err := store.QueryLiveFacts(ctx, tx, repo)
	if err != nil {
		return nil, nil, fmt.Errorf("query live facts for reachability snapshot: %w", err)
	}
	edges = CallEdges(liveFacts)

	rows, err := store.QueryLiveReachable(ctx, tx, repo)
	if err != nil {
		return nil, nil, fmt.Errorf("query live reachable for snapshot: %w", err)
	}
	reachable = make(map[string]bool, len(rows))
	for _, r := range rows {
		reachable[r.Symbol] = true
	}
	return edges, reachable, nil
}

// applyReachableDiff opens/closes atlas.reachable_symbols to match
// newReachable, given what was live before (oldReachable) — the shared
// tail end of both the full-recompute and incremental paths.
func applyReachableDiff(ctx context.Context, tx *sql.Tx, repo string, seq int64, edges []reach.Edge, entryPoints []string, oldReachable, newReachable map[string]bool) (opened, closed int, err error) {
	supportCounts := reach.SupportCounts(edges, newReachable, entryPoints)

	for sym := range newReachable {
		if oldReachable[sym] {
			continue
		}
		if err := store.OpenReachable(ctx, tx, repo, sym, supportCounts[sym], seq); err != nil {
			return opened, closed, fmt.Errorf("open reachable %s: %w", sym, err)
		}
		opened++
	}

	for sym := range oldReachable {
		if newReachable[sym] {
			continue
		}
		if err := store.CloseReachable(ctx, tx, repo, sym, seq); err != nil {
			return opened, closed, fmt.Errorf("close reachable %s: %w", sym, err)
		}
		closed++
	}

	return opened, closed, nil
}

// MaintainReachability recomputes reachability from entryPoints over the
// currently-live CALL edges (which must already reflect this commit — call
// this AFTER ApplyFacts within the same ApplyDelta transaction) via a full
// BFS recompute, and applies the result to atlas.reachable_symbols.
//
// Kept alongside MaintainReachabilityIncremental deliberately: it's the
// simplest-possible-correct reference implementation, useful as a
// differential check and for any caller that doesn't have (or doesn't want
// to bother capturing) a "before" snapshot.
//
// Must be called with the *sql.Tx from an in-flight ApplyDelta call.
func MaintainReachability(ctx context.Context, tx *sql.Tx, repo string, seq int64, entryPoints []string) (opened, closed int, err error) {
	liveFacts, err := store.QueryLiveFacts(ctx, tx, repo)
	if err != nil {
		return 0, 0, fmt.Errorf("query live facts for reachability: %w", err)
	}
	edges := CallEdges(liveFacts)
	newReachable := reach.Reachable(edges, entryPoints)

	oldRows, err := store.QueryLiveReachable(ctx, tx, repo)
	if err != nil {
		return 0, 0, fmt.Errorf("query live reachable: %w", err)
	}
	oldReachable := make(map[string]bool, len(oldRows))
	for _, r := range oldRows {
		oldReachable[r.Symbol] = true
	}

	return applyReachableDiff(ctx, tx, repo, seq, edges, entryPoints, oldReachable, newReachable)
}

// MaintainReachabilityIncremental is architecture.md section 4.3's actual
// DRed mechanism, wired to the store: given the PRE-delta edge set and
// reachable set (captured via SnapshotReachabilityState BEFORE ApplyFacts
// runs), it reads the POST-delta live CALL edges and calls
// reach.IncrementalUpdate — over-delete, rederive, insert — instead of a
// full BFS recompute, then applies the result the same way
// MaintainReachability does.
//
// Trusted to match MaintainReachability's result exactly, per
// internal/reach's differential fuzz test (~20,000 random-graph
// comparisons, results fed forward across sequences, never a mismatch) —
// see docs/DECISIONS.md for the full validation writeup before this was
// wired in as the production path.
//
// Must be called with the *sql.Tx from an in-flight ApplyDelta call, AFTER
// ApplyFacts has already run in the same transaction.
func MaintainReachabilityIncremental(
	ctx context.Context, tx *sql.Tx, repo string, seq int64, entryPoints []string,
	oldEdges []reach.Edge, oldReachable map[string]bool,
) (opened, closed int, err error) {
	liveFacts, err := store.QueryLiveFacts(ctx, tx, repo)
	if err != nil {
		return 0, 0, fmt.Errorf("query live facts for reachability: %w", err)
	}
	newEdges := CallEdges(liveFacts)

	newReachable := reach.IncrementalUpdate(entryPoints, oldEdges, newEdges, oldReachable)

	return applyReachableDiff(ctx, tx, repo, seq, newEdges, entryPoints, oldReachable, newReachable)
}
