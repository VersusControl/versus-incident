-- Change feed: the columns the hour buckets and error bursts group by, the
-- per-minute counts that stand in for routine events and failure storms,
-- the reason each unread stretch was not read, and per-pair poll state.
-- Schema only: existing rows keep empty defaults and are rebuilt by polling.

ALTER TABLE aws_changes ADD COLUMN IF NOT EXISTS error_code TEXT NOT NULL DEFAULT '';
ALTER TABLE aws_changes ADD COLUMN IF NOT EXISTS template TEXT NOT NULL DEFAULT '';
ALTER TABLE aws_changes ADD COLUMN IF NOT EXISTS session_class TEXT NOT NULL DEFAULT '';
ALTER TABLE aws_changes ADD COLUMN IF NOT EXISTS via TEXT NOT NULL DEFAULT '';
ALTER TABLE aws_changes ADD COLUMN IF NOT EXISTS recorded_at TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS aws_changes_failures
    ON aws_changes (org_id, account, region, event_name, error_code, event_time);

CREATE TABLE IF NOT EXISTS aws_change_counts (
    org_id        TEXT COLLATE "C" NOT NULL,
    account       TEXT COLLATE "C" NOT NULL,
    region        TEXT COLLATE "C" NOT NULL,
    kind          TEXT COLLATE "C" NOT NULL,
    event_name    TEXT COLLATE "C" NOT NULL,
    actor         TEXT COLLATE "C" NOT NULL DEFAULT '',
    session_class TEXT COLLATE "C" NOT NULL DEFAULT '',
    error_code    TEXT COLLATE "C" NOT NULL DEFAULT '',
    minute        TIMESTAMPTZ      NOT NULL,
    service       TEXT COLLATE "C" NOT NULL DEFAULT '',
    event_source  TEXT             NOT NULL DEFAULT '',
    actor_class   TEXT             NOT NULL DEFAULT '',
    events        BIGINT           NOT NULL DEFAULT 0,
    PRIMARY KEY (org_id, account, region, kind, event_name, actor, session_class, error_code, minute)
);
CREATE INDEX IF NOT EXISTS aws_change_counts_minute
    ON aws_change_counts (org_id, minute);

CREATE TABLE IF NOT EXISTS aws_change_gaps (
    org_id   TEXT COLLATE "C" NOT NULL,
    account  TEXT COLLATE "C" NOT NULL,
    region   TEXT COLLATE "C" NOT NULL,
    gap_from TIMESTAMPTZ      NOT NULL,
    gap_to   TIMESTAMPTZ      NOT NULL,
    reason   TEXT             NOT NULL,
    detail   TEXT             NOT NULL DEFAULT '',
    PRIMARY KEY (org_id, account, region, gap_from)
);
CREATE INDEX IF NOT EXISTS aws_change_gaps_to
    ON aws_change_gaps (org_id, gap_to);

CREATE TABLE IF NOT EXISTS aws_change_pairs (
    org_id         TEXT COLLATE "C" NOT NULL,
    account        TEXT COLLATE "C" NOT NULL,
    region         TEXT COLLATE "C" NOT NULL,
    connected_at   TIMESTAMPTZ      NOT NULL,
    history_from   TIMESTAMPTZ      NOT NULL,
    last_poll_at   TIMESTAMPTZ,
    late_arrivals  BIGINT           NOT NULL DEFAULT 0,
    dropped        BIGINT           NOT NULL DEFAULT 0,
    clock_skew_ms  BIGINT           NOT NULL DEFAULT 0,
    PRIMARY KEY (org_id, account, region)
);
