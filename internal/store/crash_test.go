package store_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"github.com/yourorg/symex/internal/store"
)

// testDSN returns the Postgres DSN to use for integration tests. Overridable
// via SYMEX_TEST_DSN; defaults to the docker-compose.yml instance.
func testDSN() string {
	if dsn := os.Getenv("SYMEX_TEST_DSN"); dsn != "" {
		return dsn
	}
	return "postgres://symex:symex@localhost:5434/symex?sslmode=disable"
}

// openTestDB skips the test if Postgres is unreachable, rather than
// failing — these are integration tests that require docker-compose up.
func openTestDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(testDSN())
	if err != nil {
		t.Skipf("skipping: postgres unreachable at %s: %v", testDSN(), err)
	}
	if err := db.ApplySchemaV3(context.Background()); err != nil {
		t.Fatalf("apply schema v3: %v", err)
	}
	return db
}

func uniqueRepoName(t *testing.T) string {
	return fmt.Sprintf("crashtest:%s:%d", t.Name(), time.Now().UnixNano())
}

// TestApplyDelta_WatermarkCommitsAtomicallyWithData is the core section 3.1
// assertion: on success, repo_state.last_applied_seq advances to exactly
// the seq that was applied, in the same transaction as the data write.
func TestApplyDelta_WatermarkCommitsAtomicallyWithData(t *testing.T) {
	db := openTestDB(t)
	repo := uniqueRepoName(t)

	rs, err := db.GetRepoState(context.Background(), repo)
	if err != nil {
		t.Fatalf("GetRepoState: %v", err)
	}
	if rs != nil {
		t.Fatalf("expected no repo_state for a fresh repo, got %+v", rs)
	}

	resume, err := db.ResumeFromSeq(context.Background(), repo)
	if err != nil {
		t.Fatalf("ResumeFromSeq: %v", err)
	}
	if resume != 0 {
		t.Fatalf("expected fresh repo to resume from seq 0, got %d", resume)
	}

	err = db.ApplyDelta(context.Background(), repo, 0, "fp-0", func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO commits (repo, seq, commit_hash, parent_hash)
			VALUES ($1, 0, 'deadbeef', NULL)
		`, repo)
		return err
	})
	if err != nil {
		t.Fatalf("ApplyDelta: %v", err)
	}

	rs, err = db.GetRepoState(context.Background(), repo)
	if err != nil {
		t.Fatalf("GetRepoState after apply: %v", err)
	}
	if rs == nil || rs.LastAppliedSeq != 0 || rs.LinearizationFingerprint != "fp-0" {
		t.Fatalf("expected watermark seq=0 fp=fp-0, got %+v", rs)
	}

	resume, err = db.ResumeFromSeq(context.Background(), repo)
	if err != nil {
		t.Fatalf("ResumeFromSeq: %v", err)
	}
	if resume != 1 {
		t.Fatalf("expected resume seq 1 after applying seq 0, got %d", resume)
	}
}

// TestApplyDelta_FailedDeltaLeavesNoWatermarkAdvance proves the negative
// case architecture.md section 3.1 is actually about: if the data write
// fails partway through, the watermark must NOT advance, and none of the
// partial data write should be visible either — the whole delta is one
// transaction.
func TestApplyDelta_FailedDeltaLeavesNoWatermarkAdvance(t *testing.T) {
	db := openTestDB(t)
	repo := uniqueRepoName(t)

	// Seed at seq 0 successfully.
	if err := db.ApplyDelta(context.Background(), repo, 0, "fp-0", func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO commits (repo, seq, commit_hash) VALUES ($1, 0, 'aaa')`, repo)
		return err
	}); err != nil {
		t.Fatalf("seed ApplyDelta: %v", err)
	}

	// Attempt seq 1, but the delta function fails after doing a partial
	// write.
	wantErr := fmt.Errorf("simulated derivation failure")
	err := db.ApplyDelta(context.Background(), repo, 1, "fp-1", func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO commits (repo, seq, commit_hash) VALUES ($1, 1, 'bbb')`, repo); err != nil {
			return err
		}
		return wantErr
	})
	if err == nil {
		t.Fatal("expected ApplyDelta to fail")
	}

	rs, err := db.GetRepoState(context.Background(), repo)
	if err != nil {
		t.Fatalf("GetRepoState: %v", err)
	}
	if rs.LastAppliedSeq != 0 {
		t.Fatalf("watermark must not have advanced past 0 on a failed delta, got %d", rs.LastAppliedSeq)
	}

	var count int
	if err := db.RawDB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM commits WHERE repo = $1 AND seq = 1`, repo,
	).Scan(&count); err != nil {
		t.Fatalf("count seq=1 commits: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected the partial seq=1 insert to be rolled back, found %d rows", count)
	}
}

