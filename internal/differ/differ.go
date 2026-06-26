// Package differ maps a git diff to the set of symbols whose bodies were
// changed. This is the frontier from which the reachability engine starts.
//
// # Why changed symbols, not changed files
//
// A file can contain dozens of functions. If only one changes, running all
// tests that transitively depend on any function in that file is a massive
// over-approximation. We map changed line ranges to the specific function
// bodies they fall inside, using the line spans recorded in the symbol table.
// This is the minimum correct frontier: exactly the functions that changed.
//
// # What counts as "changed"
//
// Any line within a function's declared range (start line of `func` keyword
// to end line of closing `}`) that appears in the diff's added or removed
// hunks puts that function in the changed symbol set.
//
// Caller beware: a function body that is moved without modification will
// appear changed (its old location shows deleted lines, its new location
// shows added lines). The semantic change classifier (stage 4, not built
// here) handles that via the canonical hash comparison.
package differ

import (
	"bufio"
	"bytes"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/yourorg/symex/internal/store"
)

// ChangedFile holds the line ranges that were modified in one file.
type ChangedFile struct {
	// Path is the repo-relative path (e.g. "billing/ledger.go").
	Path string

	// HunkRanges is the list of [startLine, endLine] pairs from the diff hunks
	// for this file. These are lines in the NEW version of the file.
	HunkRanges [][2]int
}

// Diff represents a parsed git diff: the set of files changed and the line
// ranges within each file that were affected.
type Diff struct {
	// BaseCommit is the commit before the change.
	BaseCommit string
	// HeadCommit is the commit after the change (or "HEAD").
	HeadCommit string
	// Files holds per-file change information.
	Files []ChangedFile
}

// ChangedSymbolSet is the output of MapToSymbols: the set of fully-qualified
// symbol names whose bodies contain at least one changed line.
type ChangedSymbolSet struct {
	// Symbols maps qualified symbol name → the file it lives in.
	Symbols map[string]string
}

// FromGit runs `git diff baseCommit headCommit` in repoDir and parses the
// unified diff output into a Diff struct. repoDir must be the root of the git
// repo (the directory containing the .git folder).
func FromGit(repoDir, baseCommit, headCommit string) (*Diff, error) {
	args := []string{"diff", "--unified=0", baseCommit, headCommit}
	cmd := exec.Command("git", args...)
	cmd.Dir = repoDir

	out, err := cmd.Output()
	if err != nil {
		// Include stderr in the error so the caller can diagnose git failures.
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("git diff: %w\nstderr: %s", err, ee.Stderr)
		}
		return nil, fmt.Errorf("git diff: %w", err)
	}

	return parseDiff(out, baseCommit, headCommit)
}

// FromPatch parses a unified diff provided as a raw byte slice (e.g. from a
// GitHub webhook payload). baseCommit and headCommit are recorded for
// provenance but not used in the parsing.
func FromPatch(patch []byte, baseCommit, headCommit string) (*Diff, error) {
	return parseDiff(patch, baseCommit, headCommit)
}

// parseDiff parses a unified diff (output of `git diff --unified=0`) into a
// Diff struct. It only extracts .go files; all other file changes are ignored.
//
// Hunk header format: @@ -oldStart[,oldCount] +newStart[,newCount] @@
// We only track the new-file line ranges (+newStart,newCount).
func parseDiff(data []byte, baseCommit, headCommit string) (*Diff, error) {
	d := &Diff{
		BaseCommit: baseCommit,
		HeadCommit: headCommit,
	}

	var currentFile *ChangedFile
	scanner := bufio.NewScanner(bytes.NewReader(data))

	for scanner.Scan() {
		line := scanner.Text()

		switch {
		case strings.HasPrefix(line, "diff --git "):
			// New file section. Extract the b/ path (the new version).
			// Format: "diff --git a/path/to/file.go b/path/to/file.go"
			parts := strings.Fields(line)
			if len(parts) < 4 {
				continue
			}
			newPath := strings.TrimPrefix(parts[3], "b/")
			if !strings.HasSuffix(newPath, ".go") {
				currentFile = nil // skip non-Go files
				continue
			}
			d.Files = append(d.Files, ChangedFile{Path: filepath.ToSlash(newPath)})
			currentFile = &d.Files[len(d.Files)-1]

		case strings.HasPrefix(line, "@@ ") && currentFile != nil:
			// Hunk header. Extract the new-file line range.
			start, count, err := parseHunkHeader(line)
			if err != nil {
				// Malformed hunk — skip it, don't abort.
				continue
			}
			end := start + count - 1
			if end < start {
				end = start // single-line hunk (count==0 means deletion only)
			}
			currentFile.HunkRanges = append(currentFile.HunkRanges, [2]int{start, end})
		}
	}

	return d, scanner.Err()
}

