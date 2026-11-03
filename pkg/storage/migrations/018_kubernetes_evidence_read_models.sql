SET LOCAL lock_timeout = '2s';
ALTER TABLE kubernetes_evidence_events ADD COLUMN IF NOT EXISTS superseded_seq BIGINT;
ALTER TABLE kubernetes_evidence_scopes ADD COLUMN IF NOT EXISTS read_model_ready BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE kubernetes_evidence_scopes ADD COLUMN IF NOT EXISTS backfill_sequence BIGINT NOT NULL DEFAULT 0;
ALTER TABLE kubernetes_evidence_scopes ADD COLUMN IF NOT EXISTS backfill_occurrence TEXT NOT NULL DEFAULT '';
ALTER TABLE kubernetes_evidence_scopes ADD COLUMN IF NOT EXISTS backfill_version TEXT NOT NULL DEFAULT '';
ALTER TABLE kubernetes_evidence_scopes ADD COLUMN IF NOT EXISTS high_sequence BIGINT NOT NULL DEFAULT 0;
ALTER TABLE kubernetes_evidence_scopes ADD COLUMN IF NOT EXISTS event_counts JSONB NOT NULL DEFAULT '{}';
ALTER TABLE kubernetes_evidence_scopes ADD COLUMN IF NOT EXISTS scope_binding TEXT NOT NULL DEFAULT '';
ALTER TABLE kubernetes_evidence_scopes ADD COLUMN IF NOT EXISTS updated_at TIMESTAMPTZ NOT NULL DEFAULT 'epoch';
ALTER TABLE kubernetes_evidence_issues ADD COLUMN IF NOT EXISTS root_uid TEXT NOT NULL DEFAULT '';
ALTER TABLE kubernetes_evidence_issues ADD COLUMN IF NOT EXISTS namespace TEXT NOT NULL DEFAULT '';
ALTER TABLE kubernetes_evidence_issues ADD COLUMN IF NOT EXISTS kind TEXT NOT NULL DEFAULT '';
ALTER TABLE kubernetes_evidence_issues ADD COLUMN IF NOT EXISTS rule TEXT NOT NULL DEFAULT '';
ALTER TABLE kubernetes_evidence_issues ADD COLUMN IF NOT EXISTS severity TEXT NOT NULL DEFAULT '';
ALTER TABLE kubernetes_evidence_issues ADD COLUMN IF NOT EXISTS authorization_kind TEXT NOT NULL DEFAULT '';
ALTER TABLE kubernetes_evidence_issues ADD COLUMN IF NOT EXISTS broken BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE kubernetes_evidence_issues ADD COLUMN IF NOT EXISTS broke_at TIMESTAMPTZ;
ALTER TABLE kubernetes_evidence_issues ADD COLUMN IF NOT EXISTS last_observed_at TIMESTAMPTZ;
ALTER TABLE kubernetes_evidence_issues ADD COLUMN IF NOT EXISTS model_ready BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE kubernetes_evidence_issues ADD COLUMN IF NOT EXISTS observation_only BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE kubernetes_evidence_issues SET (fillfactor=80, autovacuum_vacuum_scale_factor=0.02);
ALTER TABLE kubernetes_evidence_events SET (autovacuum_vacuum_scale_factor=0.02);
ALTER TABLE kubernetes_evidence_occurrence_observations SET (autovacuum_vacuum_scale_factor=0.02);

CREATE TABLE kubernetes_evidence_occurrence_read_versions (
 scope_key TEXT NOT NULL REFERENCES kubernetes_evidence_scopes(scope_key) ON DELETE CASCADE,
 occurrence_id TEXT NOT NULL,
 payload_version TEXT NOT NULL,
 revision BIGINT NOT NULL,
 superseded_revision BIGINT,
 observed_at TIMESTAMPTZ NOT NULL,
 superseded_at TIMESTAMPTZ,
 first_seen TIMESTAMPTZ NOT NULL,
 closed_at TIMESTAMPTZ,
 root_uid TEXT NOT NULL,
 namespace TEXT NOT NULL,
 kind TEXT NOT NULL,
 rule TEXT NOT NULL,
 severity TEXT NOT NULL,
 authorization_kind TEXT NOT NULL,
 broken BOOLEAN NOT NULL,
 observation_only BOOLEAN NOT NULL,
 broke_at TIMESTAMPTZ,
 payload JSONB NOT NULL,
 PRIMARY KEY(scope_key,payload_version),
 UNIQUE(scope_key,occurrence_id,revision)
);