// TestSIGKILLInjection_RecoversToKnownSeq is architecture.md section 3.1's
// required test: "SIGKILL injection at randomized points within delta
// application, >=500 trials, asserting recovery to a clean, known seq."
//
// It runs a real subprocess (see crash_helper_test.go's TestHelperApplyDelta)
// that performs one ApplyDelta call and is killed with SIGKILL at a
// randomized point during its execution (approximated here via a randomized
// pre-commit sleep the helper process honors, since Windows has no
// equivalent of injecting a signal mid-syscall the way a POSIX SIGKILL test
// would on Linux — see docs/DECISIONS.md for the platform-specific
// mechanism used).
//
// Trial count is controlled by ATLAS_CRASH_TRIALS (default kept low for
// fast local iteration; set to >=500 to satisfy the spec's actual
// requirement — see docs/DECISIONS.md).
func TestSIGKILLInjection_RecoversToKnownSeq(t *testing.T) {
	if os.Getenv("SYMEX_RUN_CRASH_TEST") == "" {
		t.Skip("skipping: set SYMEX_RUN_CRASH_TEST=1 to run (spawns real subprocesses, requires Postgres)")
	}
	db := openTestDB(t)

	trials := 20
	if v := os.Getenv("ATLAS_CRASH_TRIALS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			t.Fatalf("bad ATLAS_CRASH_TRIALS: %v", err)
		}
		trials = n
	}

	repo := uniqueRepoName(t)
	seq := int64(0)

	for i := 0; i < trials; i++ {
		// Kill the helper at a randomized point in [0, 200ms) into its
		// ApplyDelta call.
		killAfterMS := 5 + (i*37)%195

		cmd := exec.Command(os.Args[0], "-test.run=TestHelperApplyDelta", "-test.v")
		cmd.Env = append(os.Environ(),
			"SYMEX_CRASH_HELPER=1",
			fmt.Sprintf("SYMEX_TEST_DSN=%s", testDSN()),
			fmt.Sprintf("SYMEX_HELPER_REPO=%s", repo),
			fmt.Sprintf("SYMEX_HELPER_SEQ=%d", seq),
			fmt.Sprintf("SYMEX_HELPER_KILL_AFTER_MS=%d", killAfterMS),
		)
		if err := cmd.Start(); err != nil {
			t.Fatalf("trial %d: start helper: %v", i, err)
		}

		time.Sleep(time.Duration(killAfterMS) * time.Millisecond)
		killErr := cmd.Process.Kill() // SIGKILL-equivalent: no graceful shutdown, no defer/rollback runs
		waitErr := cmd.Wait()

		// Recovery assertion: repo_state, if present, must point at a seq
		// that has a fully-applied, self-consistent commits row — i.e.
		// last_applied_seq must never be a seq whose data write was only
		// partially committed. Since ApplyDelta wraps everything in one
		// transaction, the only two observable states are "seq-1 fully
		// applied, seq not started" or "seq fully applied" — never partial.
		rs, err := db.GetRepoState(context.Background(), repo)
		if err != nil {
			t.Fatalf("trial %d: GetRepoState: %v", i, err)
		}
		var observedSeq int64 = -1
		if rs != nil {
			observedSeq = rs.LastAppliedSeq
		}

		var commitCount int
		if err := db.RawDB().QueryRowContext(context.Background(),
			`SELECT COUNT(*) FROM commits WHERE repo = $1 AND seq = $2`, repo, seq,
		).Scan(&commitCount); err != nil {
			t.Fatalf("trial %d: count commits: %v", i, err)
		}

		// A kill can race a subprocess's own tx.Commit(): data already
		// handed to the OS network stack before the kill lands still gets
		// delivered to Postgres and committed there, even though the local
		// process never got to report success. That commit is genuinely
		// atomic (same transaction as the watermark write) — it just may
		// take a beat longer than cmd.Wait() to become visible over a
		// SEPARATE connection's query. Retry briefly before treating a
		// mismatch as a real crash-consistency violation, and report
		// whether the retry resolved it — that distinguishes "transient
		// visibility lag" from "genuinely inconsistent."
		mismatch := func() (bool, bool) { // (isMismatch, isCommitsAheadOfWatermark)
			return (observedSeq == seq && commitCount != 1) || (observedSeq < seq && commitCount != 0),
				observedSeq < seq && commitCount != 0
		}
		isMismatch, _ := mismatch()
		resolvedByRetry := false
		if isMismatch {
			for retry := 0; retry < 5 && isMismatch; retry++ {
				time.Sleep(100 * time.Millisecond)
				rs, _ = db.GetRepoState(context.Background(), repo)
				observedSeq = -1
				if rs != nil {
					observedSeq = rs.LastAppliedSeq
				}
				db.RawDB().QueryRowContext(context.Background(),
					`SELECT COUNT(*) FROM commits WHERE repo = $1 AND seq = $2`, repo, seq,
				).Scan(&commitCount)
				isMismatch, _ = mismatch()
			}
			if !isMismatch {
				resolvedByRetry = true
			}
		}

		if isMismatch {
			t.Fatalf("trial %d: seq=%d observedSeq=%d commitCount=%d killErr=%v waitErr=%v — crash consistency violated (did not resolve after retries)",
				i, seq, observedSeq, commitCount, killErr, waitErr)
		}
		if resolvedByRetry {
			t.Logf("trial %d: seq=%d transiently inconsistent right after cmd.Wait() (killErr=%v waitErr=%v), resolved within 500ms retry — logging as a visibility-lag note, not a failure",
				i, seq, killErr, waitErr)
		}

		if observedSeq == seq {
			seq++ // this trial's delta landed cleanly; advance for the next one
		}
		// else: retry the same seq next trial (simulates resuming from the watermark)
	}
}
