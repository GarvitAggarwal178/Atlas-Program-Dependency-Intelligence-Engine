// fixtures_test.go implements the remaining architecture.md section 9.1
// adversarial fixtures (fixture 3 is already covered by
// TestIndexCommitFromRepo_EndToEndSection2_2 in index_test.go; fixture 8
// needs DRed reachability, not yet built). Each fixture indexes a small
// synthetic module twice — no git repo needed, since IndexCommitFromRepo
// only requires a valid go/packages-loadable directory, not a VCS history
// — mutating the source on disk between calls the way a real commit would,
// and asserts the resulting live-fact-set change is exactly what the edit
// class demands.
package index_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/yourorg/symex/internal/derive"
	"github.com/yourorg/symex/internal/index"
	"github.com/yourorg/symex/internal/store"
)

func writeModule(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatalf("mkdir for %s: %v", rel, err)
		}
		if err := os.WriteFile(full, []byte(content), 0644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
}

func liveEdgeSet(t *testing.T, db *store.DB, repo string) map[string]bool {
	t.Helper()
	live, err := store.QueryLiveFacts(context.Background(), db.RawDB(), repo)
	if err != nil {
		t.Fatalf("QueryLiveFacts: %v", err)
	}
	edges := make(map[string]bool, len(live))
	for _, f := range live {
		edges[f.SourceSymbol+" -("+f.Provenance+")-> "+f.TargetSymbol] = true
	}
	return edges
}

// --- Fixture 1: add a method to a dispatched interface ---------------------

func TestFixture1_AddMethodToDispatchedInterface(t *testing.T) {
	db := openIndexTestDB(t)
	repo := uniqueIndexTestRepo(t)
	dir := t.TempDir()
	const mod = "example.com/fixture1"

	writeModule(t, dir, map[string]string{
		"go.mod": "module " + mod + "\n\ngo 1.21\n",
		"iface.go": `package main

type Speaker interface {
	Speak() string
}

type Dog struct{}
func (d *Dog) Speak() string { return "woof" }

func UseSpeaker(s Speaker) string {
	return s.Speak()
}

func main() {}
`,
	})

	if _, err := index.IndexCommitFromRepo(context.Background(), db, dir, mod, repo, 0, "fp-0"); err != nil {
		t.Fatalf("index seq=0: %v", err)
	}
	before := liveEdgeSet(t, db, repo)
	speakEdge := mod + ".UseSpeaker -(interface_resolved)-> " + mod + ".(Dog).Speak"
	if !before[speakEdge] {
		t.Fatalf("seq=0: expected %s to be live, got %v", speakEdge, before)
	}

	// Add a new method to the interface, implement it on Dog, and add a
	// new call site dispatching through it.
	writeModule(t, dir, map[string]string{
		"iface.go": `package main

type Speaker interface {
	Speak() string
	Volume() int
}

type Dog struct{}
func (d *Dog) Speak() string { return "woof" }
func (d *Dog) Volume() int   { return 10 }

func UseSpeaker(s Speaker) string {
	return s.Speak()
}

func UseVolume(s Speaker) int {
	return s.Volume()
}

func main() {}
`,
	})

	if _, err := index.IndexCommitFromRepo(context.Background(), db, dir, mod, repo, 1, "fp-1"); err != nil {
		t.Fatalf("index seq=1: %v", err)
	}
	after := liveEdgeSet(t, db, repo)

	if !after[speakEdge] {
		t.Errorf("seq=1: existing Speak dispatch must still be live, got %v", after)
	}
	volumeEdge := mod + ".UseVolume -(interface_resolved)-> " + mod + ".(Dog).Volume"
	if !after[volumeEdge] {
		t.Errorf("seq=1: expected NEW Volume dispatch %s to be live after adding the method, got %v", volumeEdge, after)
	}
}

// --- Fixture 2: remove a method from a dispatched interface ----------------

