// Package incremental — differential_harness.go
//
// The differential test harness is the proof that incremental == full rebuild.
// It walks a real repo's commit history and for each commit:
//
//  (a) Applies the incremental update from the previous commit's graph.
//  (b) Runs a full rebuild from scratch at that commit.
//  (c) Loads both graphs and diffs their edge sets.
//  (d) Reports PASS if the sets are identical, FAIL + exact diff otherwise.
//
// This is not a theoretical correctness argument — it's empirical verification
// across real commit history.

package incremental

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/yourorg/symex/internal/callgraph"
	"github.com/yourorg/symex/internal/modpath"
	"github.com/yourorg/symex/internal/parser"
	"github.com/yourorg/symex/internal/store"
)

// CommitResult is the outcome of one commit's differential check.
type CommitResult struct {
	// Commit is the SHA being tested.
	Commit string
	// BaseCommit is the parent commit.
	BaseCommit string
	// Pass is true if incremental graph == full rebuild graph.
	Pass bool
	// OnlyInIncremental are edges present in the incremental graph but not full.
	OnlyInIncremental []store.Edge
	// OnlyInFull are edges present in the full graph but not incremental.
	OnlyInFull []store.Edge
	// ChangedFiles is the list of .go files that changed in this commit.
	ChangedFiles []string
	// FullBuildTime is how long the full rebuild took.
	FullBuildTime time.Duration
	// IncrementalTime is how long the incremental update took.
	IncrementalTime time.Duration
	// Error is non-nil if the harness itself failed (not a graph mismatch).
	Error error
}

// HarnessResult is the aggregate output of a full harness run.
type HarnessResult struct {
	Repo          string
	TotalCommits  int
	PassCount     int
	FailCount     int
	ErrorCount    int
	Results       []CommitResult
	AvgSpeedup    float64 // IncrementalTime / FullBuildTime ratio
}

