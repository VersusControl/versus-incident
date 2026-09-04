CREATE TABLE IF NOT EXISTS vs_detection_episodes (
    episode_id                 TEXT        PRIMARY KEY,
    org_id                     TEXT        NOT NULL,
    identity_version           TEXT        NOT NULL,
    agent_kind                 TEXT        NOT NULL,
    source                     TEXT        NOT NULL,
    service                    TEXT        NOT NULL,
    signal_kind                TEXT        NOT NULL,
    condition_key              TEXT        NOT NULL,
    fingerprint                TEXT        NOT NULL,
    incident_id                TEXT        NOT NULL,
    occurrence_count           BIGINT      NOT NULL CHECK (occurrence_count > 0),
    first_seen                 TIMESTAMPTZ NOT NULL,
    last_seen                  TIMESTAMPTZ NOT NULL,
    highest_observed_severity  TEXT        NOT NULL,
    highest_handled_severity   TEXT,
    highest_notified_severity  TEXT,
    closed_at                  TIMESTAMPTZ,
    pending_kind               TEXT,
    pending_severity           TEXT,
    pending_token              TEXT,
    pending_owner              TEXT,
    pending_expires_at         TIMESTAMPTZ,
    last_completed_token       TEXT
    ,last_notification_outcome TEXT
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_detection_episodes_active_identity
    ON vs_detection_episodes (org_id, fingerprint)
    WHERE closed_at IS NULL;

CREATE UNIQUE INDEX IF NOT EXISTS idx_detection_episodes_org_incident
    ON vs_detection_episodes (org_id, incident_id);

CREATE INDEX IF NOT EXISTS idx_detection_episodes_org_last_seen
    ON vs_detection_episodes (org_id, last_seen DESC);