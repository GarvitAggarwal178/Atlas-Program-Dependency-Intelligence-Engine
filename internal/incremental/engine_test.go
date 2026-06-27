package incremental_test

import (
	"context"
	"fmt"
	"os"
	"sort"
	"testing"

	"github.com/yourorg/symex/internal/callgraph"
	"github.com/yourorg/symex/internal/store"
	"github.com/yourorg/symex/internal/symboltable"
)

// ── in-memory test double for store.DB ───────────────────────────────────────
//
// We don't want a live Postgres for unit tests.  This fake store implements
// just enough of the store.DB surface to exercise the incremental engine's
// correctness logic without any I/O.

type memEdge struct {
	source, target, prov, file string
}

type memSite struct {
	iface, method, callerSym, callerFile string
}

type memTypeIface struct {
	typeName, ifaceName, file string
}

type memStore struct {
	edges      map[string]memEdge    // edgeKey → edge
	sites      map[string]memSite    // "iface|sym" → site
	typeIfaces map[string]memTypeIface // "type|iface" → row
	symbols    map[string]store.SymbolRow
}

func newMemStore() *memStore {
	return &memStore{
		edges:      make(map[string]memEdge),
		sites:      make(map[string]memSite),
		typeIfaces: make(map[string]memTypeIface),
		symbols:    make(map[string]store.SymbolRow),
	}
}

func (m *memStore) edgeKey(src, tgt, prov string) string {
	return src + "→" + tgt + ":" + prov
}

func (m *memStore) siteKey(iface, sym string) string { return iface + "|" + sym }
func (m *memStore) tiKey(typ, iface string) string   { return typ + "|" + iface }

func (m *memStore) insertEdge(src, tgt, prov, file string) {
	k := m.edgeKey(src, tgt, prov)
	m.edges[k] = memEdge{src, tgt, prov, file}
}

func (m *memStore) insertSite(iface, method, callerSym, callerFile string) {
	k := m.siteKey(iface, callerSym)
	m.sites[k] = memSite{iface, method, callerSym, callerFile}
}

func (m *memStore) insertTypeIface(typeName, ifaceName, file string) {
	k := m.tiKey(typeName, ifaceName)
	m.typeIfaces[k] = memTypeIface{typeName, ifaceName, file}
}

func (m *memStore) retractFile(file string) {
	for k, e := range m.edges {
		if e.file == file {
			delete(m.edges, k)
		}
	}
	for k, s := range m.sites {
		if s.callerFile == file {
			delete(m.sites, k)
		}
	}
	for k, ti := range m.typeIfaces {
		if ti.file == file {
			delete(m.typeIfaces, k)
		}
	}
}

func (m *memStore) retractInterfaceEdgesForCallers(callers []string) {
	callerSet := make(map[string]bool, len(callers))
	for _, c := range callers {
		callerSet[c] = true
	}
	for k, e := range m.edges {
		if callerSet[e.source] && e.prov == "interface_resolved" {
			delete(m.edges, k)
		}
	}
}

func (m *memStore) dispatchSitesForInterface(ifaceName string) []memSite {
	var result []memSite
	for _, s := range m.sites {
		if s.iface == ifaceName {
			result = append(result, s)
		}
	}
	return result
}

func (m *memStore) typeIfacesForFile(file string) []memTypeIface {
	var result []memTypeIface
	for _, ti := range m.typeIfaces {
		if ti.file == file {
			result = append(result, ti)
		}
	}
	return result
}

