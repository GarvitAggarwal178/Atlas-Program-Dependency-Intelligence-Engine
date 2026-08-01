package derive

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"sort"
)

//go:embed *.go
var sourceFS embed.FS

// SourceHash is a stable hash of this package's own .go source files. See
// internal/parser.SourceHash's doc comment for why this pattern is
// duplicated per-package rather than shared.
func SourceHash() string {
	var names []string
	_ = fs.WalkDir(sourceFS, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		names = append(names, path)
		return nil
	})
	sort.Strings(names)

	h := sha256.New()
	for _, name := range names {
		content, err := sourceFS.ReadFile(name)
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
