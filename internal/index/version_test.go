package index_test

import (
	"testing"

	"github.com/yourorg/symex/internal/index"
)

// TestAnalyzerVersion_Stable checks the parts of I13 that are actually
// testable at runtime without rebuilding the binary with different source:
// determinism (same value every call) and non-triviality (not empty, not
// some degenerate constant). The "changes when source changes" property is
// structurally guaranteed by go:embed being content-addressed — there's no
// runtime code path where it could NOT change if the embedded bytes did.
func TestAnalyzerVersion_Stable(t *testing.T) {
	v1 := index.AnalyzerVersion()
	v2 := index.AnalyzerVersion()
	if v1 != v2 {
		t.Fatalf("AnalyzerVersion must be deterministic within one build: %q != %q", v1, v2)
	}
	if len(v1) < 32 {
		t.Fatalf("AnalyzerVersion looks degenerate: %q", v1)
	}
}
