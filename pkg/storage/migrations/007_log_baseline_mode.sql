-- 007_log_baseline_mode.sql — Core, OSS.
-- Add the two remaining explicit spike-baseline columns to vs_logs so the
-- operator can flip WHICH learned baseline the spike z-score is scored against
-- with no re-learn:
--   - baseline_avg: the cumulative arithmetic mean of the per-second match
--     rate (total ÷ number of folded ticks). It is the CENTER the "average"
--     baseline mode scores against; that mode reuses the existing
--     baseline_variance as its spread, so there is one dispersion source.
--   - spike_baseline_mode: the per-pattern override of the baseline mode
--     ('' = inherit the agent.catalog.spike_baseline_mode config default;
--     otherwise 'default' | 'average' | 'time_of_day').
--
-- All three baselines (global frequency/variance, the arithmetic average, and
-- the 24 hour-of-day seasonal buckets) are now ALWAYS computed and stored, so
-- switching modes never needs a re-learn.
--
-- ADDITIVE, DEFAULT-SAFE: baseline_avg defaults to 0 and spike_baseline_mode
-- defaults to '', so every existing vs_logs row reads back byte-identically —
-- the empty mode simply inherits the config default and the average re-learns
-- within a learning window. There is NO backfill copy: this rides the same
-- "migration = re-learn, no copy" pattern already used for the earlier
-- baseline_variance and seasonal signal-table columns.
--
-- This file is ledger-tracked by RunSQLMigrations (versus_schema_migrations)
-- and therefore runs EXACTLY ONCE. The file backend has no vs_logs table and
-- never runs this migration; it re-learns the average in the whole-blob
-- patterns document instead.
--
-- SQLi-safety: static DDL, no interpolated value. All runtime DML against
-- vs_logs binds every value as a $N parameter and names the table as a Go
-- constant.

ALTER TABLE vs_logs
    ADD COLUMN baseline_avg DOUBLE PRECISION NOT NULL DEFAULT 0;

ALTER TABLE vs_logs
    ADD COLUMN spike_baseline_mode TEXT NOT NULL DEFAULT '';
