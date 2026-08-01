package store

import (
	"context"
	"fmt"
)

// Audit infrastructure for measuring v2's ACTUAL retraction/reinsertion
// volume without touching a single line of v2's frozen Go source
// (internal/incremental, this package's own Schema constant).
// architecture.md section 10.1 needs "facts withdrawn" for v2 — but v2's
// harness only proves the NET result matches a full rebuild (that's what
// the differential harness already checks); it says nothing about how
// much internal churn (delete-then-reinsert-the-identical-thing) it took
// to get there, which is exactly what "over-invalidation" means. Net
// before/after diffing can't see that churn — only observing the actual
// DELETE/INSERT statements v2 issues can.
//
// A Postgres trigger on v2's own "facts" table (public schema) is a clean
// way to observe that: it's a database-level addition, not a code change
// to anything tagged at v2-frozen. Every INSERT/DELETE v2's engine issues
// against facts gets logged, regardless of which Go code issued it.
const AuditSchema = `
CREATE TABLE IF NOT EXISTS public.facts_audit (
    audit_id      BIGSERIAL PRIMARY KEY,
    op             TEXT NOT NULL,   -- 'INSERT' | 'DELETE'
    repo           TEXT NOT NULL,
    commit_hash    TEXT NOT NULL,
    source_symbol  TEXT NOT NULL,
    target_symbol  TEXT NOT NULL,
    provenance     TEXT NOT NULL,
    logged_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE OR REPLACE FUNCTION public.facts_audit_fn() RETURNS TRIGGER AS $$
BEGIN
    IF (TG_OP = 'DELETE') THEN
        INSERT INTO public.facts_audit (op, repo, commit_hash, source_symbol, target_symbol, provenance)
        VALUES ('DELETE', OLD.repo, OLD.commit_hash, OLD.source_symbol, OLD.target_symbol, OLD.provenance);
        RETURN OLD;
    ELSIF (TG_OP = 'INSERT') THEN
        INSERT INTO public.facts_audit (op, repo, commit_hash, source_symbol, target_symbol, provenance)
        VALUES ('INSERT', NEW.repo, NEW.commit_hash, NEW.source_symbol, NEW.target_symbol, NEW.provenance);
        RETURN NEW;
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS facts_audit_trigger ON public.facts;
CREATE TRIGGER facts_audit_trigger
    AFTER INSERT OR DELETE ON public.facts
    FOR EACH ROW EXECUTE FUNCTION public.facts_audit_fn();
`

// ApplyAuditSchema installs the audit table/trigger on v2's facts table.
func (s *DB) ApplyAuditSchema(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, AuditSchema); err != nil {
		return fmt.Errorf("apply audit schema: %w", err)
	}
	return nil
}

// RemoveAuditTrigger detaches the trigger (leaves the log table intact) —
// for tests/measurements that want to stop observing without dropping
// history.
func (s *DB) RemoveAuditTrigger(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DROP TRIGGER IF EXISTS facts_audit_trigger ON public.facts;`)
	if err != nil {
		return fmt.Errorf("remove audit trigger: %w", err)
	}
	return nil
}

// AuditEvent is a single logged INSERT/DELETE against public.facts.
type AuditEvent struct {
	Op           string // "INSERT" | "DELETE"
	Repo         string
	CommitHash   string
	SourceSymbol string
	TargetSymbol string
	Provenance   string
}

// ClearAuditLog truncates the audit log for repo — call between measured
// commits/scenarios to isolate what each one specifically triggered.
func (s *DB) ClearAuditLog(ctx context.Context, repo string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM public.facts_audit WHERE repo = $1`, repo)
	if err != nil {
		return fmt.Errorf("clear audit log: %w", err)
	}
	return nil
}

// AuditLog returns every logged event for repo, in the order they were
// issued.
func (s *DB) AuditLog(ctx context.Context, repo string) ([]AuditEvent, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT op, repo, commit_hash, source_symbol, target_symbol, provenance
		FROM public.facts_audit WHERE repo = $1 ORDER BY audit_id ASC
	`, repo)
	if err != nil {
		return nil, fmt.Errorf("query audit log: %w", err)
	}
	defer rows.Close()

	var result []AuditEvent
	for rows.Next() {
		var e AuditEvent
		if err := rows.Scan(&e.Op, &e.Repo, &e.CommitHash, &e.SourceSymbol, &e.TargetSymbol, &e.Provenance); err != nil {
			return nil, fmt.Errorf("scan audit event: %w", err)
		}
		result = append(result, e)
	}
	return result, rows.Err()
}
