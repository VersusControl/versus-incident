-- AWS issues: one row per rule and owner, with its life cycle (open,
-- acknowledged, rejected, resolved, regressed) and the events that moved
-- it. Schema only.
--
-- id is a hash of the rule and the owner, so it is stable across node
-- churn, restarts and replicas. version is bumped by every write; a write
-- names the version it read and fails when another writer moved the row
-- first. rejected_at is set while the issue carries a rejection, including
-- one kept through a resolve, and the retention sweep never deletes such a
-- row or the events of its issue. payload holds the rest of the issue
-- (evidence, steps, the rejection) as the issue package encodes it.

CREATE TABLE IF NOT EXISTS aws_issues (
    org_id      TEXT COLLATE "C" NOT NULL,
    id          TEXT COLLATE "C" NOT NULL,
    rule        TEXT COLLATE "C" NOT NULL,
    kind        TEXT COLLATE "C" NOT NULL CHECK (kind IN ('reliability', 'security')),
    severity    TEXT COLLATE "C" NOT NULL CHECK (severity IN ('critical', 'high', 'medium', 'low')),
    tier        TEXT COLLATE "C" NOT NULL CHECK (tier IN ('act_now', 'worth_a_look')),
    state       TEXT COLLATE "C" NOT NULL CHECK (state IN ('open', 'acknowledged', 'rejected', 'resolved', 'regressed')),
    confirm     TEXT COLLATE "C" NOT NULL CHECK (confirm IN ('confirmed', 'cant_confirm')),
    account_id  TEXT COLLATE "C" NOT NULL,
    region      TEXT COLLATE "C" NOT NULL DEFAULT '',
    owner       TEXT COLLATE "C" NOT NULL,
    group_id    TEXT COLLATE "C" NOT NULL DEFAULT '',
    title       TEXT             NOT NULL DEFAULT '',
    since       TIMESTAMPTZ,
    first_seen  TIMESTAMPTZ      NOT NULL,
    opened_at   TIMESTAMPTZ      NOT NULL,
    resolved_at TIMESTAMPTZ,
    rejected_at TIMESTAMPTZ,
    review_on   DATE,
    version     BIGINT           NOT NULL CHECK (version > 0),
    updated_at  TIMESTAMPTZ      NOT NULL,
    payload     JSONB,
    PRIMARY KEY (org_id, id)
);

-- The queue and the summary: open issues by severity.
CREATE INDEX IF NOT EXISTS aws_issues_state_severity
    ON aws_issues (org_id, state, severity);

-- One account's issues, read by each evaluation.
CREATE INDEX IF NOT EXISTS aws_issues_account
    ON aws_issues (org_id, account_id);

-- One group's issues.
CREATE INDEX IF NOT EXISTS aws_issues_group
    ON aws_issues (org_id, group_id);

-- Rejected issues whose review date has come.
CREATE INDEX IF NOT EXISTS aws_issues_review_on
    ON aws_issues (org_id, review_on) WHERE state = 'rejected';

-- The events of one issue, in order. kind is what happened; actor is the
-- user, or the evaluator; detail says why (a reject's detail is its
-- reason).
CREATE TABLE IF NOT EXISTS aws_issue_events (
    org_id   TEXT COLLATE "C" NOT NULL,
    issue_id TEXT COLLATE "C" NOT NULL,
    seq      BIGINT           NOT NULL,
    at       TIMESTAMPTZ      NOT NULL,
    kind     TEXT COLLATE "C" NOT NULL CHECK (kind IN ('opened', 'acknowledged', 'rejected', 'reopened', 'resolved', 'regressed', 'alerted', 'alert_given_up', 'reread')),
    actor    TEXT             NOT NULL DEFAULT '',
    detail   TEXT             NOT NULL DEFAULT '',
    PRIMARY KEY (org_id, issue_id, seq)
);

-- The retention sweep.
CREATE INDEX IF NOT EXISTS aws_issue_events_at
    ON aws_issue_events (org_id, at);

-- When each rule was first assessed on an account, per Region ('' for an
-- account-wide rule). A rule with no row in any organization the
-- deployment reads has never been assessed there, so its next assessment is
-- the baseline: what it finds existed when first read. A rule added in a
-- later release baselines on its own first assessment. A row with an empty
-- rule is a reset, written when the account or the Region stops being
-- read: baselines taken before it no longer count, so reconnecting behaves
-- like a first connect.
CREATE TABLE IF NOT EXISTS aws_issue_baselines (
    org_id            TEXT COLLATE "C" NOT NULL,
    account_id        TEXT COLLATE "C" NOT NULL,
    rule              TEXT COLLATE "C" NOT NULL,
    region            TEXT COLLATE "C" NOT NULL DEFAULT '',
    first_assessed_at TIMESTAMPTZ      NOT NULL,
    PRIMARY KEY (org_id, account_id, rule, region)
);
