-- migrate_incident_columns.sql — one-time operator migration.
--
-- As of the release that promoted every incident property to its own column,
-- the Postgres backend stores each IncidentRecord field in a dedicated column
-- on vs_incidents and no longer reads or writes the legacy `data` JSONB blob.
--
-- New deployments need nothing: the embedded migration (008_incident_columns)
-- adds the columns automatically on boot, and every new incident is written to
-- them. This script is ONLY for operators who have PRE-UPGRADE incident
-- history still living in the `data` column and want to:
--
--   1. move that history into the new columns so old incidents render, and
--   2. drop the now-unused `data` column to reclaim its storage and its GIN
--      index (created by migration 002).
--
-- Run it ONCE, during a maintenance window, against the same database in your
-- POSTGRES_DSN. It is wrapped in a transaction: the whole thing commits or
-- nothing does. The final DROP COLUMN is naturally one-shot — a second run
-- fails at the DROP because the column is already gone, which is the intended
-- signal that the migration is complete.
--
-- The `file` storage backend needs NOTHING: it already serializes every field
-- of the record to disk. This script is Postgres-only.
--
-- If you do NOT run this script, nothing breaks: the new code never touches
-- `data`, so the column simply lingers unused — but pre-upgrade incidents will
-- render empty (their fields live only in `data`) until you migrate them.

BEGIN;

UPDATE vs_incidents SET
    -- Scalars: extract as text; NULL when the key is absent.
    org_id   = COALESCE(NULLIF(data->>'org_id', ''), 'default'),
    team_id  = data->>'team_id',
    title    = data->>'title',
    source   = data->>'source',
    service  = data->>'service',

    -- Effective origin: the explicit value when present, otherwise derived
    -- from source exactly as IncidentRecord.EffectiveOrigin does — an "agent"
    -- or "agent:<...>" source is AI-detect, everything else is inbound webhook.
    origin = COALESCE(
        NULLIF(data->>'origin', ''),
        CASE
            WHEN data->>'source' = 'agent'
              OR data->>'source' LIKE 'agent:%' THEN 'ai_detect'
            ELSE 'webhook'
        END
    ),

    -- Booleans: cast from text; default false for resolved.
    resolved         = COALESCE((data->>'resolved')::boolean, false),
    oncall_triggered = (data->>'oncall_triggered')::boolean,

    oncall_error  = data->>'oncall_error',
    notify_status = data->>'notify_status',
    notify_error  = data->>'notify_error',

    -- Timestamp: cast from text; NULL when absent.
    resolved_at = (data->>'resolved_at')::timestamptz,

    -- String arrays: expand a JSON array into a TEXT[]; NULL when the key is
    -- missing or is not a JSON array (guards a null/scalar value).
    channels_enabled = CASE
        WHEN jsonb_typeof(data->'channels_enabled') = 'array'
        THEN ARRAY(SELECT jsonb_array_elements_text(data->'channels_enabled'))
        ELSE NULL
    END,
    channels_notified = CASE
        WHEN jsonb_typeof(data->'channels_notified') = 'array'
        THEN ARRAY(SELECT jsonb_array_elements_text(data->'channels_notified'))
        ELSE NULL
    END,
    assigned_member_ids = CASE
        WHEN jsonb_typeof(data->'assigned_member_ids') = 'array'
        THEN ARRAY(SELECT jsonb_array_elements_text(data->'assigned_member_ids'))
        ELSE NULL
    END,

    -- Arbitrary content map: keep it as JSONB.
    content = data->'content',

    assigned_team_id = data->>'assigned_team_id'
WHERE data IS NOT NULL;

-- Reclaim the legacy column (and its GIN index from migration 002). This is
-- the point of no return; the backfill above must run first.
ALTER TABLE vs_incidents DROP COLUMN data;

COMMIT;
