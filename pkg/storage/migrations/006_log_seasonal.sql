-- 006_log_seasonal.sql — Core, OSS.
-- Add the per-time-bucket baseline the seasonal spike detector needs: an array
-- of hour-of-day (24) or hour-of-week (168) EWMA buckets on vs_logs, mirroring
-- the enterprise vs_metrics / vs_traces `seasonal` JSONB exactly. With it a
-- pattern that is busy at 02:00 (a nightly batch job) is scored against the
-- 02:00 bucket rather than a blended all-day mean, so the batch burst is
-- normal-for-2am while the same rate at 14:00 still pages.
--
-- ADDITIVE, DEFAULT-SAFE: the column defaults to '[]' (no buckets), so every
-- existing vs_logs row reads back byte-identically and simply re-learns its
-- buckets on the next few ticks. There is NO backfill copy — "migration =
-- re-learn, no copy", the same precedent as vs_metrics / vs_traces.
--
-- The bucket array is bounded (24 or 168) and always read whole, so JSONB is
-- the right shape (a child table was rejected — see the design doc). Each entry
-- is a {mean, variance, count} EWMA, identical to the enterprise seasonal
-- payload, decoded through the SAME pkg/stats.EWMA type.
--
-- This file is ledger-tracked by RunSQLMigrations (versus_schema_migrations)
-- and therefore runs EXACTLY ONCE. The file backend has no vs_logs table and
-- never runs this migration; it re-learns the buckets in the whole-blob
-- patterns document instead.
--
-- SQLi-safety: static DDL, no interpolated value. All runtime DML against
-- vs_logs binds every value as a $N parameter and names the table as a Go
-- constant.

ALTER TABLE vs_logs
    ADD COLUMN seasonal JSONB NOT NULL DEFAULT '[]';
