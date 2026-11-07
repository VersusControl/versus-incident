-- versus:nontransactional
-- versus:partitioned-table
CREATE INDEX CONCURRENTLY IF NOT EXISTS kubernetes_history_object_time ON kubernetes_evidence_events(scope_key,namespace,kind,resource_name,event_at DESC,event_id DESC);
-- versus:partitioned-table
CREATE INDEX CONCURRENTLY IF NOT EXISTS kubernetes_partition_object_time ON kubernetes_evidence_events_partitioned(scope_key,namespace,kind,resource_name,event_at DESC,event_id DESC)