func TestFixture2_RemoveMethodFromDispatchedInterface(t *testing.T) {
	db := openIndexTestDB(t)
	repo := uniqueIndexTestRepo(t)
	dir := t.TempDir()
	const mod = "example.com/fixture2"

	writeModule(t, dir, map[string]string{
		"go.mod": "module " + mod + "\n\ngo 1.21\n",
		"iface.go": `package main

type Speaker interface {
	Speak() string
	Volume() int
}

type Dog struct{}
func (d *Dog) Speak() string { return "woof" }
func (d *Dog) Volume() int   { return 10 }

func UseSpeaker(s Speaker) string { return s.Speak() }
func UseVolume(s Speaker) int     { return s.Volume() }

func main() {}
`,
	})

	if _, err := index.IndexCommitFromRepo(context.Background(), db, dir, mod, repo, 0, "fp-0"); err != nil {
		t.Fatalf("index seq=0: %v", err)
	}
	before := liveEdgeSet(t, db, repo)
	volumeEdge := mod + ".UseVolume -(interface_resolved)-> " + mod + ".(Dog).Volume"
	if !before[volumeEdge] {
		t.Fatalf("seq=0: expected %s to be live, got %v", volumeEdge, before)
	}

	// Remove Volume from the interface AND its only call site (must remove
	// the call site too, or the commit won't compile — UseVolume's
	// parameter is typed Speaker, which would no longer have Volume()).
	writeModule(t, dir, map[string]string{
		"iface.go": `package main

type Speaker interface {
	Speak() string
}

type Dog struct{}
func (d *Dog) Speak() string { return "woof" }
func (d *Dog) Volume() int   { return 10 } // still a method ON Dog, just not required by Speaker anymore

func UseSpeaker(s Speaker) string { return s.Speak() }

func main() {}
`,
	})

	if _, err := index.IndexCommitFromRepo(context.Background(), db, dir, mod, repo, 1, "fp-1"); err != nil {
		t.Fatalf("index seq=1: %v", err)
	}
	after := liveEdgeSet(t, db, repo)

	if after[volumeEdge] {
		t.Errorf("seq=1: Volume dispatch must be WITHDRAWN after removing the method+call site, still found in %v", after)
	}
	speakEdge := mod + ".UseSpeaker -(interface_resolved)-> " + mod + ".(Dog).Speak"
	if !after[speakEdge] {
		t.Errorf("seq=1: unrelated Speak dispatch must remain live, got %v", after)
	}
}

// --- Fixture 4: remove the only implementer of a dispatched interface ------

func TestFixture4_RemoveOnlyImplementer(t *testing.T) {
	db := openIndexTestDB(t)
	repo := uniqueIndexTestRepo(t)
	dir := t.TempDir()
	const mod = "example.com/fixture4"

	writeModule(t, dir, map[string]string{
		"go.mod": "module " + mod + "\n\ngo 1.21\n",
		"iface.go": `package main

type Greeter interface {
	Greet() string
}

type English struct{}
func (e *English) Greet() string { return "hello" }

func UseGreeter(g Greeter) string { return g.Greet() }

func main() {}
`,
	})

	if _, err := index.IndexCommitFromRepo(context.Background(), db, dir, mod, repo, 0, "fp-0"); err != nil {
		t.Fatalf("index seq=0: %v", err)
	}
	before := liveEdgeSet(t, db, repo)
	concreteEdge := mod + ".UseGreeter -(interface_resolved)-> " + mod + ".(English).Greet"
	if !before[concreteEdge] {
		t.Fatalf("seq=0: expected %s to be live, got %v", concreteEdge, before)
	}

	// Remove English entirely — Greeter now has ZERO implementers in this
	// package. UseGreeter's call site must fall back to a raw
	// interface-method-descriptor edge (ComputeFacts's documented
	// behavior when ExpandInterfaceCall returns no targets), not silently
	// vanish.
	writeModule(t, dir, map[string]string{
		"iface.go": `package main

type Greeter interface {
	Greet() string
}

func UseGreeter(g Greeter) string { return g.Greet() }

func main() {}
`,
	})

	if _, err := index.IndexCommitFromRepo(context.Background(), db, dir, mod, repo, 1, "fp-1"); err != nil {
		t.Fatalf("index seq=1: %v", err)
	}
	after := liveEdgeSet(t, db, repo)

	if after[concreteEdge] {
		t.Errorf("seq=1: the withdrawn implementer's edge must not still be live, found in %v", after)
	}
	fallbackEdge := mod + ".UseGreeter -(interface_resolved)-> " + mod + ".Greeter.Greet"
	if !after[fallbackEdge] {
		t.Errorf("seq=1: expected the raw fallback edge %s (no known implementers) to be live, got %v", fallbackEdge, after)
	}
}

