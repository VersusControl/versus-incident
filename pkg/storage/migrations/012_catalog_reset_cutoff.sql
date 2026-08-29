-- Make whole-catalog resets authoritative over immutable lower-scope learned
-- partitions without deleting their archive rows. The root records the reset
-- boundary, while each learned partition records its most recent persistence.

ALTER TABLE vs_patterns
    ADD COLUMN reset_at TIMESTAMPTZ;

ALTER TABLE vs_logs
    ADD COLUMN persisted_at TIMESTAMPTZ NOT NULL DEFAULT NOW();