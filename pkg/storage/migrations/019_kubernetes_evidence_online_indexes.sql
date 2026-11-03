-- versus:nontransactional
CREATE INDEX CONCURRENTLY IF NOT EXISTS kubernetes_evidence_current_type ON kubernetes_evidence_events(scope_key,event_type,event_at DESC,event_id DESC) WHERE superseded_seq IS NULL;
CREATE INDEX CONCURRENTLY IF NOT EXISTS kubernetes_evidence_current_namespace ON kubernetes_evidence_events(scope_key,namespace,event_at DESC,event_id DESC) WHERE superseded_seq IS NULL;
CREATE INDEX CONCURRENTLY IF NOT EXISTS kubernetes_evidence_current_root ON kubernetes_evidence_events(scope_key,root_uid,event_at DESC,event_id DESC) WHERE superseded_seq IS NULL;
CREATE INDEX CONCURRENTLY IF NOT EXISTS kubernetes_evidence_current_break ON kubernetes_evidence_events(scope_key,is_break,event_at DESC,event_id DESC) WHERE superseded_seq IS NULL;
CREATE INDEX CONCURRENTLY IF NOT EXISTS kubernetes_evidence_issue_root ON kubernetes_evidence_issues(scope_key,root_uid,last_seen DESC,occurrence_id DESC);
CREATE INDEX CONCURRENTLY IF NOT EXISTS kubernetes_evidence_issue_center ON kubernetes_evidence_issues(scope_key,root_uid,broke_at,occurrence_id);
CREATE INDEX CONCURRENTLY IF NOT EXISTS kubernetes_evidence_issue_namespace ON kubernetes_evidence_issues(scope_key,namespace,last_seen DESC,occurrence_id DESC);
CREATE INDEX CONCURRENTLY IF NOT EXISTS kubernetes_evidence_issue_latest ON kubernetes_evidence_issues(scope_key,last_seen DESC,occurrence_id DESC);
CREATE INDEX CONCURRENTLY IF NOT EXISTS kubernetes_evidence_issue_broken ON kubernetes_evidence_issues(scope_key,broke_at DESC,occurrence_id DESC) WHERE broken;
CREATE INDEX CONCURRENTLY IF NOT EXISTS kubernetes_evidence_issue_backfill ON kubernetes_evidence_issues(scope_key,occurrence_id) WHERE NOT model_ready;
CREATE INDEX CONCURRENTLY IF NOT EXISTS kubernetes_occurrence_read_time ON kubernetes_evidence_occurrence_read_versions(scope_key,first_seen DESC,occurrence_id DESC,revision DESC);
CREATE INDEX CONCURRENTLY IF NOT EXISTS kubernetes_occurrence_read_root ON kubernetes_evidence_occurrence_read_versions(scope_key,root_uid,first_seen DESC,occurrence_id DESC);
CREATE INDEX CONCURRENTLY IF NOT EXISTS kubernetes_occurrence_read_center ON kubernetes_evidence_occurrence_read_versions(scope_key,root_uid,broke_at,first_seen DESC,occurrence_id DESC);
CREATE INDEX CONCURRENTLY IF NOT EXISTS kubernetes_occurrence_read_namespace ON kubernetes_evidence_occurrence_read_versions(scope_key,namespace,first_seen DESC,occurrence_id DESC);
CREATE INDEX CONCURRENTLY IF NOT EXISTS kubernetes_occurrence_read_broken ON kubernetes_evidence_occurrence_read_versions(scope_key,first_seen DESC,occurrence_id DESC) WHERE broken;
CREATE INDEX CONCURRENTLY IF NOT EXISTS kubernetes_occurrence_read_id ON kubernetes_evidence_occurrence_read_versions(scope_key,occurrence_id,revision DESC);
CREATE INDEX CONCURRENTLY IF NOT EXISTS kubernetes_evidence_scope_binding ON kubernetes_evidence_scopes(org_id,cluster_id,scope_binding,updated_at DESC);