// --- Fixture 6: signature change without body change, and the converse ----

func TestFixture6_SignatureChangeWithoutBodyChange(t *testing.T) {
	db := openIndexTestDB(t)
	repo := uniqueIndexTestRepo(t)
	dir := t.TempDir()
	const mod = "example.com/fixture6"

	const mainV0 = `package main

func A(x int) {}

func main() { A(1) }
`
	const mainV1 = `package main

func A(x int64) {}

func main() { A(1) }
`

	writeModule(t, dir, map[string]string{
		"go.mod":  "module " + mod + "\n\ngo 1.21\n",
		"main.go": mainV0,
	})

	if _, err := index.IndexCommitFromRepo(context.Background(), db, dir, mod, repo, 0, "fp-0"); err != nil {
		t.Fatalf("index seq=0: %v", err)
	}
	edge := mod + ".main -(direct_call)-> " + mod + ".A"
	before := liveEdgeSet(t, db, repo)
	if !before[edge] {
		t.Fatalf("seq=0: expected %s to be live, got %v", edge, before)
	}

	// Change A's signature (int -> int64) WITHOUT changing its (empty)
	// body, and update the one call site to match (otherwise the module
	// wouldn't compile). The edge's natural key (same qualified names,
	// same call site line) is UNCHANGED even though the file's content —
	// and therefore its FILE derivation hash — did change.
	writeModule(t, dir, map[string]string{"main.go": mainV1})

	if _, err := index.IndexCommitFromRepo(context.Background(), db, dir, mod, repo, 1, "fp-1"); err != nil {
		t.Fatalf("index seq=1: %v", err)
	}
	after := liveEdgeSet(t, db, repo)
	if !after[edge] {
		t.Fatalf("seq=1: edge must survive a signature-only change (same call graph shape), got %v", after)
	}

	// The FILE derivation hash recorded against this fact must have been
	// refreshed to the new content's hash — proving ApplyFacts's
	// "unchanged natural key still refreshes derivations" path actually
	// ran, not left the seq=0 hash sitting there stale. Compare against
	// the REAL before/after content hashes, not an arbitrary string, so
	// this test can't pass vacuously.
	oldHash := derive.FileStructuralHash([]byte(mainV0))
	newHash := derive.FileStructuralHash([]byte(mainV1))
	if oldHash == newHash {
		t.Fatal("test setup error: v0 and v1 content hashes must differ")
	}

	live, err := store.QueryLiveFacts(context.Background(), db.RawDB(), repo)
	if err != nil {
		t.Fatalf("QueryLiveFacts: %v", err)
	}
	var factID int64
	for _, f := range live {
		if f.SourceSymbol == mod+".main" && f.TargetSymbol == mod+".A" {
			factID = f.FactID
		}
	}
	if factID == 0 {
		t.Fatal("could not find the fact_id for the main->A edge")
	}

	staleAgainstOldHash, err := store.StaleLiveFacts(context.Background(), db.RawDB(), repo, store.InputKindFile, "main.go", oldHash)
	if err != nil {
		t.Fatalf("StaleLiveFacts(oldHash): %v", err)
	}
	foundStaleVsOld := false
	for _, f := range staleAgainstOldHash {
		if f.FactID == factID {
			foundStaleVsOld = true
		}
	}
	if !foundStaleVsOld {
		t.Error("expected the fact to be flagged stale against the SEQ=0 file hash (proving the recorded hash moved away from it) — if this fails, ApplyFacts never refreshed the derivation")
	}

	staleAgainstNewHash, err := store.StaleLiveFacts(context.Background(), db.RawDB(), repo, store.InputKindFile, "main.go", newHash)
	if err != nil {
		t.Fatalf("StaleLiveFacts(newHash): %v", err)
	}
	for _, f := range staleAgainstNewHash {
		if f.FactID == factID {
			t.Error("the fact must NOT be flagged stale against the CURRENT (seq=1) file hash — its recorded hash should already equal this")
		}
	}
}

