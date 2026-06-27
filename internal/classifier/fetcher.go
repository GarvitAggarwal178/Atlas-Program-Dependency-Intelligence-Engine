package classifier

import (
	"fmt"
	"os/exec"
	"strings"
)

// GitFetcher retrieves file content at a specific git commit by running
// `git show <commit>:<path>` in the repository directory.
type GitFetcher struct {
	// RepoDir is the path to the git repository root (contains .git/).
	RepoDir string
}

// FetchAt returns the content of repoRelativePath at commitSHA.
// Returns an error if the file did not exist at that commit (e.g. new file).
func (g *GitFetcher) FetchAt(commitSHA, repoRelativePath string) ([]byte, error) {
	// git show <sha>:<path> prints the file content at that commit to stdout.
	// Exit code 128 means the object doesn't exist (file not present at commit).
	ref := commitSHA + ":" + repoRelativePath
	cmd := exec.Command("git", "show", ref)
	cmd.Dir = g.RepoDir

	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("git show %s: %w (stderr: %s)",
				ref, err, strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("git show %s: %w", ref, err)
	}
	return out, nil
}

// MockFetcher is a SourceFetcher for tests. It returns pre-loaded source bytes
// keyed by (commitSHA, filePath). If a key is missing it returns an error,
// simulating a file that didn't exist at that commit.
type MockFetcher struct {
	// Sources maps "commitSHA:filePath" → source bytes.
	Sources map[string][]byte
}

// NewMockFetcher builds a MockFetcher from a flat map for convenience.
func NewMockFetcher(sources map[string][]byte) *MockFetcher {
	return &MockFetcher{Sources: sources}
}

// FetchAt returns the source registered under "commitSHA:filePath".
func (m *MockFetcher) FetchAt(commitSHA, repoRelativePath string) ([]byte, error) {
	key := commitSHA + ":" + repoRelativePath
	src, ok := m.Sources[key]
	if !ok {
		return nil, fmt.Errorf("mock: no source registered for %q", key)
	}
	return src, nil
}

// key is the lookup key for MockFetcher. Helper for test setup.
func key(commit, path string) string {
	return commit + ":" + path
}

// MockSources is a convenience builder for MockFetcher.Sources.
// Usage:
//
//	fetcher := NewMockFetcher(MockSources{
//	    {"abc", "foo.go"}: []byte(`package p; func F() {}`),
//	})
type MockSources map[[2]string][]byte

// Build converts a MockSources map to the flat string-keyed map that
// MockFetcher expects.
func (ms MockSources) Build() map[string][]byte {
	result := make(map[string][]byte, len(ms))
	for k, v := range ms {
		result[k[0]+":"+k[1]] = v
	}
	return result
}
