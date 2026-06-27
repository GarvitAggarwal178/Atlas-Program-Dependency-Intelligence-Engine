// symex-graph — call graph builder, git diff integration, semantic change
// classifier (stage 3), and reachability engine.
//
// # Modes
//
// ## build: parse a repo and persist its call graph to Postgres.
//
//	symex-graph build -repo <path> -dsn <postgres-dsn> [-commit <sha>]
//
// ## analyze: given a diff (two commits), classify changed symbols and
//             compute the reachable set using the filtered frontier.
//
//	symex-graph analyze -repo <path> -dsn <postgres-dsn> \
//	    -base <base-commit> -head <head-commit>
//
// ## build-and-analyze: do both in one shot (most common for development).
//
//	symex-graph build-and-analyze -repo <path> -dsn <postgres-dsn> \
//	    -base <base-commit> -head <head-commit>
//
// # Postgres DSN format
//
//	"host=localhost port=5432 user=symex password=symex dbname=symex sslmode=disable"
//	or the URL form: "postgres://symex:symex@localhost/symex?sslmode=disable"
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/yourorg/symex/internal/callgraph"
	"github.com/yourorg/symex/internal/classifier"
	"github.com/yourorg/symex/internal/differ"
	"github.com/yourorg/symex/internal/modpath"
	"github.com/yourorg/symex/internal/parser"
	"github.com/yourorg/symex/internal/reachability"
	"github.com/yourorg/symex/internal/store"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "build":
		runBuild(os.Args[2:])
	case "analyze":
		runAnalyze(os.Args[2:])
	case "build-and-analyze":
		runBuildAndAnalyze(os.Args[2:])
	default:
		usage()
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `symex-graph — call graph + semantic classifier (stage 3)

Commands:
  build              Parse repo and store call graph in Postgres
  analyze            Classify changed symbols and compute reachability
  build-and-analyze  build + analyze in one shot

Flags (all commands):
  -repo    Path to Go repo root (contains go.mod)      [required]
  -dsn     Postgres connection string                   [required]
  -commit  Commit SHA to build against (default: HEAD) [build only]
  -base    Base commit for diff                         [analyze only]
  -head    Head commit for diff                         [analyze only]
  -pretty  Pretty-print JSON output (default: true)

Example:
  symex-graph build-and-analyze \
    -repo /tmp/chi \
    -dsn "postgres://symex:symex@localhost/symex?sslmode=disable" \
    -base HEAD~1 \
    -head HEAD`)
}

// --- build command ---

func runBuild(args []string) {
	fs := flag.NewFlagSet("build", flag.ExitOnError)
	repo := fs.String("repo", ".", "Path to Go repo root")
	dsn := fs.String("dsn", "", "Postgres DSN")
	commit := fs.String("commit", "", "Commit SHA (default: HEAD)")
	fs.Parse(args)

	requireFlag(*dsn, "-dsn")

	ctx := context.Background()
	commitHash := resolveCommit(*repo, *commit)
	repoRoot, modulePath := resolveRepo(*repo)

	db := mustOpenDB(*dsn, ctx)
	defer db.Close()

	logf("parsing %s @ %s (module: %s)", repoRoot, shortHash(commitHash), modulePath)
	t0 := time.Now()
	table, err := parser.ParseRepo(repoRoot, modulePath)
	must(err, "parse repo")
	logf("parsed %d files in %v", len(table.Files), time.Since(t0).Round(time.Millisecond))

	logf("building call graph...")
	t1 := time.Now()
	result, err := callgraph.Build(ctx, db, table, repoRoot, commitHash)
	must(err, "build graph")
	logf("built %d edges (%d direct, %d interface_resolved) in %v",
		result.TotalEdges, result.DirectEdges, result.InterfaceResolvedEdges,
		time.Since(t1).Round(time.Millisecond))
	logf("indexed %d symbols, %d dispatch sites", result.SymbolsIndexed, result.DispatchSites)
}

// --- analyze command ---

