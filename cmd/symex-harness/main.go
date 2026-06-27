// symex-harness — differential test harness for the incremental update engine.
//
// Walks a real repo's commit history and for each commit verifies that the
// incremental graph == the full rebuild graph. Reports per-commit PASS/FAIL
// with exact edge diffs on failures.
//
// Usage:
//
//	symex-harness -repo /path/to/repo -dsn <postgres-dsn> [-commits 25]
//
// The repo must have at least 2 commits that touch .go files.
// All commits are checked out via git (detached HEAD), so do not use a repo
// with uncommitted local changes you want to preserve.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/yourorg/symex/internal/incremental"
	"github.com/yourorg/symex/internal/store"
)

func main() {
	repo := flag.String("repo", "", "Path to local git repo (required)")
	dsn := flag.String("dsn", "", "Postgres DSN (required)")
	commits := flag.Int("commits", 25, "Max commits to test (0 = all)")
	outJSON := flag.String("out", "", "Write JSON results to this file")
	flag.Parse()

	if *repo == "" || *dsn == "" {
		fmt.Fprintln(os.Stderr, "usage: symex-harness -repo <path> -dsn <dsn> [-commits N] [-out results.json]")
		os.Exit(1)
	}

	ctx := context.Background()

	db, err := store.Open(*dsn)
	if err != nil {
		fatalf("open postgres: %v", err)
	}
	defer db.Close()

	if err := db.ApplySchema(ctx); err != nil {
		fatalf("apply schema: %v", err)
	}
	if err := db.ApplySchemaV2(ctx); err != nil {
		fatalf("apply schema v2: %v", err)
	}

	result, err := incremental.RunHarness(ctx, db, *repo, *commits)
	if err != nil {
		fatalf("harness: %v", err)
	}

	incremental.PrintSummary(result)

	if *outJSON != "" {
		data, _ := json.MarshalIndent(result, "", "  ")
		if err := os.WriteFile(*outJSON, data, 0644); err != nil {
			fmt.Fprintf(os.Stderr, "warn: write json: %v\n", err)
		} else {
			fmt.Printf("Results written to %s\n", *outJSON)
		}
	}

	if result.FailCount > 0 {
		os.Exit(1)
	}
}

func fatalf(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	os.Exit(2)
}
