package index

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/yourorg/symex/internal/parser"
	"github.com/yourorg/symex/internal/store"
)

// Stats reports what ApplyFacts / IndexCommit did.
type Stats struct {
	Opened    int // newly-live facts
	Closed    int // facts withdrawn (present at the prior seq, absent now)
	Unchanged int // facts live at both the prior seq and now (derivations refreshed)
}

// naturalKey mirrors atlas.facts_live_uniq exactly: (repo, kind,
// source_symbol, target_symbol, provenance, call_site, source_module).
func naturalKey(repo string, f store.Fact) string {
	return strings.Join([]string{repo, f.Kind, f.SourceSymbol, f.TargetSymbol, f.Provenance, f.CallSite, f.SourceModule}, "\x00")
}

// ApplyFacts diffs newFacts against the currently-live fact set for repo
// and applies the difference: opens facts newly present, closes facts no
// longer present (setting valid_to = seq), and re-records derivations for
// facts present in both (refreshing input hashes — a fact can be present
// in both the old and new commit under an unchanged natural key while one
// of its recorded inputs' hash still moved, e.g. a comment-only edit to
// the source file; keeping derivations current is what lets a LATER
// commit's StaleLiveFacts call be accurate).
//
// changedFiles scopes the work to what actually needs touching: a fact is
// only considered for opening/closing/refreshing if its own source file is
// in changedFiles, OR (for interface_resolved facts) one of the interfaces
// it dispatches through just changed implementer set — everything else is
// left completely alone, no DB write at all. This is the actual section
// 2.2 payoff: StaleLiveFacts drives what gets touched instead of
// recomputing and diffing the whole live set every commit. Pass nil (or
// every file in the table) to force a full pass, e.g. for the first commit
// of a repo where nothing is live yet.
//
// Must be called with the *sql.Tx from an in-flight ApplyDelta call — see
// interval.go's and derivation.go's doc comments for why fact/derivation
// writes never accept a bare *sql.DB.
func ApplyFacts(ctx context.Context, tx *sql.Tx, repo string, seq int64, newFacts []FactWithDerivations, changedFiles map[string]bool) (Stats, error) {
	var stats Stats

	// Interface implementer sets are maintained regardless of scoping —
	// this is cheap (one row per interface) and is how we find OUT which
	// interfaces changed in the first place.
	ifaceHashes := make(map[string]string)
	for _, nf := range newFacts {
		for _, d := range nf.Derivations {
			if d.InputKind == store.InputKindInterface {
				ifaceHashes[d.InputKey] = d.InputHash
			}
		}
	}
	changedInterfaces := make(map[string]bool)
	for iface, hash := range ifaceHashes {
		changed, err := store.UpsertInterfaceImplementers(ctx, tx, repo, iface, hash, seq)
		if err != nil {
			return stats, fmt.Errorf("upsert interface_implementers for %s: %w", iface, err)
		}
		if changed {
			changedInterfaces[iface] = true
		}
	}

	// Close-candidates: live facts in a changed file, plus live facts
	// anywhere whose recorded INTERFACE hash no longer matches (cross-file
	// impact of a changed implementer set).
	liveByKey := make(map[string]store.Fact)
	if len(changedFiles) > 0 {
		files := make([]string, 0, len(changedFiles))
		for f := range changedFiles {
			files = append(files, f)
		}
		rows, err := store.LiveFactsForFiles(ctx, tx, repo, files)
		if err != nil {
			return stats, fmt.Errorf("live facts for changed files: %w", err)
		}
		for _, f := range rows {
			liveByKey[naturalKey(repo, f)] = f
		}
	}
	for iface := range changedInterfaces {
		rows, err := store.StaleLiveFacts(ctx, tx, repo, store.InputKindInterface, iface, ifaceHashes[iface])
		if err != nil {
			return stats, fmt.Errorf("stale live facts for %s: %w", iface, err)
		}
		for _, f := range rows {
			liveByKey[naturalKey(repo, f)] = f
		}
	}

	// Open-candidates: new facts whose own file changed, or that dispatch
	// through a changed interface.
	touched := func(nf FactWithDerivations) bool {
		if changedFiles[nf.Fact.SourceFile] {
			return true
		}
		for _, d := range nf.Derivations {
			if d.InputKind == store.InputKindInterface && changedInterfaces[d.InputKey] {
				return true
			}
		}
		return false
	}

	seenKeys := make(map[string]bool, len(newFacts))

	for _, nf := range newFacts {
		if !touched(nf) {
			continue
		}
		nf.Fact.Repo = repo
		nf.Fact.ValidFrom = seq
		key := naturalKey(repo, nf.Fact)
		if seenKeys[key] {
			// Two references producing the identical natural key within the
			// same commit (should not normally happen — call_site includes
			// the line number — but if it does, the second is a harmless
			// duplicate, not a new fact).
			continue
		}
		seenKeys[key] = true

		if existing, ok := liveByKey[key]; ok {
			// Unchanged natural key: refresh derivations against the
			// existing fact_id.
			for i := range nf.Derivations {
				nf.Derivations[i].FactID = existing.FactID
			}
			if err := store.RecordDerivations(ctx, tx, nf.Derivations); err != nil {
				return stats, fmt.Errorf("refresh derivations for %s: %w", key, err)
			}
			stats.Unchanged++
			continue
		}

		factID, err := store.OpenFact(ctx, tx, nf.Fact)
		if err != nil {
			return stats, fmt.Errorf("open fact %s->%s: %w", nf.Fact.SourceSymbol, nf.Fact.TargetSymbol, err)
		}
		for i := range nf.Derivations {
			nf.Derivations[i].FactID = factID
		}
		if err := store.RecordDerivations(ctx, tx, nf.Derivations); err != nil {
			return stats, fmt.Errorf("record derivations for new fact %s->%s: %w", nf.Fact.SourceSymbol, nf.Fact.TargetSymbol, err)
		}
		stats.Opened++
	}

	for key, existing := range liveByKey {
		if seenKeys[key] {
			continue
		}
		if err := store.CloseFactByID(ctx, tx, existing.FactID, seq); err != nil {
			return stats, fmt.Errorf("close withdrawn fact %s->%s: %w", existing.SourceSymbol, existing.TargetSymbol, err)
		}
		stats.Closed++
	}

	return stats, nil
}

