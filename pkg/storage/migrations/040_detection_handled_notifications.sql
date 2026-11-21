ALTER TABLE vs_detection_episodes
    ADD COLUMN IF NOT EXISTS handled_notifications BIGINT NOT NULL DEFAULT 0;
