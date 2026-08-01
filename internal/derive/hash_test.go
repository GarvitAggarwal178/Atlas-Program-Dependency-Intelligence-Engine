package derive_test

import (
	"testing"

	"github.com/yourorg/symex/internal/derive"
)

func TestImplementerSetHash_OrderIndependent(t *testing.T) {
	a := derive.ImplementerSetHash([]string{"pkg.A", "pkg.B", "pkg.C"})
	b := derive.ImplementerSetHash([]string{"pkg.C", "pkg.A", "pkg.B"})
	if a != b {
		t.Fatalf("hash must be order-independent: %s != %s", a, b)
	}
}

func TestImplementerSetHash_DuplicatesCollapse(t *testing.T) {
	a := derive.ImplementerSetHash([]string{"pkg.A", "pkg.B"})
	b := derive.ImplementerSetHash([]string{"pkg.A", "pkg.B", "pkg.A"})
	if a != b {
		t.Fatalf("hash must treat the implementer list as a SET: %s != %s", a, b)
	}
}

func TestImplementerSetHash_ChangesWhenSetChanges(t *testing.T) {
	before := derive.ImplementerSetHash([]string{"pkg.A", "pkg.B"})
	after := derive.ImplementerSetHash([]string{"pkg.A", "pkg.B", "pkg.C"})
	if before == after {
		t.Fatal("adding a new implementer must change the hash")
	}
}

// TestSoundnessFix_MethodSetHashMissesNewImplementer reproduces
// architecture.md section 2.2's worked example directly: two DIFFERENT
// implementer sets for the same interface, where the interface's method
// set itself never changes (interfaces don't carry a method set that
// depends on who implements them) — so MethodSetHash, computed over the
// INTERFACE's own methods, is identical before and after a new type starts
// implementing it. This is exactly the v3 bug: nothing about the
// interface's declaration changed, so a method-set-keyed invalidation
// would never fire. ImplementerSetHash, by contrast, is sensitive to
// exactly this change — that's the whole fix.
func TestSoundnessFix_MethodSetHashMissesNewImplementer_ImplementerSetHashCatchesIt(t *testing.T) {
	ifaceMethods := []string{"Debit", "Credit", "Balance"}

	// The interface's method set is fixed and does not depend on who
	// implements it — before and after a new implementer appears, hashing
	// the INTERFACE's own method set gives the same value. This is v3's bug
	// made concrete: whatever invalidation was keyed on this hash would
	// never fire when a new implementer shows up.
	methodHashBefore := derive.MethodSetHash(ifaceMethods)
	methodHashAfter := derive.MethodSetHash(ifaceMethods)
	if methodHashBefore != methodHashAfter {
		t.Fatal("test setup error: method set hash should be identical (same interface, same methods)")
	}

	// The implementer set DID change (SQLLedger -> SQLLedger + CacheLedger).
	implementersBefore := []string{"billing.SQLLedger"}
	implementersAfter := []string{"billing.SQLLedger", "billing.CacheLedger"}

	implHashBefore := derive.ImplementerSetHash(implementersBefore)
	implHashAfter := derive.ImplementerSetHash(implementersAfter)

	if implHashBefore == implHashAfter {
		t.Fatal("ImplementerSetHash must change when a new implementer is added — this is the section 2.2 fix")
	}
	// The demonstration: method-set hashing is blind to exactly the change
	// implementer-set hashing catches.
	if methodHashBefore == methodHashAfter && implHashBefore == implHashAfter {
		t.Fatal("test did not actually demonstrate the fix — both hashes moved together")
	}
}
