-- versus:nontransactional
CREATE INDEX CONCURRENTLY IF NOT EXISTS kubernetes_history_gap_window ON kubernetes_evidence_events(scope_key,gap_to DESC,event_id DESC) WHERE event_type='coverage_gap';
