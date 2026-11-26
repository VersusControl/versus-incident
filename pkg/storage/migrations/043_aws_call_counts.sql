-- Call ledger: how many AWS requests the collectors made, per account,
-- Region, source (the collector unit service) and hour. Every request is a
-- CloudTrail management event in the customer's account, so this count is
-- what the customer's GuardDuty, extra trail copies and log pipelines bill
-- for. Schema only.
--
-- calls counts every request sent, retries included. metered counts the
-- part AWS bills directly per request (S3, KMS, SQS). runs counts the unit
-- runs that made them, and peak is the most calls one of those runs made,
-- what a per-run budget is measured against.
-- A collector run adds to its hour's row.

CREATE TABLE IF NOT EXISTS aws_call_counts (
    org_id     TEXT COLLATE "C" NOT NULL,
    account_id TEXT COLLATE "C" NOT NULL,
    region     TEXT COLLATE "C" NOT NULL,
    source     TEXT COLLATE "C" NOT NULL,
    hour       TIMESTAMPTZ      NOT NULL,
    calls      BIGINT           NOT NULL DEFAULT 0 CHECK (calls >= 0),
    metered    BIGINT           NOT NULL DEFAULT 0 CHECK (metered >= 0),
    runs       BIGINT           NOT NULL DEFAULT 0 CHECK (runs >= 0),
    peak       BIGINT           NOT NULL DEFAULT 0 CHECK (peak >= 0),
    PRIMARY KEY (org_id, account_id, region, source, hour)
);

-- The last 24 hours of one account.
CREATE INDEX IF NOT EXISTS aws_call_counts_account_hour
    ON aws_call_counts (org_id, account_id, hour);

-- The last 24 hours of every account, and the retention sweep.
CREATE INDEX IF NOT EXISTS aws_call_counts_hour
    ON aws_call_counts (org_id, hour);