// parseHunkHeader parses a unified diff hunk header line of the form:
// "@@ -oldStart[,oldCount] +newStart[,newCount] @@[ context]"
// and returns (newStart, newCount, nil).
func parseHunkHeader(line string) (start, count int, err error) {
	// Find the "+..." segment between "@@" markers.
	parts := strings.Fields(line)
	// parts[0] = "@@", parts[1] = "-...", parts[2] = "+...", parts[3] = "@@"
	if len(parts) < 4 {
		return 0, 0, fmt.Errorf("short hunk header: %q", line)
	}
	newPart := strings.TrimPrefix(parts[2], "+")
	if idx := strings.Index(newPart, ","); idx >= 0 {
		start, err = strconv.Atoi(newPart[:idx])
		if err != nil {
			return 0, 0, err
		}
		count, err = strconv.Atoi(newPart[idx+1:])
		return start, count, err
	}
	// No comma: single line.
	start, err = strconv.Atoi(newPart)
	return start, 1, err
}

// MapToSymbols maps the changed line ranges in diff to the set of functions
// and methods whose bodies contain at least one changed line, using symbol
// row data from the Postgres store.
//
// symbols should be loaded via store.LoadSymbolsForFiles for the files
// mentioned in the diff.
func MapToSymbols(diff *Diff, symbols []store.SymbolRow) *ChangedSymbolSet {
	result := &ChangedSymbolSet{
		Symbols: make(map[string]string),
	}

	// Build a per-file map of symbol rows for fast lookup.
	byFile := make(map[string][]store.SymbolRow)
	for _, sym := range symbols {
		byFile[sym.FilePath] = append(byFile[sym.FilePath], sym)
	}

	for _, cf := range diff.Files {
		fileSym, ok := byFile[cf.Path]
		if !ok {
			// File not in symbol table — possibly a new file that hasn't been
			// indexed yet, or a non-Go file that slipped through. Skip.
			continue
		}

		for _, hunk := range cf.HunkRanges {
			hunkStart, hunkEnd := hunk[0], hunk[1]

			for _, sym := range fileSym {
				// Only functions and methods have executable bodies with lines
				// that are meaningful for test impact. Skip interface and type
				// declarations.
				if sym.Kind != "function" && sym.Kind != "method" {
					continue
				}

				// A symbol is "changed" if any line in the hunk falls within
				// its declared line range [LineStart, LineEnd].
				if hunkEnd < sym.LineStart || hunkStart > sym.LineEnd {
					continue // no overlap
				}
				result.Symbols[sym.SymbolName] = sym.FilePath
			}
		}
	}

	return result
}

// FilePaths returns the list of file paths mentioned in the diff.
func (d *Diff) FilePaths() []string {
	paths := make([]string, 0, len(d.Files))
	for _, f := range d.Files {
		paths = append(paths, f.Path)
	}
	return paths
}

// Summary returns a human-readable description of the diff.
func (d *Diff) Summary() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "diff %s..%s: %d Go file(s) changed\n",
		shortHash(d.BaseCommit), shortHash(d.HeadCommit), len(d.Files))
	for _, f := range d.Files {
		fmt.Fprintf(&sb, "  %s: %d hunk(s)\n", f.Path, len(f.HunkRanges))
		for _, h := range f.HunkRanges {
			fmt.Fprintf(&sb, "    lines %d–%d\n", h[0], h[1])
		}
	}
	return sb.String()
}

func shortHash(h string) string {
	if len(h) > 8 {
		return h[:8]
	}
	return h
}
