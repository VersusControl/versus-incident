-- 004_signal_kind_fk.sql — signal-kind FK hardening (Core, OSS).
-- Fold `kind` into the child→root foreign key so a child signal row can NEVER
-- attach to a wrong-kind vs_patterns root.
--
-- BEFORE: vs_patterns PK is (org_id, id) with `kind` a plain column; vs_logs
-- FKs (org_id, pattern_id) → vs_patterns (org_id, id). A wrong-kind link is
-- prevented TODAY only by the id-scheme (log ids `p-<hex>` never collide with
-- the metric/trace `svc~signal` ids under (org_id, id)) — a STRUCTURAL, not a
-- DECLARED, guarantee. A future id-scheme change could silently FK a log child
-- to a metric/trace root with no DB-level rejection.
--
-- AFTER: vs_patterns gains a UNIQUE key on (org_id, id, kind) so it can be a
-- kind-carrying FK target, and vs_logs gains a fixed `kind` column (= 'log',
-- CHECK-enforced) plus a COMPOSITE FK (org_id, pattern_id, kind) →
-- vs_patterns (org_id, id, kind). A vs_logs row can therefore attach ONLY to a
-- kind='log' root; a wrong-kind link is rejected by the database, not merely
-- made unlikely by the id-scheme.
--
-- The pre-existing (org_id, pattern_id) → (org_id, id) FK with ON DELETE
-- CASCADE is LEFT INTACT (untouched) — the new composite FK is ADDED alongside
-- it and also cascades, so a root delete still drops its vs_logs children. This
-- migration is purely ADDITIVE (one new column, one UNIQUE key, one CHECK, one
-- FK); it drops nothing, so it is safe on an existing populated DB and loses no
-- data. Existing vs_logs rows are all logs attached to kind='log' roots, so the
-- new column DEFAULT 'log', the CHECK, and the composite FK all validate over
-- the already-learned data with no rewrite.
--
-- RUNTIME DML UNCHANGED: the catalog store's INSERT (sqlCatalogUpsertLog) does
-- not name `kind`, so the DEFAULT 'log' fills it on every write and ON CONFLICT
-- never touches it. No Go change is needed to keep writing.
--
-- IDEMPOTENCY: ledger-tracked by RunSQLMigrations under
-- versus_schema_migrations, so this file runs EXACTLY ONCE (a re-run is a
-- no-op — the ledger, not IF NOT EXISTS, is the source of truth, per the 003
-- convention). Each ADD is named so a stray manual re-run fails
-- loudly rather than silently double-applying.
--
-- SQLi-safety: static DDL — no value is interpolated. All runtime DML against
-- these tables names them as Go constants and binds every value as a $N param.
--
-- ============================================================================
-- ENTERPRISE MIRROR (for the Enterprise Engineer — vs_metrics / vs_traces):
-- the shared UNIQUE (org_id, id, kind) on vs_patterns is added HERE (OSS 004,
-- which runs before the enterprise migration set on the same pool), so the
-- enterprise migration reuses it and adds ONLY the per-table column + CHECK +
-- composite FK. The EXACT shape to add in the enterprise migration is:
--
--   ALTER TABLE vs_metrics ADD COLUMN kind TEXT NOT NULL DEFAULT 'metric';
--   ALTER TABLE vs_metrics ADD CONSTRAINT vs_metrics_kind_is_metric
--       CHECK (kind = 'metric');
--   ALTER TABLE vs_metrics ADD CONSTRAINT vs_metrics_patterns_kind_fk
--       FOREIGN KEY (org_id, pattern_id, kind)
--       REFERENCES vs_patterns (org_id, id, kind) ON DELETE CASCADE;
--
--   ALTER TABLE vs_traces ADD COLUMN kind TEXT NOT NULL DEFAULT 'trace';
--   ALTER TABLE vs_traces ADD CONSTRAINT vs_traces_kind_is_trace
--       CHECK (kind = 'trace');
--   ALTER TABLE vs_traces ADD CONSTRAINT vs_traces_patterns_kind_fk
--       FOREIGN KEY (org_id, pattern_id, kind)
--       REFERENCES vs_patterns (org_id, id, kind) ON DELETE CASCADE;
--
-- (Do NOT re-add the UNIQUE (org_id, id, kind) on vs_patterns in enterprise —
-- OSS 004 owns it. Keep the existing (org_id, pattern_id) → (org_id, id)
-- cascade FKs on vs_metrics/vs_traces intact, exactly as this file does for
-- vs_logs.)
-- ============================================================================

-- 1) Make (org_id, id, kind) a UNIQUE key on the catalog root so it can be the
--    target of a kind-carrying composite FK. (org_id, id) is already the PK,
--    so this is trivially unique too; it merely lets a child pin the kind.
ALTER TABLE vs_patterns
    ADD CONSTRAINT vs_patterns_org_id_kind_key UNIQUE (org_id, id, kind);

-- 2) Give vs_logs a fixed `kind` column. DEFAULT 'log' backfills existing rows
--    and every future INSERT that omits it; CHECK pins it so a vs_logs row can
--    never carry a non-'log' kind (belt to the FK's braces).
ALTER TABLE vs_logs
    ADD COLUMN kind TEXT NOT NULL DEFAULT 'log';
ALTER TABLE vs_logs
    ADD CONSTRAINT vs_logs_kind_is_log CHECK (kind = 'log');

-- 3) Composite FK: a vs_logs row may attach ONLY to a kind='log' root. The
--    original (org_id, pattern_id) → (org_id, id) cascade FK is left intact
--    above/alongside; this one adds the kind pin and cascades in lockstep.
ALTER TABLE vs_logs
    ADD CONSTRAINT vs_logs_patterns_kind_fk
        FOREIGN KEY (org_id, pattern_id, kind)
        REFERENCES vs_patterns (org_id, id, kind) ON DELETE CASCADE;
