package store

import (
	"context"
	"database/sql"
	"fmt"
)

// SchemaV9 adds file_versions: every parsed file's structural hash,
// tracked independently of whether that file produced any facts.
//
// Found via a real test failure, not designed up front: LiveFileHashes
// (derived from atlas.derivations, which is fact-anchored) is blind to any
// file that produces zero facts — e.g. a file containing only a function
// with an empty body and no calls. Such a file has no derivation row at
// all, so on every subsequent commit it looks "new" (not in the old hash
// map) and gets treated as changed, even when byte-for-byte identical to
// the previous commit. Not a correctness bug (there's nothing to
// open/close for a file with zero facts either way) but a real precision
// bug in the section 2.3 cache-hit-rate diagnostic and in changedFiles
// more generally. file_versions tracks every file the indexer has ever
// seen, independent of fact count, fixing this at the source.
const SchemaV9 = `
CREATE TABLE IF NOT EXISTS atlas.file_versions (
    repo TEXT NOT NULL,
    file_path TEXT NOT NULL,
    content_hash TEXT NOT NULL,
    valid_from BIGINT NOT NULL,
    valid_to BIGINT,
    CHECK (valid_to IS NULL OR valid_to > valid_from)
);

CREATE UNIQUE INDEX IF NOT EXISTS file_versions_live_uniq
    ON atlas.file_versions (repo, file_path)
    WHERE valid_to IS NULL;
`

// ApplySchemaV9 creates atlas.file_versions.
func (s *DB) ApplySchemaV9(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, SchemaV9); err != nil {
		return fmt.Errorf("apply schema v9: %w", err)
	}
	return nil
}

// LiveFileVersions returns the recorded hash for every file currently
// tracked for repo, regardless of whether that file has ever produced a
// fact. This is the correct source for "what changed since last commit" —
// see the SchemaV9 doc comment for why LiveFileHashes (derivation-anchored)
// isn't.
func LiveFileVersions(ctx context.Context, q queryer, repo string) (map[string]string, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT file_path, content_hash FROM atlas.file_versions
		WHERE repo = $1 AND valid_to IS NULL
	`, repo)
	if err != nil {
		return nil, fmt.Errorf("live file versions: %w", err)
	}
	defer rows.Close()

	result := make(map[string]string)
	for rows.Next() {
		var f, h string
		if err := rows.Scan(&f, &h); err != nil {
			return nil, fmt.Errorf("scan file version: %w", err)
		}
		result[f] = h
	}
	return result, rows.Err()
}

// UpdateFileVersions applies newHashes (the full current file->hash map)
// against what's currently live, closing entries for files that changed
// or disappeared and opening entries for files that are new or changed.
// Unchanged files are left completely untouched — same selectivity
// principle as everything else. Must be called with the *sql.Tx from an
// in-flight ApplyDelta call.
func UpdateFileVersions(ctx context.Context, tx *sql.Tx, repo string, seq int64, newHashes map[string]string) error {
	old, err := LiveFileVersions(ctx, tx, repo)
	if err != nil {
		return err
	}

	for f, newHash := range newHashes {
		if oldHash, ok := old[f]; ok && oldHash == newHash {
			continue // unchanged, leave the row alone entirely
		}
		if _, ok := old[f]; ok {
			if _, err := tx.ExecContext(ctx, `
				UPDATE atlas.file_versions SET valid_to = $1
				WHERE repo = $2 AND file_path = $3 AND valid_to IS NULL
			`, seq, repo, f); err != nil {
				return fmt.Errorf("close file_versions for %s: %w", f, err)
			}
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO atlas.file_versions (repo, file_path, content_hash, valid_from, valid_to)
			VALUES ($1, $2, $3, $4, NULL)
		`, repo, f, newHash, seq); err != nil {
			return fmt.Errorf("open file_versions for %s: %w", f, err)
		}
	}

	for f := range old {
		if _, stillExists := newHashes[f]; stillExists {
			continue
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE atlas.file_versions SET valid_to = $1
			WHERE repo = $2 AND file_path = $3 AND valid_to IS NULL
		`, seq, repo, f); err != nil {
			return fmt.Errorf("close file_versions for removed file %s: %w", f, err)
		}
	}

	return nil
}
