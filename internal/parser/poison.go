package parser

import (
	"fmt"
	"strings"

	"golang.org/x/tools/go/packages"
)

// Poison-input gate (architecture.md section 3.2).
//
// go/packages does not fail on a broken build. It returns packages with
// Errors populated and incomplete TypesInfo. Deriving facts from a
// partially-typed package writes wrong facts with valid-looking
// derivations — and the differential harness cannot catch this, because a
// full rebuild at the same broken commit is wrong in exactly the same way.
// Both sides agree; both are wrong.
//
// The fix is a hard gate, run before any fact derivation: if any loaded
// package has Errors or nil TypesInfo, refuse to index the commit and
// record why.

// Reason values. These are plain strings, not internal/store constants —
// internal/parser intentionally does not import internal/store (that
// dependency would run the wrong direction: store is the lower-level
// package). The values are chosen to match store.SkipReasonTypeErrors /
// SkipReasonModuleUnavailable / SkipReasonToolchain exactly; keep them in
// sync if either side changes.
const (
	ReasonTypeErrors        = "type_errors"
	ReasonModuleUnavailable = "module_unavailable"
)

// PoisonResult is the outcome of running the gate against a LoadPackages
// result.
type PoisonResult struct {
	// Clean is true iff every loaded package has zero Errors and complete
	// TypesInfo. If false, the commit must be refused: recorded in
	// skipped_commits, not indexed, and excluded from every measurement
	// that depends on N (architecture.md section 3.2).
	Clean bool

	// Reason is ReasonTypeErrors or ReasonModuleUnavailable. Only
	// meaningful when Clean is false.
	Reason string

	// Detail is a human-readable summary suitable for
	// skipped_commits.detail.
	Detail string
}

// CheckPoison inspects the result of LoadPackages and decides whether the
// commit is safe to derive facts from. It must be called, and its result
// honored, before any fact derivation for code paths that need soundness
// (i.e. everything except the legacy ParseRepo path — see its doc comment).
func CheckPoison(pkgs []*packages.Package) PoisonResult {
	// Zero packages is NOT automatically poison. Correction made after a
	// real false positive: a commit that adds only go.mod, with no .go
	// files yet, is a completely ordinary early-repo state — "./..." then
	// legitimately matches nothing, with no error and no per-package
	// Errors/incomplete-TypesInfo signal at all. Treating that as a hard
	// failure inflated the skip rate with commits that were never broken,
	// which section 3.2 explicitly says must not happen ("the skip rate is
	// part of every reported result" — it should reflect real build
	// failures, not empty repos). A genuine module-resolution failure
	// either surfaces as an error from packages.Load itself (returned
	// separately by LoadPackages, upstream of this function) or as
	// per-package Errors/incomplete TypesInfo on whatever DID load — both
	// still caught below. Zero packages with no such signal is just
	// "nothing to derive facts from," which is Clean.
	if len(pkgs) == 0 {
		return PoisonResult{Clean: true}
	}

	var errStrs []string
	incomplete := 0
	for _, pkg := range pkgs {
		for _, e := range pkg.Errors {
			errStrs = append(errStrs, fmt.Sprintf("%s: %s", pkg.PkgPath, e.Error()))
		}
		if pkg.TypesInfo == nil {
			incomplete++
		}
	}

	if len(errStrs) == 0 && incomplete == 0 {
		return PoisonResult{Clean: true}
	}

	// go/packages reports outright type/parse errors via pkg.Errors. A
	// package can also come back with nil TypesInfo and NO entries in
	// pkg.Errors — observed for packages that fail to load for reasons
	// upstream of type-checking (e.g. an unresolvable transitive
	// dependency). Both are poison; they're distinguished only so
	// skipped_commits.reason carries some diagnostic value.
	reason := ReasonTypeErrors
	if len(errStrs) == 0 && incomplete > 0 {
		reason = ReasonModuleUnavailable
	}

	var detail strings.Builder
	detail.WriteString(strings.Join(errStrs, "; "))
	if incomplete > 0 {
		if detail.Len() > 0 {
			detail.WriteString("; ")
		}
		fmt.Fprintf(&detail, "%d package(s) with incomplete TypesInfo", incomplete)
	}

	return PoisonResult{Clean: false, Reason: reason, Detail: detail.String()}
}
