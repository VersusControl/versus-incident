SET LOCAL lock_timeout='2s';
-- A scope whose current journal rows all have heads never needs to search its
-- journal for an event's current version. heads_backfill is the journal
-- sequence a background pass gives heads from; NULL means the scope's heads
-- are complete. Every scope starts incomplete, including a scope that a
-- writer without heads creates later; only a writer that maintains heads
-- marks a scope it creates as complete.
ALTER TABLE kubernetes_evidence_scopes ADD COLUMN IF NOT EXISTS heads_backfill BIGINT DEFAULT 0;
