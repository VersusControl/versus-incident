CREATE TABLE IF NOT EXISTS kubernetes_evidence_scopes (
 scope_key TEXT PRIMARY KEY,
 org_id TEXT NOT NULL,
 cluster_id TEXT NOT NULL,
 cluster_uid TEXT NOT NULL,
 checkpoint JSONB NOT NULL DEFAULT '{}'
);
CREATE TABLE IF NOT EXISTS kubernetes_evidence_captures (
 scope_key TEXT NOT NULL REFERENCES kubernetes_evidence_scopes(scope_key) ON DELETE CASCADE,
 capture_id TEXT NOT NULL,
 observed_at TIMESTAMPTZ NOT NULL,
 PRIMARY KEY (scope_key, capture_id)
);
CREATE TABLE IF NOT EXISTS kubernetes_evidence_events (
 sequence BIGSERIAL PRIMARY KEY,
 scope_key TEXT NOT NULL REFERENCES kubernetes_evidence_scopes(scope_key) ON DELETE CASCADE,
 event_id TEXT NOT NULL,
 event_version TEXT NOT NULL,
 priority INTEGER NOT NULL DEFAULT 0,
 event_type TEXT NOT NULL,
 is_break BOOLEAN NOT NULL DEFAULT FALSE,
 event_at TIMESTAMPTZ NOT NULL,
 observed_at TIMESTAMPTZ NOT NULL,
 gap_from TIMESTAMPTZ,
 gap_to TIMESTAMPTZ,
 namespace TEXT NOT NULL DEFAULT '',
 resource_name TEXT NOT NULL DEFAULT '',
 kind TEXT NOT NULL DEFAULT '',
 authorization_kind TEXT NOT NULL DEFAULT '',
 resource_uid TEXT NOT NULL DEFAULT '',
 root_uid TEXT NOT NULL DEFAULT '',
 pod_uid TEXT NOT NULL DEFAULT '',
 rule TEXT NOT NULL DEFAULT '',
 epoch TEXT NOT NULL,
 payload JSONB NOT NULL,
 UNIQUE (scope_key, event_id, event_version)
);
CREATE INDEX IF NOT EXISTS kubernetes_evidence_time ON kubernetes_evidence_events(scope_key, event_at DESC, event_id DESC);
CREATE INDEX IF NOT EXISTS kubernetes_evidence_sequence ON kubernetes_evidence_events(scope_key, sequence DESC);
CREATE INDEX IF NOT EXISTS kubernetes_evidence_namespace_time ON kubernetes_evidence_events(scope_key, namespace, event_at DESC, event_id DESC);
CREATE INDEX IF NOT EXISTS kubernetes_evidence_resource_time ON kubernetes_evidence_events(scope_key, resource_uid, event_at DESC, event_id DESC);
CREATE INDEX IF NOT EXISTS kubernetes_evidence_root_time ON kubernetes_evidence_events(scope_key, root_uid, event_at DESC, event_id DESC);
CREATE INDEX IF NOT EXISTS kubernetes_evidence_pod_time ON kubernetes_evidence_events(scope_key, pod_uid, event_at DESC, event_id DESC);
CREATE INDEX IF NOT EXISTS kubernetes_evidence_rule_time ON kubernetes_evidence_events(scope_key, rule, event_at DESC, event_id DESC);
CREATE INDEX IF NOT EXISTS kubernetes_evidence_type_time ON kubernetes_evidence_events(scope_key, event_type, event_at DESC, event_id DESC);
CREATE TABLE IF NOT EXISTS kubernetes_evidence_issues (
 scope_key TEXT NOT NULL REFERENCES kubernetes_evidence_scopes(scope_key) ON DELETE CASCADE,
 occurrence_id TEXT NOT NULL,
 issue_key TEXT NOT NULL,
 first_seen TIMESTAMPTZ NOT NULL,
 last_seen TIMESTAMPTZ NOT NULL,
 closed_at TIMESTAMPTZ,
 payload JSONB NOT NULL,
 PRIMARY KEY (scope_key, occurrence_id)
);
CREATE UNIQUE INDEX IF NOT EXISTS kubernetes_evidence_active_issue ON kubernetes_evidence_issues(scope_key, issue_key) WHERE closed_at IS NULL;
CREATE INDEX IF NOT EXISTS kubernetes_evidence_issue_time ON kubernetes_evidence_issues(scope_key, first_seen DESC, occurrence_id DESC);
CREATE INDEX IF NOT EXISTS kubernetes_evidence_observation_time ON kubernetes_evidence_events(scope_key,event_type,observed_at DESC,event_id DESC);
CREATE INDEX IF NOT EXISTS kubernetes_evidence_name_time ON kubernetes_evidence_events(scope_key,namespace,kind,resource_name,event_at DESC,event_id DESC);
CREATE INDEX IF NOT EXISTS kubernetes_evidence_gap_from ON kubernetes_evidence_events(scope_key,gap_from) WHERE event_type='coverage_gap';
CREATE INDEX IF NOT EXISTS kubernetes_evidence_gap_to ON kubernetes_evidence_events(scope_key,gap_to) WHERE event_type='coverage_gap';
CREATE INDEX IF NOT EXISTS kubernetes_evidence_event_versions ON kubernetes_evidence_events(scope_key,event_id,sequence DESC);
CREATE INDEX IF NOT EXISTS kubernetes_evidence_priority ON kubernetes_evidence_events(scope_key,event_id,priority DESC,observed_at DESC);
