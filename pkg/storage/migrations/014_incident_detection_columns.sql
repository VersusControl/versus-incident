ALTER TABLE vs_incidents ADD COLUMN IF NOT EXISTS detection_fingerprint       TEXT;
ALTER TABLE vs_incidents ADD COLUMN IF NOT EXISTS detection_episode_id        TEXT;
ALTER TABLE vs_incidents ADD COLUMN IF NOT EXISTS occurrence_count            BIGINT;
ALTER TABLE vs_incidents ADD COLUMN IF NOT EXISTS detection_first_seen        TIMESTAMPTZ;
ALTER TABLE vs_incidents ADD COLUMN IF NOT EXISTS detection_last_seen         TIMESTAMPTZ;
ALTER TABLE vs_incidents ADD COLUMN IF NOT EXISTS highest_observed_severity   TEXT;
ALTER TABLE vs_incidents ADD COLUMN IF NOT EXISTS highest_notified_severity   TEXT;

CREATE UNIQUE INDEX IF NOT EXISTS idx_incidents_detection_episode
    ON vs_incidents (org_id, detection_episode_id)
    WHERE detection_episode_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_incidents_detection_fingerprint
    ON vs_incidents (org_id, detection_fingerprint)
    WHERE detection_fingerprint IS NOT NULL;