package store_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/yourorg/symex/internal/store"
)

// TestHelperApplyDelta is not a real test — it is a subprocess entry point
// spawned by TestSIGKILLInjection_RecoversToKnownSeq (crash_test.go) via
// `go test -run TestHelperApplyDelta`, with SYMEX_CRASH_HELPER=1 set. It
// performs exactly one ApplyDelta call whose delta function sleeps for a
// fixed duration, so the parent process can kill it (TerminateProcess on
// Windows, SIGKILL on POSIX — both are abrupt, no-cleanup termination) at a
// randomized point before, during, or after that call.
//
// When SYMEX_CRASH_HELPER is unset, this is a no-op so it never runs as
// part of the normal `go test ./...` suite.
func TestHelperApplyDelta(t *testing.T) {
	if os.Getenv("SYMEX_CRASH_HELPER") != "1" {
		t.Skip("not running as crash-injection helper")
	}

	dsn := os.Getenv("SYMEX_TEST_DSN")
	repo := os.Getenv("SYMEX_HELPER_REPO")
	seqStr := os.Getenv("SYMEX_HELPER_SEQ")
	seq, err := strconv.ParseInt(seqStr, 10, 64)
	if err != nil {
		fmt.Fprintf(os.Stderr, "helper: bad seq %q: %v\n", seqStr, err)
		os.Exit(2)
	}

	db, err := store.Open(dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "helper: open: %v\n", err)
		os.Exit(2)
	}
	if err := db.ApplySchemaV3(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "helper: apply schema: %v\n", err)
		os.Exit(2)
	}

	// Fixed hold time inside the transaction, long enough that the parent's
	// randomized kill delay (5-200ms) sometimes lands before, during, and
	// after it — giving the required spread of injection points across
	// trials rather than always killing at the same phase.
	const holdMS = 80

	err = db.ApplyDelta(context.Background(), repo, seq, fmt.Sprintf("fp-%d", seq),
		func(ctx context.Context, tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO commits (repo, seq, commit_hash) VALUES ($1, $2, $3)`,
				repo, seq, fmt.Sprintf("sha-%d", seq),
			); err != nil {
				return err
			}
			time.Sleep(holdMS * time.Millisecond)
			return nil
		},
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "helper: apply delta: %v\n", err)
		os.Exit(1)
	}
	os.Exit(0)
}