// RunHarness runs the differential harness against a repo's commit history.
//
// repoDir must be the path to a local git clone.
// maxCommits caps how many commits to test (use 0 for no cap; recommend 30).
// The graph for each commit is stored in Postgres under a unique namespace to
// avoid conflicts between full and incremental versions.
func RunHarness(
	ctx context.Context,
	db *store.DB,
	repoDir string,
	maxCommits int,
) (*HarnessResult, error) {
	_, modulePath, err := modpath.FindModulePath(repoDir)
	if err != nil {
		return nil, fmt.Errorf("find module path: %w", err)
	}

	// Get commit history: list of SHAs from oldest to newest.
	commits, err := listCommits(repoDir, maxCommits)
	if err != nil {
		return nil, fmt.Errorf("list commits: %w", err)
	}
	if len(commits) < 2 {
		return nil, fmt.Errorf("need at least 2 commits, got %d", len(commits))
	}

	fmt.Printf("Harness: %d commits, repo=%s module=%s\n\n", len(commits)-1, repoDir, modulePath)

	result := &HarnessResult{
		Repo:         repoDir,
		TotalCommits: len(commits) - 1, // first commit has no parent
	}

	// ── Seed: full build at the first commit ─────────────────────────────────
	firstCommit := commits[0]
	fmt.Printf("Building initial graph at %s ...\n", shortSHA(firstCommit))

	if err := checkoutCommit(repoDir, firstCommit); err != nil {
		return nil, fmt.Errorf("checkout %s: %w", firstCommit, err)
	}

	table, err := parser.ParseRepo(repoDir, modulePath)
	if err != nil {
		return nil, fmt.Errorf("parse repo at %s: %w", firstCommit, err)
	}

	// "incr:" namespace = the incrementally-maintained chain.
	// "full:" namespace = fresh full rebuild at each commit.
	// Both start from the same full build at the first commit.
	incrRepo := "incr:" + repoDir
	fullRepo := "full:" + repoDir

	if _, err := callgraph.BuildV2(ctx, db, table, incrRepo, firstCommit); err != nil {
		return nil, fmt.Errorf("seed incremental graph: %w", err)
	}
	if _, err := callgraph.BuildV2(ctx, db, table, fullRepo, firstCommit); err != nil {
		return nil, fmt.Errorf("seed full graph: %w", err)
	}

	prevCommit := firstCommit

	// ── Main loop: for each subsequent commit ─────────────────────────────────
	for i := 1; i < len(commits); i++ {
		commit := commits[i]
		cr := CommitResult{
			Commit:     commit,
			BaseCommit: prevCommit,
		}

		fmt.Printf("[%d/%d] %s..%s\n", i, len(commits)-1,
			shortSHA(prevCommit), shortSHA(commit))

		// Find changed .go files between prevCommit and commit.
		changedFiles, err := changedGoFiles(repoDir, prevCommit, commit)
		if err != nil {
			cr.Error = fmt.Errorf("list changed files: %w", err)
			result.Results = append(result.Results, cr)
			result.ErrorCount++
			prevCommit = commit
			continue
		}
		cr.ChangedFiles = changedFiles

		if len(changedFiles) == 0 {
			fmt.Printf("  no Go files changed — copying graph forward\n")
			// CopyGraphToCommit copies all four tables including type_ifaces.
			if err := db.CopyGraphToCommit(ctx, incrRepo, prevCommit, commit); err != nil {
				cr.Error = err
			}
			cr.Pass = (cr.Error == nil)
			result.Results = append(result.Results, cr)
			if cr.Pass {
				result.PassCount++
			} else {
				result.ErrorCount++
			}
			prevCommit = commit
			continue
		}

		fmt.Printf("  changed files: %v\n", changedFiles)

		// Checkout the new commit so ParseRepo reads the correct source.
		if err := checkoutCommit(repoDir, commit); err != nil {
			cr.Error = fmt.Errorf("checkout %s: %w", commit, err)
			result.Results = append(result.Results, cr)
			result.ErrorCount++
			prevCommit = commit
			continue
		}

		// ── (a) Incremental update ────────────────────────────────────────────
		incrStart := time.Now()
		_, incrErr := Update(ctx, db, repoDir, modulePath,
			incrRepo, prevCommit, commit, changedFiles)
		cr.IncrementalTime = time.Since(incrStart)

		if incrErr != nil {
			cr.Error = fmt.Errorf("incremental update: %w", incrErr)
			result.Results = append(result.Results, cr)
			result.ErrorCount++
			prevCommit = commit
			continue
		}

		// ── (b) Full rebuild ──────────────────────────────────────────────────
		fullStart := time.Now()
		fullTable, fullErr := parser.ParseRepo(repoDir, modulePath)
		if fullErr == nil {
			_, fullErr = callgraph.BuildV2(ctx, db, fullTable, fullRepo, commit)
		}
		cr.FullBuildTime = time.Since(fullStart)

		if fullErr != nil {
			cr.Error = fmt.Errorf("full rebuild: %w", fullErr)
			result.Results = append(result.Results, cr)
			result.ErrorCount++
			prevCommit = commit
			continue
		}

		// ── (c) Diff the two graphs ───────────────────────────────────────────
		incrEdges, err := db.LoadAllEdges(ctx, incrRepo, commit)
		if err != nil {
			cr.Error = fmt.Errorf("load incremental edges: %w", err)
			result.Results = append(result.Results, cr)
			result.ErrorCount++
			prevCommit = commit
			continue
		}

		fullEdges, err := db.LoadAllEdges(ctx, fullRepo, commit)
		if err != nil {
			cr.Error = fmt.Errorf("load full edges: %w", err)
			result.Results = append(result.Results, cr)
			result.ErrorCount++
			prevCommit = commit
			continue
		}

		incrSet := store.EdgeSet(incrEdges)
		fullSet := store.EdgeSet(fullEdges)
		cr.OnlyInIncremental, cr.OnlyInFull = store.DiffEdgeSets(incrSet, fullSet)
		cr.Pass = len(cr.OnlyInIncremental) == 0 && len(cr.OnlyInFull) == 0

		// ── (d) Report ────────────────────────────────────────────────────────
		if cr.Pass {
			result.PassCount++
			fmt.Printf("  PASS  incr=%d edges, full=%d edges, incr_time=%v full_time=%v speedup=%.1fx\n",
				len(incrEdges), len(fullEdges),
				cr.IncrementalTime.Round(time.Millisecond),
				cr.FullBuildTime.Round(time.Millisecond),
				float64(cr.FullBuildTime)/float64(cr.IncrementalTime+1))
		} else {
			result.FailCount++
			fmt.Printf("  FAIL  incr=%d edges, full=%d edges\n", len(incrEdges), len(fullEdges))
			printEdgeDiff(cr.OnlyInIncremental, cr.OnlyInFull)
		}

		result.Results = append(result.Results, cr)
		prevCommit = commit
	}

	// Restore HEAD.
	_ = exec.Command("git", "-C", repoDir, "checkout", "-").Run()

	// Compute average speedup.
	var totalSpeedup float64
	var speedupCount int
	for _, cr := range result.Results {
		if cr.Pass && cr.IncrementalTime > 0 && cr.FullBuildTime > 0 {
			totalSpeedup += float64(cr.FullBuildTime) / float64(cr.IncrementalTime)
			speedupCount++
		}
	}
	if speedupCount > 0 {
		result.AvgSpeedup = totalSpeedup / float64(speedupCount)
	}

	return result, nil
}

