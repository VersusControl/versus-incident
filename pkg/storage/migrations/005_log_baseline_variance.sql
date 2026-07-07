-- 005_log_baseline_variance.sql — Core, OSS.
-- Add the dispersion the spike detector needs: an EWMA variance folded
-- alongside the existing baseline_frequency mean on vs_logs. With it the
-- detector can ask "how many standard deviations above normal is this tick?"
-- (a self-scaling z-score) instead of the volume-blind mean × multiplier bar,
-- and the fold can hold out a burst tick so one spike can't drag the baseline.
--
-- ADDITIVE, DEFAULT-SAFE: the column defaults to 0, so every existing vs_logs
-- row reads back byte-identically and simply re-learns its variance within a
-- learning window (0 variance → the z-score leans on the spread floor / the
-- absolute ceiling until it warms). There is NO backfill copy — this follows
-- the "migration = re-learn, no copy" precedent already set for
-- vs_metrics / vs_traces.
--
-- This file is ledger-tracked by RunSQLMigrations (versus_schema_migrations)
-- and therefore runs EXACTLY ONCE. The file backend has no vs_logs table and
-- never runs this migration; it re-learns the variance in the whole-blob
-- patterns document instead.
--
-- SQLi-safety: static DDL, no interpolated value. All runtime DML against
-- vs_logs binds every value as a $N parameter and names the table as a Go
-- constant.

ALTER TABLE vs_logs
    ADD COLUMN baseline_variance DOUBLE PRECISION NOT NULL DEFAULT 0;