func runAnalyze(args []string) {
	fs := flag.NewFlagSet("analyze", flag.ExitOnError)
	repo := fs.String("repo", ".", "Path to Go repo root")
	dsn := fs.String("dsn", "", "Postgres DSN")
	base := fs.String("base", "", "Base commit")
	head := fs.String("head", "HEAD", "Head commit")
	pretty := fs.Bool("pretty", true, "Pretty-print JSON")
	fs.Parse(args)

	requireFlag(*dsn, "-dsn")
	requireFlag(*base, "-base")

	ctx := context.Background()
	repoRoot, _ := resolveRepo(*repo)
	headHash := resolveCommit(*repo, *head)

	db := mustOpenDB(*dsn, ctx)
	defer db.Close()

	output := runAnalysisCore(ctx, db, repoRoot, *base, headHash)
	printJSON(output, *pretty)
}

// --- build-and-analyze command ---

func runBuildAndAnalyze(args []string) {
	fs := flag.NewFlagSet("build-and-analyze", flag.ExitOnError)
	repo := fs.String("repo", ".", "Path to Go repo root")
	dsn := fs.String("dsn", "", "Postgres DSN")
	base := fs.String("base", "", "Base commit for diff")
	head := fs.String("head", "HEAD", "Head commit")
	pretty := fs.Bool("pretty", true, "Pretty-print JSON")
	fs.Parse(args)

	requireFlag(*dsn, "-dsn")
	requireFlag(*base, "-base")

	ctx := context.Background()
	repoRoot, modulePath := resolveRepo(*repo)
	headHash := resolveCommit(*repo, *head)

	db := mustOpenDB(*dsn, ctx)
	defer db.Close()

	logf("parsing %s @ %s", repoRoot, shortHash(headHash))
	t0 := time.Now()
	table, err := parser.ParseRepo(repoRoot, modulePath)
	must(err, "parse repo")
	logf("parsed %d files in %v", len(table.Files), time.Since(t0).Round(time.Millisecond))

	logf("building call graph...")
	t1 := time.Now()
	br, err := callgraph.Build(ctx, db, table, repoRoot, headHash)
	must(err, "build graph")
	logf("built %d edges (%d direct, %d interface_resolved) in %v",
		br.TotalEdges, br.DirectEdges, br.InterfaceResolvedEdges,
		time.Since(t1).Round(time.Millisecond))

	output := runAnalysisCore(ctx, db, repoRoot, *base, headHash)
	printJSON(output, *pretty)
}

// --- shared analysis core ---

// AnalysisOutput is the full JSON output of an analysis run.
type AnalysisOutput struct {
	Repo       string `json:"repo"`
	BaseCommit string `json:"base_commit"`
	HeadCommit string `json:"head_commit"`
	GraphStats struct {
		TotalEdges int `json:"total_edges"`
	} `json:"graph_stats"`
	Diff struct {
		FilesChanged int      `json:"files_changed"`
		ChangedFiles []string `json:"changed_files"`
	} `json:"diff"`
	// ChangedSymbolSet is every symbol touched by the diff (before classification).
	ChangedSymbolSet []ChangedSymbolEntry `json:"changed_symbol_set"`
	// Classifications holds the semantic verdict for each changed symbol.
	Classifications []ClassificationEntry `json:"classifications"`
	// ReachableSet is the BFS result, starting only from non-trivial symbols.
	ReachableSet []ReachableSymbolEntry `json:"reachable_set"`
	Summary      struct {
		ChangedTotal           int `json:"changed_total"`
		TrivialCount           int `json:"trivial_count"`
		SignatureChangeCount   int `json:"signature_change_count"`
		LogicChangeCount       int `json:"logic_change_count"`
		FrontierSize           int `json:"frontier_size"`
		TotalReachable         int `json:"total_reachable"`
		DirectPathCount        int `json:"direct_path_count"`
		InterfacePathCount     int `json:"interface_resolved_path_count"`
	} `json:"summary"`
}

type ChangedSymbolEntry struct {
	Symbol string `json:"symbol"`
	File   string `json:"file"`
}

// ClassificationEntry is the per-symbol output of the classifier, as it
// appears in the JSON. Mirrors classifier.Classification but with
// json tags that match the output spec.
type ClassificationEntry struct {
	Symbol          string `json:"symbol"`
	Kind            string `json:"kind"`
	Reason          string `json:"reason"`
	BeforeHash      string `json:"before_hash,omitempty"`
	AfterHash       string `json:"after_hash,omitempty"`
	BeforeSignature string `json:"before_signature,omitempty"`
	AfterSignature  string `json:"after_signature,omitempty"`
}