// PrintSummary prints a concise summary of the harness results.
func PrintSummary(r *HarnessResult) {
	fmt.Printf("\n════════════════════════════════════════\n")
	fmt.Printf("Differential Harness Results\n")
	fmt.Printf("Repo: %s\n", r.Repo)
	fmt.Printf("Commits tested: %d\n", r.TotalCommits)
	fmt.Printf("PASS: %d  FAIL: %d  ERROR: %d\n",
		r.PassCount, r.FailCount, r.ErrorCount)
	if r.AvgSpeedup > 0 {
		fmt.Printf("Avg speedup (full/incr): %.1fx\n", r.AvgSpeedup)
	}

	if r.FailCount > 0 {
		fmt.Printf("\nFailed commits:\n")
		for _, cr := range r.Results {
			if !cr.Pass && cr.Error == nil {
				fmt.Printf("  %s..%s (%d only-in-incr, %d only-in-full)\n",
					shortSHA(cr.BaseCommit), shortSHA(cr.Commit),
					len(cr.OnlyInIncremental), len(cr.OnlyInFull))
			}
		}
	}
	if r.ErrorCount > 0 {
		fmt.Printf("\nError commits:\n")
		for _, cr := range r.Results {
			if cr.Error != nil {
				fmt.Printf("  %s: %v\n", shortSHA(cr.Commit), cr.Error)
			}
		}
	}
	fmt.Printf("════════════════════════════════════════\n")
}

// ── Git helpers ───────────────────────────────────────────────────────────────

// listCommits returns up to maxCommits+1 SHAs in oldest-to-newest order,
// limited to commits that touch at least one .go file.
// We get the most recent maxCommits+1 commits (so we have maxCommits pairs),
// then reverse to get chronological order for sequential application.
func listCommits(repoDir string, maxCommits int) ([]string, error) {
	n := maxCommits + 1
	args := []string{"-C", repoDir, "log",
		fmt.Sprintf("--max-count=%d", n),
		"--format=%H",
		"--", "*.go",
	}
	cmd := exec.Command("git", args...)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git log: %w", err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	var shas []string
	for _, l := range lines {
		if l = strings.TrimSpace(l); l != "" {
			shas = append(shas, l)
		}
	}
	// git log gives newest-first; reverse to get oldest-first for
	// sequential incremental application.
	for i, j := 0, len(shas)-1; i < j; i, j = i+1, j-1 {
		shas[i], shas[j] = shas[j], shas[i]
	}
	return shas, nil
}

// changedGoFiles returns repo-relative paths of .go files that changed between
// fromCommit and toCommit.
func changedGoFiles(repoDir, fromCommit, toCommit string) ([]string, error) {
	cmd := exec.Command("git", "-C", repoDir,
		"diff", "--name-only", "--diff-filter=ACMRD",
		fromCommit, toCommit, "--", "*.go")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git diff --name-only: %w", err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	var files []string
	for _, l := range lines {
		if l = strings.TrimSpace(l); l != "" {
			files = append(files, l)
		}
	}
	return files, nil
}

// checkoutCommit runs `git checkout <sha>` in repoDir, putting the repo into
// detached HEAD state at that commit.
func checkoutCommit(repoDir, sha string) error {
	cmd := exec.Command("git", "-C", repoDir, "checkout", "--detach", sha)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git checkout %s: %w\n%s", sha, err, out)
	}
	return nil
}

// printEdgeDiff prints the diff between incremental and full edge sets in a
// structured, readable format.
func printEdgeDiff(onlyIncr, onlyFull []store.Edge) {
	cap := 10
	if len(onlyIncr) > 0 {
		fmt.Printf("  Only in INCREMENTAL (stale/spurious edges — %d total):\n", len(onlyIncr))
		for i, e := range onlyIncr {
			if i >= cap {
				fmt.Printf("    ... and %d more\n", len(onlyIncr)-cap)
				break
			}
			fmt.Printf("    [%s] %s → %s\n", e.Provenance, e.SourceSymbol, e.TargetSymbol)
		}
	}
	if len(onlyFull) > 0 {
		fmt.Printf("  Only in FULL BUILD (missing from incremental — %d total):\n", len(onlyFull))
		for i, e := range onlyFull {
			if i >= cap {
				fmt.Printf("    ... and %d more\n", len(onlyFull)-cap)
				break
			}
			fmt.Printf("    [%s] %s → %s\n", e.Provenance, e.SourceSymbol, e.TargetSymbol)
		}
	}
}

func shortSHA(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}
