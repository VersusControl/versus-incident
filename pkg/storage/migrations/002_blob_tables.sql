-- 002_blob_tables.sql — give each agent JSON document its own table.
-- The opaque-blob documents that the file backend writes as separate
-- <name>.json files (patterns.json, shadow.json, detect.json,
-- members.json, teams.json) each get a dedicated Postgres table instead
-- of sharing vs_blobs. vs_blobs remains for any other/runtime blob
-- (e.g. the AI cache). Safe to re-run: IF NOT EXISTS everywhere.

CREATE TABLE IF NOT EXISTS vs_patterns (
    name       TEXT        PRIMARY KEY,
    data       BYTEA       NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS vs_shadow (
    name       TEXT        PRIMARY KEY,
    data       BYTEA       NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS vs_detect (
    name       TEXT        PRIMARY KEY,
    data       BYTEA       NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS vs_members (
    name       TEXT        PRIMARY KEY,
    data       BYTEA       NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS vs_teams (
    name       TEXT        PRIMARY KEY,
    data       BYTEA       NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Search support: GIN indexes over the JSONB document bodies so incident
-- and analysis search (containment + field lookups) stays fast as the
-- history grows.
CREATE INDEX IF NOT EXISTS idx_incidents_data_gin ON vs_incidents USING gin (data);
CREATE INDEX IF NOT EXISTS idx_analyses_data_gin  ON vs_analyses  USING gin (data);
