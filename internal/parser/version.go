package parser

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"sort"
)

//go:embed *.go
var sourceFS embed.FS

// SourceHash is a stable hash of this package's own .go source files
// (including this file), used by internal/index.AnalyzerVersion to build
// architecture.md invariant I13's "build-time hash of the analyzer
// package, not a hand-bumped constant." go:embed can't reach across
// package boundaries with "..", so each package that contributes to
// analysis semantics exposes its own SourceHash and a caller combines
// them — see internal/derive.SourceHash and internal/index.AnalyzerVersion.
func SourceHash() string {
	return hashEmbeddedGoFiles(sourceFS)
}

// hashEmbeddedGoFiles is shared logic (duplicated per-package rather than
// factored into a shared helper package, deliberately — pulling in a
// cross-package dependency just for this would be more machinery than the
// five lines it saves).
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
