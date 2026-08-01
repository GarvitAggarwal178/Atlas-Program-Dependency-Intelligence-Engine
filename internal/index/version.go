package index

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"sort"

	"github.com/yourorg/symex/internal/derive"
	"github.com/yourorg/symex/internal/parser"
)

//go:embed *.go
var sourceFS embed.FS

// AnalyzerVersion is architecture.md invariant I13: "analyzer_version
// change invalidates all cached derivations. Make it a build-time hash of
// the analyzer package, not a hand-bumped constant." Combines a hash of
// this package's own source with internal/parser's and internal/derive's —
// the three packages whose logic actually determines what a fact is and
// how it's derived. Changing any of their source changes this value,
// which callers use to force a full re-apply (see ForceFullApply in
// apply.go) rather than trusting file-hash-based selectivity against
// results computed by a different analyzer version.
//
// Deliberately NOT based on runtime/debug.ReadBuildInfo()'s vcs.revision:
// checked directly in this environment and confirmed it isn't populated
// for `go run`/`go test` invocations here (no vcs.revision setting is
// present), so building on it would have been an unverified assumption
// that silently degrades to "no version signal at all." go:embed of each
// package's own source is available unconditionally, regardless of build
// invocation or VCS state.
func AnalyzerVersion() string {
	own := hashEmbeddedGoFiles(sourceFS)
	h := sha256.New()
	h.Write([]byte(own))
	h.Write([]byte{0})
	h.Write([]byte(parser.SourceHash()))
	h.Write([]byte{0})
	h.Write([]byte(derive.SourceHash()))
	return hex.EncodeToString(h.Sum(nil))
}

func hashEmbeddedGoFiles(fsys embed.FS) string {
	var names []string
	_ = fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		names = append(names, path)
		return nil
	})
	sort.Strings(names)

	h := sha256.New()
	for _, name := range names {
		content, err := fsys.ReadFile(name)
		if err != nil {
			continue
		}
		h.Write([]byte(name))
		h.Write([]byte{0})
		h.Write(content)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}
