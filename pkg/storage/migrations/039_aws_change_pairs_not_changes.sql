-- not_changes counts events CloudTrail flagged as writes that change
-- nothing (sign-ins, tokens, reads). Ingest leaves them out; the count is
-- kept for diagnostics.
ALTER TABLE aws_change_pairs ADD COLUMN IF NOT EXISTS not_changes BIGINT NOT NULL DEFAULT 0;