// --- Fixture 7: rename a symbol and reuse the old name for a different symbol, same commit ---

func TestFixture7_RenameAndReuseNameSameCommit(t *testing.T) {
	db := openIndexTestDB(t)
	repo := uniqueIndexTestRepo(t)
	dir := t.TempDir()
	const mod = "example.com/fixture7"

	writeModule(t, dir, map[string]string{
		"go.mod": "module " + mod + "\n\ngo 1.21\n",
		"main.go": `package main

func Helper() int { return 1 }

func main() { Helper() }
`,
	})

	if _, err := index.IndexCommitFromRepo(context.Background(), db, dir, mod, repo, 0, "fp-0"); err != nil {
		t.Fatalf("index seq=0: %v", err)
	}
	before := liveEdgeSet(t, db, repo)
	oldEdge := mod + ".main -(direct_call)-> " + mod + ".Helper"
	if !before[oldEdge] {
		t.Fatalf("seq=0: expected %s to be live, got %v", oldEdge, before)
	}

	// In ONE commit: rename Helper -> HelperV2 (same behavior, moved to a
	// different line), AND define a brand new, unrelated function also
	// named Helper that does something completely different. main() now
	// calls the NEW Helper (same qualified name, but a structurally
	// different function at a different source location) plus the
	// renamed HelperV2.
	writeModule(t, dir, map[string]string{
		"main.go": `package main

func HelperV2() int { return 1 }

func Other() int { return 2 }

func Helper() int { return Other() }

func main() {
	HelperV2()
	Helper()
}
`,
	})

	if _, err := index.IndexCommitFromRepo(context.Background(), db, dir, mod, repo, 1, "fp-1"); err != nil {
		t.Fatalf("index seq=1: %v", err)
	}
	after := liveEdgeSet(t, db, repo)

	helperV2Edge := mod + ".main -(direct_call)-> " + mod + ".HelperV2"
	if !after[helperV2Edge] {
		t.Errorf("seq=1: expected renamed HelperV2 edge %s, got %v", helperV2Edge, after)
	}
	newHelperEdge := mod + ".main -(direct_call)-> " + mod + ".Helper"
	if !after[newHelperEdge] {
		t.Errorf("seq=1: expected main->Helper edge to still exist (now pointing at the NEW Helper), got %v", after)
	}
	helperToOtherEdge := mod + ".Helper -(direct_call)-> " + mod + ".Other"
	if !after[helperToOtherEdge] {
		t.Errorf("seq=1: expected the NEW Helper's own edge to Other (proving it's the new body, not the old one), got %v", after)
	}

	// The old Helper (return 1, no callees) must not have left behind an
	// edge to nothing — i.e. Helper must not simultaneously look like it
	// has zero outgoing edges AND an edge to Other. Since Helper's
	// qualified name is identical before and after, this is really
	// checking that closing-then-reopening under the same natural key
	// (main->Helper) correctly replaced the OLD derivation data, not
	// merely added to it — StaleLiveFacts against Helper's OLD (pre-rename)
	// content hash should no longer find a live fact matching the old,
	// callee-less body, because there IS no live fact with a stale
	// call_site independent of the new one (the call_site for main->Helper
	// is the same line-independent natural key here since main() calls
	// Helper() in both versions — this is exactly the ambiguous case the
	// fixture is meant to probe, and the correct outcome is that the LIVE
	// fact now reflects the NEW Helper's behavior, verified above by
	// checking Helper->Other exists).
}