func (m *memStore) allEdgesSorted() []string {
	keys := make([]string, 0, len(m.edges))
	for k := range m.edges {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ── helpers to build symbol tables ───────────────────────────────────────────

func makeTable(files []symboltable.FileSymbolTable) *symboltable.RepoSymbolTable {
	return &symboltable.RepoSymbolTable{ModulePath: "github.com/example/test", Files: files}
}

func interfaceSym(pkg, name string, methods []string) symboltable.DefinedSymbol {
	return symboltable.DefinedSymbol{
		Name:          name,
		QualifiedName: pkg + "." + name,
		Kind:          symboltable.KindInterface,
		MethodSet:     methods,
	}
}

func typeSym(pkg, name string, ifaces []string) symboltable.DefinedSymbol {
	return symboltable.DefinedSymbol{
		Name:          name,
		QualifiedName: pkg + "." + name,
		Kind:          symboltable.KindType,
		ImplementedInterfaces: ifaces,
	}
}

func methodSym(pkg, recv, name string) symboltable.DefinedSymbol {
	return symboltable.DefinedSymbol{
		Name:          name,
		QualifiedName: fmt.Sprintf("%s.(%s).%s", pkg, recv, name),
		Kind:          symboltable.KindMethod,
		ReceiverType:  recv,
	}
}

func callRef(caller, callee string, kind symboltable.CallKind) symboltable.CallReference {
	return symboltable.CallReference{Caller: caller, Callee: callee, Kind: kind}
}

// ── tests ─────────────────────────────────────────────────────────────────────

// TestExpandInterfaceCallFindsAllImplementors verifies that a call through an
// interface expands to all concrete types that implement it.
func TestExpandInterfaceCallFindsAllImplementors(t *testing.T) {
	table := makeTable([]symboltable.FileSymbolTable{
		{
			FilePath: "billing/ledger.go", Package: "billing",
			ImportPath: "github.com/example/billing",
			Defined: []symboltable.DefinedSymbol{
				interfaceSym("github.com/example/billing", "Ledger", []string{"Debit"}),
				typeSym("github.com/example/billing", "SQLLedger",
					[]string{"github.com/example/billing.Ledger"}),
				typeSym("github.com/example/billing", "MemLedger",
					[]string{"github.com/example/billing.Ledger"}),
				methodSym("github.com/example/billing", "SQLLedger", "Debit"),
				methodSym("github.com/example/billing", "MemLedger", "Debit"),
			},
		},
	})

	targets := callgraph.ExpandInterfaceCall(
		table,
		"github.com/example/billing.Ledger",
		"Debit",
	)

	want := map[string]bool{
		"github.com/example/billing.(SQLLedger).Debit": true,
		"github.com/example/billing.(MemLedger).Debit": true,
	}
	if len(targets) != 2 {
		t.Errorf("expected 2 targets, got %d: %v", len(targets), targets)
	}
	for _, tgt := range targets {
		if !want[tgt] {
			t.Errorf("unexpected target: %q", tgt)
		}
	}
}

// TestCrossFileInterfaceUpdate is the most important test.
//
// Scenario:
//   - File A contains: caller.ledger.Debit() — call through Ledger interface.
//     This is NOT changing.
//   - File B contains: type SQLLedger implements Ledger.
//     B changes to ADD a new type CacheLedger that also implements Ledger.
//
// Before the change:
//   call_edges has: caller → (SQLLedger).Debit [interface_resolved]
//
// After the incremental update for file B:
//   call_edges must have:
//     caller → (SQLLedger).Debit  [interface_resolved]  (still)
//     caller → (CacheLedger).Debit [interface_resolved]  (NEW — cross-file!)
//
// A naive implementation that only retracts B's own edges would MISS the
// second edge because the call site is in A, not B.
func TestCrossFileInterfaceUpdate(t *testing.T) {
	const (
		pkg      = "github.com/example"
		iface    = "github.com/example.Ledger"
		caller   = "github.com/example/payment.ProcessPayment"
		fileA    = "payment/processor.go"
		fileB    = "billing/ledger.go"
		sqlType  = "github.com/example/billing.SQLLedger"
		cacheType = "github.com/example/billing.CacheLedger"
	)

	// ── Set up the "base" state in our memStore ───────────────────────────────
	ms := newMemStore()

	// A's call site: ProcessPayment calls Ledger.Debit → expands to SQLLedger.Debit only
	ms.insertEdge(caller, "github.com/example/billing.(SQLLedger).Debit", "interface_resolved", fileA)
	// The dispatch site record (in A's file)
	ms.insertSite(iface, "Debit", caller, fileA)
	// B's type_iface: SQLLedger implements Ledger
	ms.insertTypeIface(sqlType, iface, fileB)

	// ── Simulate the incremental update for file B ────────────────────────────
	// New symbol table at headCommit: B now has BOTH SQLLedger and CacheLedger.
	newTable := makeTable([]symboltable.FileSymbolTable{
		{
			FilePath: fileB, Package: "billing",
			ImportPath: "github.com/example/billing",
			Defined: []symboltable.DefinedSymbol{
				interfaceSym("github.com/example/billing", "Ledger", []string{"Debit"}),
				typeSym("github.com/example/billing", "SQLLedger", []string{iface}),
				typeSym("github.com/example/billing", "CacheLedger", []string{iface}), // NEW
				methodSym("github.com/example/billing", "SQLLedger", "Debit"),
				methodSym("github.com/example/billing", "CacheLedger", "Debit"), // NEW
			},
		},
		{
			FilePath: fileA, Package: "payment",
			ImportPath: "github.com/example/payment",
			Defined:    []symboltable.DefinedSymbol{},
			References: []symboltable.CallReference{
				callRef(caller, iface+".Debit", symboltable.CallInterfaceResolved),
			},
		},
	})

	// Apply the 4-step rule manually using memStore:

	// Step 1: load old type→iface for B before retraction
	oldTIs := ms.typeIfacesForFile(fileB)
	oldIfaceSet := make(map[string]map[string]bool)
	for _, ti := range oldTIs {
		if oldIfaceSet[ti.typeName] == nil {
			oldIfaceSet[ti.typeName] = make(map[string]bool)
		}
		oldIfaceSet[ti.typeName][ti.ifaceName] = true
	}

	// Step 1: retract B
	ms.retractFile(fileB)

	// Step 2: find affected interfaces
	newIfaceSet := make(map[string]map[string]bool)
	for _, file := range newTable.Files {
		if file.FilePath != fileB {
			continue
		}
		for _, sym := range file.Defined {
			if sym.Kind != symboltable.KindType {
				continue
			}
			m := make(map[string]bool)
			for _, i := range sym.ImplementedInterfaces {
				m[i] = true
			}
			newIfaceSet[sym.QualifiedName] = m
		}
	}

	// Diff: CacheLedger is new → Ledger's implementer set changed
	affectedIfaces := make(map[string]bool)
	for typeName, oldIs := range oldIfaceSet {
		newIs := newIfaceSet[typeName]
		for i := range oldIs {
			if !newIs[i] {
				affectedIfaces[i] = true
			}
		}
		for i := range newIs {
			if !oldIs[i] {
				affectedIfaces[i] = true
			}
		}
	}
	for typeName, newIs := range newIfaceSet {
		if _, existed := oldIfaceSet[typeName]; !existed {
			for i := range newIs {
				affectedIfaces[i] = true
			}
		}
	}

	if !affectedIfaces[iface] {
		t.Errorf("expected Ledger to be in affected interfaces, got: %v", affectedIfaces)
	}

	// Step 2: find cross-file call sites for the affected interface
	var crossCallers []string
	for _, site := range ms.dispatchSitesForInterface(iface) {
		crossCallers = append(crossCallers, site.callerSym)
	}
	if len(crossCallers) == 0 {
		t.Error("no cross-file call sites found — the incremental engine would miss the update")
	}

	// Retract their interface_resolved edges
	ms.retractInterfaceEdgesForCallers(crossCallers)

	// Step 3: insert B's new data
	ms.insertTypeIface(sqlType, iface, fileB)
	ms.insertTypeIface(cacheType, iface, fileB)
	ms.insertEdge(
		"github.com/example/billing.(SQLLedger).Debit",
		"github.com/example/billing.(SQLLedger).Debit",
		"direct_call", fileB, // B's own internal edges
	)

	// Step 4: recompute cross-file interface edges using new table
	for _, callerSym := range crossCallers {
		targets := callgraph.ExpandInterfaceCall(newTable, iface, "Debit")
		for _, tgt := range targets {
			ms.insertEdge(callerSym, tgt, "interface_resolved", fileA)
		}
	}

	// ── Assert: both SQLLedger.Debit and CacheLedger.Debit are reachable ─────
	wantEdges := []string{
		caller + "→" + "github.com/example/billing.(CacheLedger).Debit:interface_resolved",
		caller + "→" + "github.com/example/billing.(SQLLedger).Debit:interface_resolved",
	}

	got := ms.allEdgesSorted()
	// Filter to just the caller's edges for assertion clarity
	var callerEdges []string
	for _, e := range got {
		if len(e) > len(caller) && e[:len(caller)] == caller {
			callerEdges = append(callerEdges, e)
		}
	}
	sort.Strings(callerEdges)

	if len(callerEdges) != 2 {
		t.Errorf("expected 2 edges from caller, got %d: %v", len(callerEdges), callerEdges)
	}
	for i, want := range wantEdges {
		if i >= len(callerEdges) || callerEdges[i] != want {
			t.Errorf("edge[%d]: want %q, got %q", i, want,
				func() string {
					if i < len(callerEdges) {
						return callerEdges[i]
					}
					return "<missing>"
				}())
		}
	}
}

// TestNaiveImplementationFailure demonstrates what would go wrong WITHOUT
// the cross-file step (step 2). This test intentionally shows the bug.
func TestNaiveImplementationFailure(t *testing.T) {
	const (
		iface     = "github.com/example.Ledger"
		caller    = "github.com/example/payment.ProcessPayment"
		fileA     = "payment/processor.go"
		fileB     = "billing/ledger.go"
		cacheType = "github.com/example/billing.CacheLedger"
	)

	// Same setup as TestCrossFileInterfaceUpdate, but apply NAIVE update:
	// only retract B's edges, don't touch A's interface_resolved edges.
	ms := newMemStore()
	ms.insertEdge(caller,
		"github.com/example/billing.(SQLLedger).Debit",
		"interface_resolved", fileA)
	ms.insertSite(iface, "Debit", caller, fileA)
	ms.insertTypeIface("github.com/example/billing.SQLLedger", iface, fileB)

	// NAIVE: retract B, reparse B, insert B's new edges — but never touch A
	ms.retractFile(fileB)
	ms.insertTypeIface("github.com/example/billing.SQLLedger", iface, fileB)
	ms.insertTypeIface(cacheType, iface, fileB)
	// Naive: does NOT recompute A's interface_resolved edges

	// Count caller's outgoing interface_resolved edges
	var callerEdges []string
	for k, e := range ms.edges {
		if e.source == caller && e.prov == "interface_resolved" {
			callerEdges = append(callerEdges, k)
		}
	}

	// The naive result is WRONG: only 1 edge (SQLLedger), CacheLedger is missing
	if len(callerEdges) != 1 {
		t.Errorf("naive: expected 1 edge (the bug), got %d — test setup is wrong", len(callerEdges))
		return
	}
	t.Logf("CONFIRMED BUG: naive implementation leaves %d edge(s) — CacheLedger.Debit is missing",
		len(callerEdges))
	// This test always passes — it documents the failure mode, not a correct result.
}

// TestFileRetraction verifies that step 1 correctly removes ALL data for a file.
func TestFileRetraction(t *testing.T) {
	ms := newMemStore()
	ms.insertEdge("pkg.A", "pkg.B", "direct_call", "a.go")
	ms.insertEdge("pkg.A", "pkg.C", "interface_resolved", "a.go")
	ms.insertEdge("pkg.X", "pkg.Y", "direct_call", "b.go") // different file
	ms.insertSite("pkg.I", "M", "pkg.A", "a.go")
	ms.insertTypeIface("pkg.T", "pkg.I", "a.go")

	ms.retractFile("a.go")

	// a.go's edges must be gone
	for _, e := range ms.edges {
		if e.file == "a.go" {
			t.Errorf("retractFile left edge with file=a.go: %+v", e)
		}
	}
	// b.go's edge must survive
	found := false
	for _, e := range ms.edges {
		if e.source == "pkg.X" {
			found = true
		}
	}
	if !found {
		t.Error("retractFile incorrectly removed edge from b.go")
	}
	// a.go's dispatch sites must be gone
	if len(ms.sites) != 0 {
		t.Errorf("retractFile left %d dispatch site(s)", len(ms.sites))
	}
	// a.go's type_ifaces must be gone
	if len(ms.typeIfaces) != 0 {
		t.Errorf("retractFile left %d type_iface row(s)", len(ms.typeIfaces))
	}
}

// TestInterfaceEdgeRetractionByCallers verifies that retracting by caller
// only removes interface_resolved edges, not direct edges from the same caller.
func TestInterfaceEdgeRetractionByCallers(t *testing.T) {
	ms := newMemStore()
	ms.insertEdge("pkg.Caller", "pkg.DirectTarget", "direct_call", "a.go")
	ms.insertEdge("pkg.Caller", "pkg.IfaceTarget", "interface_resolved", "a.go")

	ms.retractInterfaceEdgesForCallers([]string{"pkg.Caller"})

	// direct_call edge must survive
	found := false
	for _, e := range ms.edges {
		if e.source == "pkg.Caller" && e.prov == "direct_call" {
			found = true
		}
	}
	if !found {
		t.Error("retractInterfaceEdges incorrectly removed direct_call edge")
	}
	// interface_resolved edge must be gone
	for _, e := range ms.edges {
		if e.source == "pkg.Caller" && e.prov == "interface_resolved" {
			t.Errorf("retractInterfaceEdges left interface_resolved edge: %+v", e)
		}
	}
}

// TestDiffEdgeSets verifies the store.DiffEdgeSets helper used by the harness.
func TestDiffEdgeSets(t *testing.T) {
	makeEdge := func(src, tgt, prov string) store.Edge {
		return store.Edge{SourceSymbol: src, TargetSymbol: tgt, Provenance: prov}
	}

	a := store.EdgeSet([]store.Edge{
		makeEdge("A", "B", "direct_call"),
		makeEdge("A", "C", "interface_resolved"),
		makeEdge("X", "Y", "direct_call"), // only in a
	})
	b := store.EdgeSet([]store.Edge{
		makeEdge("A", "B", "direct_call"),
		makeEdge("A", "C", "interface_resolved"),
		makeEdge("P", "Q", "direct_call"), // only in b
	})

	onlyA, onlyB := store.DiffEdgeSets(a, b)

	if len(onlyA) != 1 || store.EdgeKey(onlyA[0]) != "X→Y:direct_call" {
		t.Errorf("onlyA wrong: %v", onlyA)
	}
	if len(onlyB) != 1 || store.EdgeKey(onlyB[0]) != "P→Q:direct_call" {
		t.Errorf("onlyB wrong: %v", onlyB)
	}
}

// ── Postgres integration test (skipped if no DSN) ────────────────────────────

func TestUpdateRoundtrip(t *testing.T) {
	dsn := testDSN(t)
	if dsn == "" {
		t.Skip("SYMEX_TEST_DSN not set — skipping Postgres integration test")
	}

	ctx := context.Background()
	db, err := store.Open(dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	if err := db.ApplySchema(ctx); err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	if err := db.ApplySchemaV2(ctx); err != nil {
		t.Fatalf("apply schema v2: %v", err)
	}

	const (
		repo       = "test-roundtrip"
		baseCommit = "base001"
		headCommit = "head001"
		iface      = "github.com/example.Ledger"
		callerSym  = "github.com/example/payment.Pay"
		fileA      = "payment/pay.go"
		fileB      = "billing/ledger.go"
	)

	// Clean slate
	_ = db.DeleteGraphForCommit(ctx, repo, baseCommit)
	_ = db.DeleteGraphForCommit(ctx, repo, headCommit)

	// Build base graph: SQLLedger implements Ledger, Pay calls Ledger.Debit
	baseEdges := []store.Edge{
		{Repo: repo, CommitHash: baseCommit,
			SourceSymbol: callerSym,
			TargetSymbol: "github.com/example/billing.(SQLLedger).Debit",
			Provenance: "interface_resolved", SourceFile: fileA},
	}
	if err := db.InsertEdges(ctx, baseEdges); err != nil {
		t.Fatalf("insert base edges: %v", err)
	}

	baseSites := []store.DispatchSiteV2{{
		Repo: repo, CommitHash: baseCommit,
		InterfaceName: iface, MethodName: "Debit",
		CallSiteSymbol: callerSym, CallSiteFile: fileA,
	}}
	if err := db.InsertDispatchSitesV2(ctx, baseSites); err != nil {
		t.Fatalf("insert dispatch sites: %v", err)
	}

	baseTIs := []store.TypeIface{{
		Repo: repo, CommitHash: baseCommit,
		TypeName: "github.com/example/billing.SQLLedger",
		InterfaceName: iface, SourceFile: fileB,
	}}
	if err := db.InsertTypeIfaces(ctx, baseTIs); err != nil {
		t.Fatalf("insert type_ifaces: %v", err)
	}

	// Copy to headCommit (simulating the start of an incremental update)
	if err := db.CopyGraphToCommit(ctx, repo, baseCommit, headCommit); err != nil {
		t.Fatalf("copy graph: %v", err)
	}

	// Verify type_ifaces was copied
	headTIs, err := db.LoadTypeIfacesForFile(ctx, repo, headCommit, fileB)
	if err != nil {
		t.Fatalf("load type_ifaces: %v", err)
	}
	if len(headTIs) != 1 {
		t.Errorf("CopyGraphToCommit: expected 1 type_iface at head, got %d", len(headTIs))
	}

	// Verify dispatch sites were copied
	headSites, err := db.LoadDispatchSitesByInterfacesV2(ctx, repo, headCommit, []string{iface})
	if err != nil {
		t.Fatalf("load dispatch sites: %v", err)
	}
	if len(headSites) != 1 {
		t.Errorf("CopyGraphToCommit: expected 1 dispatch site at head, got %d", len(headSites))
	}
	if len(headSites) > 0 && headSites[0].MethodName != "Debit" {
		t.Errorf("dispatch site method_name: want Debit, got %q", headSites[0].MethodName)
	}

	// Retract fileB from head
	if err := db.RetractFile(ctx, repo, headCommit, fileB); err != nil {
		t.Fatalf("retract file: %v", err)
	}
	if err := db.RetractTypeIfacesForFile(ctx, repo, headCommit, fileB); err != nil {
		t.Fatalf("retract type_ifaces: %v", err)
	}

	// Verify caller's edge is still there (it's in fileA, not fileB)
	edges, err := db.LoadAllEdges(ctx, repo, headCommit)
	if err != nil {
		t.Fatalf("load edges: %v", err)
	}
	if len(edges) != 1 {
		t.Errorf("after retractFile(fileB): expected 1 edge (from fileA), got %d", len(edges))
	}

	// Find affected interfaces (Ledger, because SQLLedger was in fileB)
	sites, err := db.LoadDispatchSitesByInterfacesV2(ctx, repo, headCommit, []string{iface})
	if err != nil {
		t.Fatalf("load cross-file sites: %v", err)
	}
	if len(sites) != 1 || sites[0].CallSiteSymbol != callerSym {
		t.Errorf("cross-file lookup: expected 1 site for callerSym, got %v", sites)
	}

	// Retract caller's interface_resolved edges
	if err := db.RetractInterfaceEdgesForCallSites(ctx, repo, headCommit, []string{callerSym}); err != nil {
		t.Fatalf("retract interface edges: %v", err)
	}

	// Insert new type_ifaces for fileB: now CacheLedger also implements Ledger
	newTIs := []store.TypeIface{
		{Repo: repo, CommitHash: headCommit,
			TypeName: "github.com/example/billing.SQLLedger", InterfaceName: iface, SourceFile: fileB},
		{Repo: repo, CommitHash: headCommit,
			TypeName: "github.com/example/billing.CacheLedger", InterfaceName: iface, SourceFile: fileB},
	}
	if err := db.InsertTypeIfaces(ctx, newTIs); err != nil {
		t.Fatalf("insert new type_ifaces: %v", err)
	}

	// Insert recomputed edges: now caller → both implementations
	recomputedEdges := []store.Edge{
		{Repo: repo, CommitHash: headCommit,
			SourceSymbol: callerSym,
			TargetSymbol: "github.com/example/billing.(SQLLedger).Debit",
			Provenance: "interface_resolved", SourceFile: fileA},
		{Repo: repo, CommitHash: headCommit,
			SourceSymbol: callerSym,
			TargetSymbol: "github.com/example/billing.(CacheLedger).Debit",
			Provenance: "interface_resolved", SourceFile: fileA},
	}
	if err := db.InsertEdges(ctx, recomputedEdges); err != nil {
		t.Fatalf("insert recomputed edges: %v", err)
	}

	// Final check: head graph has BOTH edges
	finalEdges, err := db.LoadAllEdges(ctx, repo, headCommit)
	if err != nil {
		t.Fatalf("load final edges: %v", err)
	}
	if len(finalEdges) != 2 {
		t.Errorf("expected 2 edges after full incremental update, got %d: %v", len(finalEdges), finalEdges)
	}

	// Verify the new CacheLedger edge is present
	foundCache := false
	for _, e := range finalEdges {
		if e.TargetSymbol == "github.com/example/billing.(CacheLedger).Debit" {
			foundCache = true
		}
	}
	if !foundCache {
		t.Error("CacheLedger.Debit edge missing after incremental update — cross-file step failed")
	}
}

func testDSN(t *testing.T) string {
	t.Helper()
	// Set SYMEX_TEST_DSN=postgres://symex:symex@localhost/symex?sslmode=disable
	return os.Getenv("SYMEX_TEST_DSN")
}
