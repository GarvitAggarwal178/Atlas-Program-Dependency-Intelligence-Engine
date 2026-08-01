package index_test

import (
	"context"
	"sort"
	"testing"

	"github.com/yourorg/symex/internal/index"
	"github.com/yourorg/symex/internal/parser"
	"github.com/yourorg/symex/internal/store"
)

// factKey is a canonical, order-independent representation of one computed
// fact + its derivations, for comparing ComputeFacts runs against each
// other regardless of slice/map ordering.
func factKey(f index.FactWithDerivations) string {
	derivs := make([]string, len(f.Derivations))
	for i, d := range f.Derivations {
		derivs[i] = d.InputKind + "=" + d.InputKey + ":" + d.InputHash
	}
	sort.Strings(derivs)
	key := f.Fact.SourceSymbol + "|" + f.Fact.TargetSymbol + "|" + f.Fact.Provenance + "|" + f.Fact.CallSite
	for _, d := range derivs {
		key += "|" + d
	}
	return key
}

func canonicalFactSet(facts []index.FactWithDerivations) []string {
	keys := make([]string, len(facts))
	for i, f := range facts {
		keys[i] = factKey(f)
	}
	sort.Strings(keys)
	return keys
}

// TestDeterminism_ComputeFactsRepeatable is architecture.md invariant I11
// ("derived fact set identical regardless of worker count and scheduling
// order"), narrowed to what's actually buildable right now: no
// goroutine-based parallel parsing exists yet (that's a separate, larger
// piece of work — see docs/FLAGGED.md), but ComputeFacts and the parser
// build several maps fresh on every call (implementerCache, per-package
// interface maps, etc.), and Go deliberately randomizes fresh map
// iteration order within a single process specifically to catch
// order-dependent bugs. Running ComputeFacts repeatedly against the exact
// same on-disk input and comparing canonical (sorted) fact sets exercises
// that randomization directly, without needing multiple worker goroutines
// to prove the point.
func TestDeterminism_ComputeFactsRepeatable(t *testing.T) {
	dir := t.TempDir()
	const mod = "example.com/determinism"

	// Deliberately several files, several interfaces, several implementers
	// each — more surface area for map-iteration-order bugs to show up
	// than a single trivial file would give.
	writeModule(t, dir, map[string]string{
		"go.mod": "module " + mod + "\n\ngo 1.21\n",
		"iface.go": `package main

type Speaker interface {
	Speak() string
}

type Counter interface {
	Count() int
}
`,
		"impls.go": `package main

type Dog struct{}
func (d *Dog) Speak() string { return "woof" }

type Cat struct{}
func (c *Cat) Speak() string { return "meow" }

type Bird struct{}
func (b *Bird) Speak() string { return "tweet" }

type Tally struct{ n int }
func (t *Tally) Count() int { return t.n }

type Score struct{ n int }
func (s *Score) Count() int { return s.n }
`,
		"main.go": `package main

func UseSpeaker(s Speaker) string { return s.Speak() }
func UseCounter(c Counter) int    { return c.Count() }

func A() { UseSpeaker(&Dog{}) }
func B() { UseSpeaker(&Cat{}) }
func C() { UseCounter(&Tally{}) }

func main() { A(); B(); C() }
`,
	})

	const runs = 20
	var results [][]string
	for i := 0; i < runs; i++ {
		pkgs, fset, err := parser.LoadPackages(dir)
		if err != nil {
			t.Fatalf("run %d: LoadPackages: %v", i, err)
		}
		table := parser.BuildSymbolTable(pkgs, fset, dir, mod)
		facts, _, err := index.ComputeFacts(dir, mod, table)
		if err != nil {
			t.Fatalf("run %d: ComputeFacts: %v", i, err)
		}
		results = append(results, canonicalFactSet(facts))
	}

	first := results[0]
	if len(first) == 0 {
		t.Fatal("expected at least some facts")
	}
	for i := 1; i < runs; i++ {
		if len(results[i]) != len(first) {
			t.Fatalf("run %d produced %d facts, run 0 produced %d — non-deterministic fact COUNT", i, len(results[i]), len(first))
		}
		for j := range first {
			if results[i][j] != first[j] {
				t.Fatalf("run %d differs from run 0 at position %d:\n  run 0: %s\n  run %d: %s",
					i, j, first[j], i, results[i][j])
			}
		}
	}
}

// canonicalLiveFactSet reads live facts back from the store and produces
// the same kind of order-independent representation as canonicalFactSet,
// so store-backed pipeline runs can be compared the same way.
func canonicalLiveFactSet(facts []store.Fact) []string {
	keys := make([]string, len(facts))
	for i, f := range facts {
		keys[i] = f.SourceSymbol + "|" + f.TargetSymbol + "|" + f.Provenance + "|" + f.CallSite
	}
	sort.Strings(keys)
	return keys
}

// TestDeterminism_FullPipelineRepeatable is the same property one level
// up: indexing the same commit (fresh repo each time, so runs can't
// interfere with each other) N times must produce the exact same
// live-fact-set every time, read back from the store itself.
func TestDeterminism_FullPipelineRepeatable(t *testing.T) {
	dir := t.TempDir()
	const mod = "example.com/determinism2"
	writeModule(t, dir, map[string]string{
		"go.mod": "module " + mod + "\n\ngo 1.21\n",
		"main.go": `package main

type Speaker interface{ Speak() string }
type Dog struct{}
func (d *Dog) Speak() string { return "woof" }
type Cat struct{}
func (c *Cat) Speak() string { return "meow" }

func Use(s Speaker) string { return s.Speak() }
func main() { Use(&Dog{}) }
`,
	})

	const runs = 5
	var canonical []string
	for i := 0; i < runs; i++ {
		db := openIndexTestDB(t)
		repo := uniqueIndexTestRepo(t)
		if _, err := index.IndexCommitFromRepo(context.Background(), db, dir, mod, repo, 0, "fp-0"); err != nil {
			t.Fatalf("run %d: IndexCommitFromRepo: %v", i, err)
		}
		live, err := store.QueryLiveFacts(context.Background(), db.RawDB(), repo)
		if err != nil {
			t.Fatalf("run %d: QueryLiveFacts: %v", i, err)
		}
		got := canonicalLiveFactSet(live)
		if canonical == nil {
			canonical = got
			continue
		}
		if len(got) != len(canonical) {
			t.Fatalf("run %d: %d facts, run 0: %d facts", i, len(got), len(canonical))
		}
		for j := range canonical {
			if got[j] != canonical[j] {
				t.Fatalf("run %d differs from run 0 at position %d:\n  run 0: %s\n  run %d: %s", i, j, canonical[j], i, got[j])
			}
		}
	}
}
