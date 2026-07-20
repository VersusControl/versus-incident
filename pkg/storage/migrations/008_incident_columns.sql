-- 008_incident_columns.sql — promote every IncidentRecord field to its own
-- typed column on vs_incidents.
--
-- The incident read/write path stores each property in a dedicated column
-- (no more whole-record JSON in `data`). This migration is additive and
-- idempotent: every ADD COLUMN uses IF NOT EXISTS and every index uses
-- IF NOT EXISTS, so a re-run is a no-op.
--
-- It intentionally does NOT backfill the new columns from `data` and does NOT
-- drop `data`. Those are one-time, potentially expensive operations an
-- operator runs during a maintenance window from the manual migration script
-- (scripts/postgres/migrate_incident_columns.sql). Until that runs, the
-- `data` column simply lingers, unused by the new code.

ALTER TABLE vs_incidents ADD COLUMN IF NOT EXISTS org_id              TEXT NOT NULL DEFAULT 'default';
ALTER TABLE vs_incidents ADD COLUMN IF NOT EXISTS team_id             TEXT;
ALTER TABLE vs_incidents ADD COLUMN IF NOT EXISTS title               TEXT;
ALTER TABLE vs_incidents ADD COLUMN IF NOT EXISTS source              TEXT;
ALTER TABLE vs_incidents ADD COLUMN IF NOT EXISTS service             TEXT;
ALTER TABLE vs_incidents ADD COLUMN IF NOT EXISTS origin              TEXT;
ALTER TABLE vs_incidents ADD COLUMN IF NOT EXISTS resolved            BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE vs_incidents ADD COLUMN IF NOT EXISTS channels_enabled    TEXT[];
ALTER TABLE vs_incidents ADD COLUMN IF NOT EXISTS channels_notified   TEXT[];
ALTER TABLE vs_incidents ADD COLUMN IF NOT EXISTS oncall_triggered    BOOLEAN;
ALTER TABLE vs_incidents ADD COLUMN IF NOT EXISTS oncall_error        TEXT;
ALTER TABLE vs_incidents ADD COLUMN IF NOT EXISTS notify_status       TEXT;
ALTER TABLE vs_incidents ADD COLUMN IF NOT EXISTS notify_error        TEXT;
ALTER TABLE vs_incidents ADD COLUMN IF NOT EXISTS resolved_at         TIMESTAMPTZ;
ALTER TABLE vs_incidents ADD COLUMN IF NOT EXISTS content             JSONB;
ALTER TABLE vs_incidents ADD COLUMN IF NOT EXISTS assigned_team_id    TEXT;
ALTER TABLE vs_incidents ADD COLUMN IF NOT EXISTS assigned_member_ids TEXT[];

-- The write path no longer populates `data`, so an insert must succeed without
-- it. Drop the NOT NULL constraint the initial schema (001) put on `data`.
ALTER TABLE vs_incidents ALTER COLUMN data DROP NOT NULL;

-- Indexes for the list/count/filter paths that now read the promoted columns.
-- idx_incidents_created_at (created by 001) already covers the unfiltered feed.
CREATE INDEX IF NOT EXISTS idx_incidents_origin_created_at
    ON vs_incidents (origin, created_at DESC);

CREATE INDEX IF NOT EXISTS idx_incidents_unresolved_created_at
    ON vs_incidents (created_at DESC) WHERE resolved = false;

CREATE INDEX IF NOT EXISTS idx_incidents_origin_unresolved_created_at
    ON vs_incidents (origin, created_at DESC) WHERE resolved = false;
