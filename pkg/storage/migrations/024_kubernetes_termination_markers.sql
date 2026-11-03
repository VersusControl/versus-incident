-- versus:nontransactional
CREATE INDEX CONCURRENTLY IF NOT EXISTS kubernetes_history_termination_markers ON kubernetes_evidence_events(scope_key,event_at DESC,event_id DESC) WHERE event_type='crash' AND payload->>'type'='termination';
-- versus:optional-table
CREATE INDEX CONCURRENTLY IF NOT EXISTS kubernetes_partition_termination_markers ON kubernetes_evidence_events_partitioned(scope_key,event_at DESC,event_id DESC) WHERE event_type='crash' AND payload->>'type'='termination';
