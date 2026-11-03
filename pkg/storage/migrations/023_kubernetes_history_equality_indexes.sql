-- versus:nontransactional
CREATE INDEX CONCURRENTLY IF NOT EXISTS kubernetes_history_issue_filter ON kubernetes_evidence_events(scope_key,namespace,kind,root_uid,rule,event_at DESC,event_id DESC) WHERE event_type IN('issue_opened','issue_observed','issue_resolved');
CREATE INDEX CONCURRENTLY IF NOT EXISTS kubernetes_history_occurrence_filter ON kubernetes_evidence_occurrence_read_versions(scope_key,namespace,kind,rule,severity,first_seen DESC,occurrence_id DESC,revision DESC) WHERE broken;
