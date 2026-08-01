// Package linearize implements architecture.md section 2.1's commit
// linearization: a first-parent walk of one named branch, turning git's DAG
// history into the single total order the interval store's seq column
// requires.
//
// Two honest limits, stated in architecture.md and reproduced here because
// they shape this package's API:
//
//  1. Rebase and force-push silently invalidate every interval. Nothing in
//     a naive design detects history rewriting. The mitigation is a rolling
//     fingerprint over the first-parent commit-hash chain, verified at the
//     start of every index run — see VerifyFingerprint.
//  2. On merge-based workflows, attribution is PR-granular, not
//     commit-granular: a first-parent walk collapses each merged PR to a
//     single seq. That is accepted, not worked around.
package linearize

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os/exec"
	"strings"
)

// Commit is one entry in the linearized sequence.
type Commit struct {
	Seq        int64
	CommitHash string
	ParentHash string // "" for the root commit (seq 0)
}

// Walk returns the first-parent history of branch in repoDir, oldest-first,
// with Seq assigned as the 0-based index in that walk.
//
// `git rev-list --first-parent <branch>` lists commits newest-first, each
// one connected to the next by exactly the first-parent edge — so after
// reversing to oldest-first, commits[i]'s first parent is commits[i-1] by
// construction; no extra `git log --format=%P` lookup is needed to recover
// parent hashes.
func Walk(repoDir, branch string) ([]Commit, error) {
	cmd := exec.Command("git", "-C", repoDir, "rev-list", "--first-parent", branch)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("git rev-list --first-parent %s: %w\nstderr: %s", branch, err, ee.Stderr)
		}
		return nil, fmt.Errorf("git rev-list --first-parent %s: %w", branch, err)
	}

	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil, fmt.Errorf("no commits found on first-parent walk of %s", branch)
	}

	// Reverse newest-first -> oldest-first.
	n := len(lines)
	hashes := make([]string, n)
	for i, l := range lines {
		hashes[n-1-i] = strings.TrimSpace(l)
	}

	commits := make([]Commit, n)
	for i, h := range hashes {
		c := Commit{Seq: int64(i), CommitHash: h}
		if i > 0 {
			c.ParentHash = hashes[i-1]
		}
		commits[i] = c
	}
	return commits, nil
}

// Fingerprint is a rolling hash of the first-parent commit-hash chain
// commits[0:upToSeq+1] (inclusive), architecture.md section 2.1. It is
// "rolling" in the sense that Fingerprint(commits, k) can be recovered
// incrementally from Fingerprint(commits, k-1) — implemented here as
// SHA-256 over the newline-joined ordered hash list, which is equivalent
// to (and simpler than) chaining H(prev || hash) per step, and just as
// sensitive to any change anywhere in the prefix: changing, inserting, or
// reordering any commit_hash in [0, upToSeq] changes the output.
//
// upToSeq must be a valid index into commits (0 <= upToSeq < len(commits)).
func Fingerprint(commits []Commit, upToSeq int64) (string, error) {
	if upToSeq < 0 || int(upToSeq) >= len(commits) {
		return "", fmt.Errorf("upToSeq %d out of range [0, %d)", upToSeq, len(commits))
	}
	var sb strings.Builder
	for i := int64(0); i <= upToSeq; i++ {
		sb.WriteString(commits[i].CommitHash)
		sb.WriteByte('\n')
	}
	sum := sha256.Sum256([]byte(sb.String()))
	return hex.EncodeToString(sum[:]), nil
}

// VerifyResult is the outcome of comparing a freshly-walked history against
// a previously stored fingerprint.
type VerifyResult struct {
	// Stable is true iff the freshly computed fingerprint over
	// commits[0:lastAppliedSeq+1] matches storedFingerprint exactly.
	Stable bool

	// ComputedFingerprint is always populated: the fingerprint the engine
	// should store going forward (on first run, or to advance the
	// watermark after this run's new commits are applied).
	ComputedFingerprint string
}

// VerifyFingerprint re-derives the fingerprint over the prefix of commits
// that was previously indexed (commits[0:lastAppliedSeq+1]) and compares it
// to storedFingerprint. architecture.md section 2.1: "verify it at the
// start of every index run. Mismatch => refuse to proceed and require a
// full re-index."
//
// If lastAppliedSeq < 0 (nothing indexed yet — a fresh repo), there is
// nothing to verify: Stable is trivially true.
func VerifyFingerprint(commits []Commit, lastAppliedSeq int64, storedFingerprint string) (VerifyResult, error) {
	if lastAppliedSeq < 0 {
		return VerifyResult{Stable: true}, nil
	}
	if int(lastAppliedSeq) >= len(commits) {
		// The previously-applied seq no longer exists in the fresh walk at
		// all (history got SHORTER than what we'd indexed) — definitely
		// unstable, not just a hash mismatch.
		return VerifyResult{Stable: false}, nil
	}
	fp, err := Fingerprint(commits, lastAppliedSeq)
	if err != nil {
		return VerifyResult{}, err
	}
	return VerifyResult{
		Stable:               fp == storedFingerprint,
		ComputedFingerprint:  fp,
	}, nil
}
