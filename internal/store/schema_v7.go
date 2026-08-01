package store

import (
	"context"
	"database/sql"
	"fmt"
)

// SchemaV7 adds dependency_versions (architecture.md section 5/section 7):
// which version of each required module was in play, as an interval-based
// relation, same pattern as everything else.
const SchemaV7 = `
CREATE TABLE IF NOT EXISTS atlas.dependency_versions (
    repo TEXT NOT NULL,
    module_path TEXT NOT NULL,
    version TEXT NOT NULL,
    valid_from BIGINT NOT NULL,
    valid_to BIGINT,
    CHECK (valid_to IS NULL OR valid_to > valid_from)
);

CREATE UNIQUE INDEX IF NOT EXISTS dependency_versions_live_uniq
    ON atlas.dependency_versions (repo, module_path)
    WHERE valid_to IS NULL;
`

// DependencyVersion is a single row in atlas.dependency_versions.
type DependencyVersion struct {
	Repo       string
	ModulePath string
	Version    string
	ValidFrom  int64
	ValidTo    *int64
}

// ApplySchemaV7 creates atlas.dependency_versions.
func (s *DB) ApplySchemaV7(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, SchemaV7); err != nil {
		return fmt.Errorf("apply schema v7: %w", err)
	}
	return nil
}

// GetLiveDependencyVersion returns the currently-recorded version for
// (repo, modulePath), or ok=false if the module has never been seen.
func GetLiveDependencyVersion(ctx context.Context, q queryer, repo, modulePath string) (version string, ok bool, err error) {
	err = q.QueryRowContext(ctx, `
		SELECT version FROM atlas.dependency_versions
		WHERE repo = $1 AND module_path = $2 AND valid_to IS NULL
	`, repo, modulePath).Scan(&version)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("get live dependency version for %s: %w", modulePath, err)
	}
	return version, true, nil
}

// UpsertDependencyVersion maintains atlas.dependency_versions the same way
// UpsertInterfaceImplementers maintains atlas.interface_implementers: if
// the recorded live version already matches newVersion, do nothing
// (changed=false). Otherwise close the old interval (if any) and open a
// new one at seq, returning changed=true so the caller knows to run
// StaleLiveFacts(MODULE, modulePath, newVersion) to find facts that need
// withdrawing.
//
// Must be called with the *sql.Tx from an in-flight ApplyDelta call.
func UpsertDependencyVersion(ctx context.Context, tx *sql.Tx, repo, modulePath, newVersion string, seq int64) (changed bool, err error) {
	oldVersion, ok, err := GetLiveDependencyVersion(ctx, tx, repo, modulePath)
	if err != nil {
		return false, err
	}
	if ok && oldVersion == newVersion {
		return false, nil
	}

	if ok {
		if _, err := tx.ExecContext(ctx, `
			UPDATE atlas.dependency_versions SET valid_to = $1
			WHERE repo = $2 AND module_path = $3 AND valid_to IS NULL
		`, seq, repo, modulePath); err != nil {
			return false, fmt.Errorf("close dependency_versions for %s: %w", modulePath, err)
		}
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO atlas.dependency_versions (repo, module_path, version, valid_from, valid_to)
		VALUES ($1, $2, $3, $4, NULL)
	`, repo, modulePath, newVersion, seq); err != nil {
		return false, fmt.Errorf("open dependency_versions for %s: %w", modulePath, err)
	}

	return true, nil
}
