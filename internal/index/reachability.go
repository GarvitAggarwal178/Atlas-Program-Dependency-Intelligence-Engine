package index

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/yourorg/symex/internal/reach"
	"github.com/yourorg/symex/internal/store"
)

// MaintainReachability recomputes reachability from entryPoints over the
// currently-live CALL edges (which must already reflect this commit — call
// this AFTER ApplyFacts within the same ApplyDelta transaction) and
// applies the result to atlas.reachable_symbols via the same open/close
// interval diff every other derived relation in this codebase uses.
//
// This is a full BFS recompute every call, not yet the incremental
// over-delete/rederive DRed algorithm architecture.md section 4.3
// describes (phases: over-delete, rederive, insert) — see
// docs/DECISIONS.md for why that's a deliberate, documented scope
// decision rather than an oversight. What IS built and proven (section
// 4.4's fixture, internal/reach) is the CORRECT semantics: least-fixpoint
// reachability, not naive support counting. A full recompute of the
// correct fixpoint is always correct; it just isn't the efficient
// maintenance mechanism yet.
//
// Must be called with the *sql.Tx from an in-flight ApplyDelta call.
func MaintainReachability(ctx context.Context, tx *sql.Tx, repo string, seq int64, entryPoints []string) (opened, closed int, err error) {
	liveFacts, err := store.QueryLiveFacts(ctx, tx, repo)
	if err != nil {
		return 0, 0, fmt.Errorf("query live facts for reachability: %w", err)
	}

	var edges []reach.Edge
	for _, f := range liveFacts {
		if f.Kind != store.FactKindCall {
			continue // reachability is a call-graph concept; IMPLEMENTS facts aren't traversal edges
		}
		edges = append(edges, reach.Edge{Source: f.SourceSymbol, Target: f.TargetSymbol})
	}

	newReachable := reach.Reachable(edges, entryPoints)
	supportCounts := reach.SupportCounts(edges, newReachable, entryPoints)

	oldRows, err := store.QueryLiveReachable(ctx, tx, repo)
	if err != nil {
		return 0, 0, fmt.Errorf("query live reachable: %w", err)
	}
	oldReachable := make(map[string]bool, len(oldRows))
	for _, r := range oldRows {
		oldReachable[r.Symbol] = true
	}

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
