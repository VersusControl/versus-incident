-- 001_init.sql — Versus Incident initial Postgres schema
-- Safe to re-run: all statements use IF NOT EXISTS.

CREATE TABLE IF NOT EXISTS vs_blobs (
    name       TEXT        PRIMARY KEY,
    data       BYTEA       NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS vs_incidents (
    id         TEXT        PRIMARY KEY,
    data       JSONB       NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    acked_at   TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_incidents_created_at ON vs_incidents (created_at DESC);

CREATE TABLE IF NOT EXISTS vs_analyses (
    id           TEXT        PRIMARY KEY,
    incident_id  TEXT        NOT NULL,
    data         JSONB       NOT NULL,
    requested_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_analyses_incident_id   ON vs_analyses (incident_id);
CREATE INDEX IF NOT EXISTS idx_analyses_requested_at  ON vs_analyses (requested_at DESC);
