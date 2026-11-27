-- versus:nontransactional
CREATE INDEX CONCURRENTLY IF NOT EXISTS kubernetes_evidence_open_issue ON kubernetes_evidence_issues(scope_key,occurrence_id) WHERE closed_at IS NULL AND NOT observation_only;
