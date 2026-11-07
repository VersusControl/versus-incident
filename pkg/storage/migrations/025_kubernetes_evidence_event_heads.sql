SET LOCAL lock_timeout='2s';
-- One row per event: its current version. Saves compare with and supersede
-- only this row, so their cost does not grow with an event's history. Events
-- saved before this table existed get their head on their next save.
CREATE TABLE IF NOT EXISTS kubernetes_evidence_event_heads (
 scope_key TEXT NOT NULL REFERENCES kubernetes_evidence_scopes(scope_key) ON DELETE CASCADE,
 event_id TEXT NOT NULL,
 sequence BIGINT NOT NULL,
 event_at TIMESTAMPTZ NOT NULL,
 event_version TEXT NOT NULL,
 priority INTEGER NOT NULL,
 observed_at TIMESTAMPTZ NOT NULL,
 PRIMARY KEY(scope_key,event_id)
);
CREATE INDEX IF NOT EXISTS kubernetes_evidence_event_heads_time ON kubernetes_evidence_event_heads(event_at);
ALTER TABLE kubernetes_evidence_event_heads SET (fillfactor=80, autovacuum_vacuum_scale_factor=0.02);