type ReachableSymbolEntry struct {
	Symbol     string   `json:"symbol"`
	Path       []string `json:"path"`
	Provenance string   `json:"provenance"`
	Depth      int      `json:"depth"`
}

func runAnalysisCore(
	ctx context.Context,
	db *store.DB,
	repoRoot, baseCommit, headCommit string,
) *AnalysisOutput {
	output := &AnalysisOutput{
		Repo:       repoRoot,
		BaseCommit: baseCommit,
		HeadCommit: headCommit,
	}

	n, err := db.EdgeCount(ctx, repoRoot, headCommit)
	must(err, "count edges")
	output.GraphStats.TotalEdges = n

	// ── Step 1: git diff ──────────────────────────────────────────────────────
	logf("computing diff %s..%s", shortHash(baseCommit), shortHash(headCommit))
	diff, err := differ.FromGit(repoRoot, baseCommit, headCommit)
	must(err, "git diff")
	logf(diff.Summary())

	output.Diff.FilesChanged = len(diff.Files)
	for _, f := range diff.Files {
		output.Diff.ChangedFiles = append(output.Diff.ChangedFiles, f.Path)
	}

	// ── Step 2: load symbol rows for changed files ────────────────────────────
	filePaths := diff.FilePaths()
	symRows, err := db.LoadSymbolsForFiles(ctx, repoRoot, headCommit, filePaths)
	must(err, "load symbols")

	// ── Step 3: map diff hunks → changed symbol set ───────────────────────────
	changedSet := differ.MapToSymbols(diff, symRows)
	logf("raw changed symbol set: %d symbol(s)", len(changedSet.Symbols))

	for sym, file := range changedSet.Symbols {
		output.ChangedSymbolSet = append(output.ChangedSymbolSet, ChangedSymbolEntry{
			Symbol: sym,
			File:   file,
		})
	}
	sort.Slice(output.ChangedSymbolSet, func(i, j int) bool {
		return output.ChangedSymbolSet[i].Symbol < output.ChangedSymbolSet[j].Symbol
	})
	output.Summary.ChangedTotal = len(changedSet.Symbols)

	if len(changedSet.Symbols) == 0 {
		logf("no changed symbols — no functions were modified in the diff")
		return output
	}

	// ── Step 4: semantic change classification ────────────────────────────────
	//
	// This is the stage 3 addition. For each symbol in the raw changed set,
	// compare the pre-change and post-change versions:
	//   trivial        → excluded from reachability frontier (zero blast radius)
	//   signature_change → included, flagged distinctly in output
	//   logic_change    → included, normal reachability
	//
	// GitFetcher runs `git show <commit>:<path>` for each file at each commit.
	fetcher := &classifier.GitFetcher{RepoDir: repoRoot}
	logf("classifying %d changed symbol(s)...", len(changedSet.Symbols))
	t := time.Now()

	classifications, filteredFrontier, err := classifier.ClassifyAll(
		fetcher, baseCommit, headCommit, changedSet.Symbols,
	)
	must(err, "classify symbols")
	logf("classification complete in %v", time.Since(t).Round(time.Millisecond))

	// Collect classification results for output and tally by kind.
	for sym, c := range classifications {
		entry := ClassificationEntry{
			Symbol:          sym,
			Kind:            string(c.Kind),
			Reason:          c.Reason,
			BeforeHash:      c.BeforeHash,
			AfterHash:       c.AfterHash,
			BeforeSignature: c.BeforeSignature,
			AfterSignature:  c.AfterSignature,
		}
		output.Classifications = append(output.Classifications, entry)

		switch c.Kind {
		case classifier.Trivial:
			output.Summary.TrivialCount++
			logf("  trivial:          %s — %s", sym, c.Reason)
		case classifier.SignatureChange:
			output.Summary.SignatureChangeCount++
			logf("  signature_change: %s — %s", sym, c.Reason)
		case classifier.LogicChange:
			output.Summary.LogicChangeCount++
			logf("  logic_change:     %s — %s", sym, c.Reason)
		}
	}
	sort.Slice(output.Classifications, func(i, j int) bool {
		return output.Classifications[i].Symbol < output.Classifications[j].Symbol
	})

	logf("frontier after trivial exclusion: %d symbol(s) (excluded %d trivial)",
		len(filteredFrontier),
		len(changedSet.Symbols)-len(filteredFrontier))
	output.Summary.FrontierSize = len(filteredFrontier)

	if len(filteredFrontier) == 0 {
		logf("all changes are trivial — zero blast radius, no reachability needed")
		return output
	}

	// ── Step 5: load call graph into memory ───────────────────────────────────
	logf("loading call graph from Postgres...")
	t = time.Now()
	graph, err := callgraph.LoadInMemory(ctx, db, repoRoot, headCommit)
	must(err, "load graph")
	logf("loaded %d nodes with outgoing edges in %v",
		len(graph.Edges), time.Since(t).Round(time.Millisecond))

	// ── Step 6: BFS reachability from the filtered frontier ───────────────────
	//
	// filteredFrontier contains only non-trivial symbols. Trivial symbols are
	// excluded: a pure rename or comment edit cannot affect any caller's
	// behavior, so there is no blast radius to compute.
	logf("running BFS from %d-symbol filtered frontier...", len(filteredFrontier))
	t = time.Now()
	reach := reachability.Run(graph, filteredFrontier)
	logf("reachability complete: %d nodes reached in %v",
		reach.TotalNodes, time.Since(t).Round(time.Millisecond))

	sort.Slice(reach.Reached, func(i, j int) bool {
		if reach.Reached[i].Depth != reach.Reached[j].Depth {
			return reach.Reached[i].Depth < reach.Reached[j].Depth
		}
		return reach.Reached[i].Symbol < reach.Reached[j].Symbol
	})

	for _, rs := range reach.Reached {
		output.ReachableSet = append(output.ReachableSet, ReachableSymbolEntry{
			Symbol:     rs.Symbol,
			Path:       rs.Path,
			Provenance: rs.Provenance,
			Depth:      rs.Depth,
		})
	}

	output.Summary.TotalReachable = reach.TotalNodes
	output.Summary.DirectPathCount = reach.DirectCount
	output.Summary.InterfacePathCount = reach.InterfaceResolvedCount

	return output
}

