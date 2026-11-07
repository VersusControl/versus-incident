-- versus:nontransactional
CREATE INDEX CONCURRENTLY IF NOT EXISTS kubernetes_occurrence_read_open_span ON kubernetes_evidence_occurrence_read_versions USING gist(tstzrange(first_seen,GREATEST(first_seen,COALESCE(closed_at,'infinity'::timestamptz)),'[]')) WHERE superseded_revision IS NULL;
CREATE INDEX CONCURRENTLY IF NOT EXISTS kubernetes_occurrence_read_visible_span ON kubernetes_evidence_occurrence_read_versions USING gist(tstzrange(LEAST(observed_at,superseded_at),superseded_at,'[)')) WHERE superseded_revision IS NOT NULL;
CREATE INDEX CONCURRENTLY IF NOT EXISTS kubernetes_occurrence_read_replaced ON kubernetes_evidence_occurrence_read_versions(scope_key,superseded_revision) WHERE superseded_revision IS NOT NULL;
