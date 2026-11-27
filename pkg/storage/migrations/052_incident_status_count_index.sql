-- versus:nontransactional
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_incidents_org_created_status ON vs_incidents (org_id, created_at) INCLUDE (resolved, acked_at, origin);