// --- helpers ---

func resolveRepo(repoFlag string) (repoRoot, modulePath string) {
	abs, err := filepath.Abs(repoFlag)
	must(err, "resolve repo path")
	_, mod, err := modpath.FindModulePath(abs)
	must(err, "find go.mod")
	return abs, mod
}

func resolveCommit(repoDir, commitFlag string) string {
	if commitFlag == "" || commitFlag == "HEAD" {
		return gitRevParse(repoDir, "HEAD")
	}
	if len(commitFlag) == 40 && isHex(commitFlag) {
		return commitFlag
	}
	return gitRevParse(repoDir, commitFlag)
}

func gitRevParse(repoDir, ref string) string {
	cmd := exec.Command("git", "rev-parse", ref)
	cmd.Dir = repoDir
	out, err := cmd.Output()
	must(err, "git rev-parse "+ref)
	return strings.TrimSpace(string(out))
}

func isHex(s string) bool {
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

func mustOpenDB(dsn string, ctx context.Context) *store.DB {
	db, err := store.Open(dsn)
	must(err, "open postgres")
	if err := db.ApplySchema(ctx); err != nil {
		must(err, "apply schema")
	}
	return db
}

func requireFlag(val, name string) {
	if val == "" {
		fmt.Fprintf(os.Stderr, "error: %s is required\n", name)
		os.Exit(1)
	}
}

func must(err error, msg string) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %s: %v\n", msg, err)
		os.Exit(2)
	}
}

func logf(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "[symex] "+format+"\n", args...)
}

func shortHash(h string) string {
	if len(h) > 8 {
		return h[:8]
	}
	return h
}

func printJSON(v interface{}, pretty bool) {
	var out []byte
	var err error
	if pretty {
		out, err = json.MarshalIndent(v, "", "  ")
	} else {
		out, err = json.Marshal(v)
	}
	must(err, "marshal JSON")
	fmt.Println(string(out))
}
