-- Account labels: the name and label a user gives an AWS account the
-- deployment is not connected to ("our log archive", "Datadog"), so links to
-- it read as known. One row per organization and account. Schema only.

CREATE TABLE IF NOT EXISTS aws_account_labels (
    org_id     TEXT COLLATE "C" NOT NULL,
    account_id TEXT COLLATE "C" NOT NULL,
    label      TEXT             NOT NULL,
    name       TEXT             NOT NULL DEFAULT '',
    created_by TEXT             NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ      NOT NULL,
    PRIMARY KEY (org_id, account_id)
);
