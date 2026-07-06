-- 003_signal_tables.sql — Core, OSS.
-- Refactor the log-pattern catalog from a whole-blob row into explicit,
-- searchable, typed signal tables. The old whole-blob vs_patterns (created
-- by 002) is DROPPED — its data is regenerable — and replaced by the typed
-- catalog root plus its log/service
-- children. "patterns" is also removed from the Postgres blobTables allowlist
-- (pkg/storage/postgres.go) so the catalog no longer routes through a
-- whole-blob table.
--
-- This file is ledger-tracked by RunSQLMigrations and therefore runs
-- EXACTLY ONCE — the DROP below is safe because it never re-runs on a later
-- boot. It is NOT written with IF NOT EXISTS on the typed tables for that
-- reason: the ledger, not IF-NOT-EXISTS, provides idempotency.
--
-- SQLi-safety: this is static DDL — no value is interpolated. All runtime DML
-- against these tables (the Postgres catalog store) binds every value as a
-- parameter and names the tables as Go constants.

-- Drop the old whole-blob catalog table (name, data, updated_at). Its
-- learned/curated content is regenerable and self-heals within one learning
-- window; there is no lossy blob->columns copy.
DROP TABLE IF EXISTS vs_patterns;

-- Catalog ROOT: one partition-neutral row per learned signal identity, shared
-- by log/metric/trace so a single query lists every learned signal for a
-- service across kinds. Holds identity + the fleet-wide operator-curated
-- overlay (verdict/tags/tombstone/service reassignment) — this REPLACES the
-- enterprise shared `overlay` blob. The OSS catalog store only ever
-- reads/writes kind='log' rows; the enterprise intel store owns
-- kind IN ('metric','trace') on this SAME root.
CREATE TABLE vs_patterns (
    org_id     TEXT        NOT NULL,
    id         TEXT        NOT NULL,                    -- kind-namespaced signal id
    kind       TEXT        NOT NULL,                    -- 'log' | 'metric' | 'trace'
    service    TEXT,                                    -- resolved attribution (upsert: real wins)
    verdict    TEXT,                                    -- operator curation (log): '' | 'known'
    tags       JSONB       NOT NULL DEFAULT '[]',
    deleted    BOOLEAN     NOT NULL DEFAULT FALSE,      -- tombstone: suppressed fleet-wide
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (org_id, id)
);
CREATE INDEX idx_patterns_kind    ON vs_patterns (org_id, kind);
CREATE INDEX idx_patterns_service ON vs_patterns (org_id, service);
CREATE INDEX idx_patterns_verdict ON vs_patterns (org_id, verdict);

-- Log-pattern LEARNED properties. PARTITIONED: log sources fan out across HA
-- instances, so each instance is the single writer for its
-- instance_index rows and the fleet view SUMs across partitions (no lost
-- updates). Single-instance / OSS writes instance_index = 0 (one row per
-- pattern), so the column names no HA policy — it is a tier-neutral
-- write-shard ordinal.
CREATE TABLE vs_logs (
    org_id             TEXT             NOT NULL,
    pattern_id         TEXT             NOT NULL,
    instance_index     INT              NOT NULL DEFAULT 0,   -- HA ordinal; 0 = single-instance
    template           TEXT             NOT NULL,
    source             TEXT,
    rule_name          TEXT,
    count              BIGINT           NOT NULL DEFAULT 0,
    baseline_frequency DOUBLE PRECISION NOT NULL DEFAULT 0,
    samples            JSONB            NOT NULL DEFAULT '[]', -- redacted ring, oldest->newest
    first_seen         TIMESTAMPTZ      NOT NULL,
    last_seen          TIMESTAMPTZ      NOT NULL,
    PRIMARY KEY (org_id, pattern_id, instance_index),
    FOREIGN KEY (org_id, pattern_id)
        REFERENCES vs_patterns (org_id, id) ON DELETE CASCADE
);
CREATE INDEX idx_logs_last_seen ON vs_logs (org_id, last_seen DESC);

-- Discovered/manual services: a catalog-scoped entity of its OWN, NOT a
-- pattern child (plan §3.3). Convergent discovery + operator curation =>
-- partition-neutral, keyed per org.
CREATE TABLE vs_services (
    org_id     TEXT        NOT NULL,
    name       TEXT        NOT NULL,
    manual     BOOLEAN     NOT NULL DEFAULT FALSE,
    first_seen TIMESTAMPTZ NOT NULL,                    -- grace anchor (zero-time => grace ended)
    deleted    BOOLEAN     NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (org_id, name)
);
CREATE INDEX idx_services_deleted ON vs_services (org_id, deleted);
