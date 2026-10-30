ALTER TABLE kubernetes_evidence_issues ADD COLUMN IF NOT EXISTS payload_version TEXT NOT NULL DEFAULT '';

CREATE TABLE IF NOT EXISTS kubernetes_evidence_occurrence_versions (
 scope_key TEXT NOT NULL REFERENCES kubernetes_evidence_scopes(scope_key) ON DELETE CASCADE,
 payload_version TEXT NOT NULL,
 occurrence_id TEXT NOT NULL,
 payload JSONB NOT NULL,
 PRIMARY KEY(scope_key,payload_version)
);
CREATE TABLE IF NOT EXISTS kubernetes_evidence_occurrence_observations (
 scope_key TEXT NOT NULL REFERENCES kubernetes_evidence_scopes(scope_key) ON DELETE CASCADE,
 occurrence_id TEXT NOT NULL,
 revision BIGINT NOT NULL,
 observed_at TIMESTAMPTZ NOT NULL,
 last_seen TIMESTAMPTZ NOT NULL,
 payload_version TEXT NOT NULL,
 PRIMARY KEY(scope_key,occurrence_id,revision),
 FOREIGN KEY(scope_key,payload_version) REFERENCES kubernetes_evidence_occurrence_versions(scope_key,payload_version)
);
CREATE INDEX IF NOT EXISTS kubernetes_occurrence_snapshot ON kubernetes_evidence_occurrence_observations(scope_key,occurrence_id,revision DESC);
CREATE INDEX IF NOT EXISTS kubernetes_occurrence_window ON kubernetes_evidence_occurrence_observations(scope_key,observed_at DESC,revision DESC);

