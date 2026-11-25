-- Group corrections: the renames, merges and moves a user made to the
-- resource groups of one AWS account. They key on stable group IDs and
-- member ARNs, so they survive every re-read. Schema only.
--
-- kind is rename, merge or move. A rename names group_id; a merge folds
-- group_id into target_group_id; a move puts member_arn into
-- target_group_id. The column a kind does not use is the empty string, so
-- one row exists per group renamed, group merged and member moved.

CREATE TABLE IF NOT EXISTS aws_group_corrections (
    org_id          TEXT COLLATE "C" NOT NULL,
    account_id      TEXT COLLATE "C" NOT NULL,
    kind            TEXT COLLATE "C" NOT NULL CHECK (kind IN ('rename', 'merge', 'move')),
    group_id        TEXT COLLATE "C" NOT NULL DEFAULT '',
    member_arn      TEXT COLLATE "C" NOT NULL DEFAULT '',
    target_group_id TEXT COLLATE "C" NOT NULL DEFAULT '',
    name            TEXT             NOT NULL DEFAULT '',
    created_by      TEXT             NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ      NOT NULL,
    PRIMARY KEY (org_id, account_id, kind, group_id, member_arn)
);

-- Corrections are applied in the order they were made.
CREATE INDEX IF NOT EXISTS aws_group_corrections_org_created
    ON aws_group_corrections (org_id, created_at);

-- A group's page lists the corrections that point into it.
CREATE INDEX IF NOT EXISTS aws_group_corrections_target
    ON aws_group_corrections (org_id, account_id, target_group_id);
