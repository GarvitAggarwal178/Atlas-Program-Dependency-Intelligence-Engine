package store_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/yourorg/symex/internal/store"
)

func openTestDBWithDependencyVersions(t *testing.T) *store.DB {
	t.Helper()
	db := openTestDBWithFacts(t)
	if err := db.ApplySchemaV7(context.Background()); err != nil {
		t.Fatalf("apply schema v7: %v", err)
	}
	return db
}

// TestUpsertDependencyVersion_TracksBumpsAndNoOps is the MODULE-delta
// version of the interface_implementers test: a no-op update reports no
// change, a real version bump reports change and correctly maintains the
// interval (old closed, new opened).
func TestUpsertDependencyVersion_TracksBumpsAndNoOps(t *testing.T) {
	db := openTestDBWithDependencyVersions(t)
	repo := uniqueRepoName(t)
	const mod = "github.com/foo/bar"

	// First sighting: always "changed" (no prior record).
	err := db.ApplyDelta(context.Background(), repo, 0, "fp-0", func(ctx context.Context, tx *sql.Tx) error {
		changed, err := store.UpsertDependencyVersion(ctx, tx, repo, mod, "v1.2.3", 0)
		if err != nil {
			return err
		}
		if !changed {
			t.Fatal("expected the first sighting of a module to report changed=true")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed ApplyDelta: %v", err)
	}

	// No-op: same version again.
	err = db.ApplyDelta(context.Background(), repo, 1, "fp-1", func(ctx context.Context, tx *sql.Tx) error {
		changed, err := store.UpsertDependencyVersion(ctx, tx, repo, mod, "v1.2.3", 1)
		if err != nil {
			return err
		}
		if changed {
			t.Fatal("expected an unchanged version to report changed=false")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("no-op ApplyDelta: %v", err)
	}

	// Real bump.
	err = db.ApplyDelta(context.Background(), repo, 2, "fp-2", func(ctx context.Context, tx *sql.Tx) error {
		changed, err := store.UpsertDependencyVersion(ctx, tx, repo, mod, "v1.3.0", 2)
		if err != nil {
			return err
		}
		if !changed {
			t.Fatal("expected a real version bump to report changed=true")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("bump ApplyDelta: %v", err)
	}

	version, ok, err := store.GetLiveDependencyVersion(context.Background(), db.RawDB(), repo, mod)
	if err != nil {
		t.Fatalf("GetLiveDependencyVersion: %v", err)
	}
	if !ok || version != "v1.3.0" {
		t.Fatalf("expected live version v1.3.0, got ok=%v version=%q", ok, version)
	}

	// The OLD interval must be closed, not just superseded.
	var closedCount int
	if err := db.RawDB().QueryRowContext(context.Background(), `
		SELECT COUNT(*) FROM atlas.dependency_versions
		WHERE repo = $1 AND module_path = $2 AND version = 'v1.2.3' AND valid_to IS NOT NULL
	`, repo, mod).Scan(&closedCount); err != nil {
		t.Fatalf("count closed versions: %v", err)
	}
	if closedCount != 1 {
		t.Errorf("expected exactly 1 closed row for the old version v1.2.3, got %d", closedCount)
	}
}