// diffFileHashes returns the set of files that are new, removed, or whose
// content hash changed between old (what's currently recorded live) and
// new (what was just computed). For a fresh repo (old is empty), every
// file in new comes back changed — which is correct, everything needs
// opening.
func diffFileHashes(old, new map[string]string) map[string]bool {
	changed := make(map[string]bool)
	for f, h := range new {
		if oldH, ok := old[f]; !ok || oldH != h {
			changed[f] = true
		}
	}
	for f := range old {
		if _, ok := new[f]; !ok {
			changed[f] = true // file deleted since last commit
		}
	}
	return changed
}

// IndexCommitFromRepo is the full pipeline for one commit: load packages,
// run the poison-input gate (architecture.md section 3.2), and — only if
// clean — compute and apply facts. On a poison commit, it records the skip
// in atlas.skipped_commits and returns (nil, nil): a poison commit is an
// EXPECTED, handled outcome, not an error the caller needs to branch on
// specially — check the returned *Stats for nil to detect a skip.
func IndexCommitFromRepo(
	ctx context.Context, db *store.DB,
	repoRoot, modulePath, repo string, seq int64, fingerprint string,
) (*Stats, error) {
	pkgs, fset, err := parser.LoadPackages(repoRoot)
	if err != nil {
		return nil, fmt.Errorf("load packages: %w", err)
	}

	poison := parser.CheckPoison(pkgs)
	if !poison.Clean {
		if err := db.RecordSkippedCommit(ctx, store.SkippedCommit{
			Repo: repo, Seq: seq, Reason: poison.Reason, Detail: poison.Detail,
		}); err != nil {
			return nil, fmt.Errorf("record skipped commit: %w", err)
		}
		return nil, nil
	}

	table := parser.BuildSymbolTable(pkgs, fset, repoRoot, modulePath)

	facts, newHashes, err := ComputeFacts(repoRoot, modulePath, table)
	if err != nil {
		return nil, fmt.Errorf("compute facts: %w", err)
	}

	oldHashes, err := store.LiveFileHashes(ctx, db.RawDB(), repo)
	if err != nil {
		return nil, fmt.Errorf("live file hashes: %w", err)
	}
	changedFiles := diffFileHashes(oldHashes, newHashes)

	var stats Stats
	err = db.ApplyDelta(ctx, repo, seq, fingerprint, func(ctx context.Context, tx *sql.Tx) error {
		s, err := ApplyFacts(ctx, tx, repo, seq, facts, changedFiles)
		stats = s
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("apply delta seq=%d: %w", seq, err)
	}
	return &stats, nil
}
