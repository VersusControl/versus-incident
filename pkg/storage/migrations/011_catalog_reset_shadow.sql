-- Distinguish whole-catalog reset tombstones from deliberate operator deletes.
-- Learner persistence may revive only reset shadows when an entity is observed
-- again; explicit deletes remain suppressed across restarts.

ALTER TABLE vs_patterns
    ADD COLUMN reset_shadow BOOLEAN NOT NULL DEFAULT FALSE;

ALTER TABLE vs_services
    ADD COLUMN reset_shadow BOOLEAN NOT NULL DEFAULT FALSE;