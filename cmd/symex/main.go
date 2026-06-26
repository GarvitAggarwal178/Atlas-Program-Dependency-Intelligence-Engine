// symex — stage 1: AST symbol extractor for the incremental call graph engine.
//
// Usage:
//
//	symex [-repo <path>] [-json] [-pretty]
//
// Flags:
//
//	-repo    Path to the Go repository root (directory containing go.mod).
//	         Defaults to the current working directory.
//	-pretty  Pretty-print JSON output with indentation (default: true).
//	-out     Write output to this file path instead of stdout.
//
// Output:
//
//	A JSON-encoded RepoSymbolTable to stdout (or -out), documenting every
//	symbol defined and every call expression found across all .go files.
//
// Exit codes:
//
//	0  Success
//	1  Usage error
//	2  Parse or type-check error (partial output may have been written)
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/yourorg/symex/internal/modpath"
	"github.com/yourorg/symex/internal/parser"
)

func main() {
	repoFlag := flag.String("repo", ".", "Path to Go repository root (contains go.mod)")
	prettyFlag := flag.Bool("pretty", true, "Pretty-print JSON output")
	outFlag := flag.String("out", "", "Write output to file instead of stdout")
	flag.Parse()

	repoDir, err := filepath.Abs(*repoFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: resolving repo path: %v\n", err)
		os.Exit(1)
	}

	// Locate the module root (may be above repoDir if -repo pointed to a
	// subdirectory).
	repoRoot, modulePath, err := modpath.FindModulePath(repoDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		fmt.Fprintf(os.Stderr, "hint: make sure -repo points to a directory at or below a go.mod\n")
		os.Exit(1)
	}

	fmt.Fprintf(os.Stderr, "parsing repo: %s (module: %s)\n", repoRoot, modulePath)

	table, err := parser.ParseRepo(repoRoot, modulePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(2)
	}

	var encoded []byte
	if *prettyFlag {
		encoded, err = json.MarshalIndent(table, "", "  ")
	} else {
		encoded, err = json.Marshal(table)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: encoding JSON: %v\n", err)
		os.Exit(2)
	}
	encoded = append(encoded, '\n')

	if *outFlag != "" {
		if err := os.WriteFile(*outFlag, encoded, 0644); err != nil {
			fmt.Fprintf(os.Stderr, "error: writing output: %v\n", err)
			os.Exit(2)
		}
		fmt.Fprintf(os.Stderr, "output written to %s\n", *outFlag)
	} else {
		os.Stdout.Write(encoded)
	}
}
