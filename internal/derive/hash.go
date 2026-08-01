// Package derive computes the input hashes that back architecture.md
// section 2.2's derivation-tracking model: every derived fact records the
// hashes of the inputs it was derived from, so invalidation is "withdraw
// exactly the facts whose recorded inputs changed" rather than a
// hand-written rule per trigger type.
package derive

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
)

// ImplementerSetHash is the section 2.2 soundness fix, load-bearing enough
// to warrant its own function rather than being inlined at call sites.
//
// v3's bug: hashing an interface's METHOD SET means adding a new type in a
// brand new file that implements an already-dispatched interface changes
// NOTHING that any existing fact's recorded inputs depend on — the file
// that changed isn't in any dispatch site's FILE/TYPE inputs, the
// interface's method set is unchanged, so nothing invalidates and the new
// implementer's edge is never derived. Silent, permanent false negative.
//
// The fix: hash the interface's IMPLEMENTER SET instead. Adding a type
// changes the set, changes the hash, and any dispatch site whose recorded
// INTERFACE input hash no longer matches gets withdrawn and re-derived —
// turning "derive a new edge" into "retract-then-rederive," which
// provenance-based invalidation handles natively without needing a
// declarative rule engine (architecture.md section 2.2's "why this does
// not require a Datalog engine").
//
// Required properties (architecture.md section 2.2): stable — independent
// of iteration order and of unrelated code. Sorting the implementer names
// before hashing gives order-independence directly; hashing only the
// qualified type names (not e.g. file paths or declaration order) gives
// independence from unrelated code. Duplicate names collapse (a set, not a
// multiset) so accidental double-registration of the same implementer
// can't spuriously change the hash.
func ImplementerSetHash(implementerNames []string) string {
	uniq := make(map[string]struct{}, len(implementerNames))
	for _, n := range implementerNames {
		uniq[n] = struct{}{}
	}
	sorted := make([]string, 0, len(uniq))
	for n := range uniq {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)

	var sb strings.Builder
	for _, n := range sorted {
		sb.WriteString(n)
		sb.WriteByte('\n')
	}
	sum := sha256.Sum256([]byte(sb.String()))
	return hex.EncodeToString(sum[:])
}

// MethodSetHash is v3's REJECTED mechanism, kept here only so tests can
// demonstrate exactly why it's wrong (architecture.md section 2.2's
// worked example) — nothing in the store package should call this for
// INTERFACE input hashing. Hashes the sorted method names only, which is
// blind to which concrete types implement the interface.
func MethodSetHash(methodNames []string) string {
	uniq := make(map[string]struct{}, len(methodNames))
	for _, n := range methodNames {
		uniq[n] = struct{}{}
	}
	sorted := make([]string, 0, len(uniq))
	for n := range uniq {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)

	var sb strings.Builder
	for _, n := range sorted {
		sb.WriteString(n)
		sb.WriteByte('\n')
	}
	sum := sha256.Sum256([]byte(sb.String()))
	return hex.EncodeToString(sum[:])
}

// FileStructuralHash and TypeMethodSetHash round out the derivations.input_hash
// vocabulary from architecture.md section 5's schema comment
// ("FILE: structural hash", "TYPE: method-set hash") for completeness of
// the derivation-tracking model, even though section 2.2's fix is specific
// to INTERFACE. Both simply wrap the same stable-hash primitive used above.

// FileStructuralHash hashes a file's structural content (e.g. the
// canonicalized source produced by internal/canonicalize) — callers pass
// in whatever byte content should be considered "the file's structure" for
// invalidation purposes; this function only does the hashing, not the
// canonicalization.
func FileStructuralHash(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// TypeMethodSetHash hashes a concrete type's own method set (distinct from
// ImplementerSetHash, which hashes an INTERFACE's implementer set — these
// are two different relations and must not be confused).
func TypeMethodSetHash(methodNames []string) string {
	return MethodSetHash(methodNames)
}
