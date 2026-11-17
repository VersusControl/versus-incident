-- AWS collector output. Every row carries its organization; key columns use
-- byte order so reads sort the same on every backend. Resources, health,
-- security flags and unit generations are temporal: a row is current while
-- valid_to is NULL, and a read as of t keeps valid_from <= t < valid_to.

-- The purge epoch of each account. A purge advances it in the transaction
-- that deletes the account; a guarded write locks the row and is refused
-- when the epoch moved since the writer started.
CREATE TABLE IF NOT EXISTS aws_account_epochs (
    org_id  TEXT COLLATE "C" NOT NULL,
    account TEXT COLLATE "C" NOT NULL,
    epoch   BIGINT           NOT NULL DEFAULT 0,
    PRIMARY KEY (org_id, account)
);

CREATE TABLE IF NOT EXISTS aws_sync_units (
    org_id       TEXT COLLATE "C" NOT NULL,
    account      TEXT COLLATE "C" NOT NULL,
    region       TEXT COLLATE "C" NOT NULL,
    service      TEXT COLLATE "C" NOT NULL,
    generation   BIGINT           NOT NULL DEFAULT 0,
    state        TEXT             NOT NULL DEFAULT '',
    last_good_at TIMESTAMPTZ,
    attempted_at TIMESTAMPTZ,
    verified_at  TIMESTAMPTZ,
    error_code   TEXT             NOT NULL DEFAULT '',
    gaps         JSONB,
    PRIMARY KEY (org_id, account, region, service)
);

CREATE TABLE IF NOT EXISTS aws_unit_generations (
    org_id         TEXT COLLATE "C" NOT NULL,
    account        TEXT COLLATE "C" NOT NULL,
    region         TEXT COLLATE "C" NOT NULL,
    service        TEXT COLLATE "C" NOT NULL,
    seq            BIGINT           NOT NULL,
    digest         TEXT             NOT NULL,
    verified_at    TIMESTAMPTZ      NOT NULL,
    resource_count INTEGER          NOT NULL,
    gaps           JSONB,
    valid_from     TIMESTAMPTZ      NOT NULL,
    valid_to       TIMESTAMPTZ,
    PRIMARY KEY (org_id, account, region, service, seq)
);
CREATE UNIQUE INDEX IF NOT EXISTS aws_unit_generations_current
    ON aws_unit_generations (org_id, account, region, service) WHERE valid_to IS NULL;
CREATE INDEX IF NOT EXISTS aws_unit_generations_history
    ON aws_unit_generations (org_id, account, region, service, valid_from);
CREATE INDEX IF NOT EXISTS aws_unit_generations_retention
    ON aws_unit_generations (org_id, valid_to) WHERE valid_to IS NOT NULL;

CREATE TABLE IF NOT EXISTS aws_resources (
    org_id        TEXT COLLATE "C" NOT NULL,
    account       TEXT COLLATE "C" NOT NULL,
    region        TEXT COLLATE "C" NOT NULL,
    service       TEXT COLLATE "C" NOT NULL,
    arn           TEXT COLLATE "C" NOT NULL,
    resource_type TEXT COLLATE "C" NOT NULL DEFAULT '',
    app           TEXT COLLATE "C" NOT NULL DEFAULT '',
    name          TEXT COLLATE "C" NOT NULL DEFAULT '',
    state         TEXT             NOT NULL DEFAULT '',
    tags          JSONB,
    relations     JSONB,
    attributes    JSONB,
    detail        JSONB,
    digest        TEXT             NOT NULL,
    valid_from    TIMESTAMPTZ      NOT NULL,
    valid_to      TIMESTAMPTZ
);
CREATE UNIQUE INDEX IF NOT EXISTS aws_resources_current
    ON aws_resources (org_id, account, region, service, arn) WHERE valid_to IS NULL;
CREATE INDEX IF NOT EXISTS aws_resources_history
    ON aws_resources (org_id, account, region, service, valid_from);
CREATE INDEX IF NOT EXISTS aws_resources_app
    ON aws_resources (org_id, app) WHERE valid_to IS NULL;
CREATE INDEX IF NOT EXISTS aws_resources_type
    ON aws_resources (org_id, resource_type) WHERE valid_to IS NULL;
CREATE INDEX IF NOT EXISTS aws_resources_name
    ON aws_resources (org_id, lower(name)) WHERE valid_to IS NULL;
CREATE INDEX IF NOT EXISTS aws_resources_arn
    ON aws_resources (org_id, arn, valid_from);
CREATE INDEX IF NOT EXISTS aws_resources_retention
    ON aws_resources (org_id, valid_to) WHERE valid_to IS NOT NULL;

CREATE TABLE IF NOT EXISTS aws_health (
    org_id     TEXT COLLATE "C" NOT NULL,
    account    TEXT COLLATE "C" NOT NULL,
    region     TEXT COLLATE "C" NOT NULL,
    service    TEXT COLLATE "C" NOT NULL,
    arn        TEXT COLLATE "C" NOT NULL DEFAULT '',
    kind       TEXT COLLATE "C" NOT NULL,
    ref        TEXT COLLATE "C" NOT NULL DEFAULT '',
    state      TEXT COLLATE "C" NOT NULL DEFAULT '',
    summary    TEXT             NOT NULL DEFAULT '',
    since      TIMESTAMPTZ,
    children   JSONB,
    detail     JSONB,
    digest     TEXT             NOT NULL,
    valid_from TIMESTAMPTZ      NOT NULL,
    valid_to   TIMESTAMPTZ
);
CREATE UNIQUE INDEX IF NOT EXISTS aws_health_current
    ON aws_health (org_id, account, region, service, arn, kind, ref) WHERE valid_to IS NULL;
CREATE INDEX IF NOT EXISTS aws_health_history
    ON aws_health (org_id, account, region, service, valid_from);
CREATE INDEX IF NOT EXISTS aws_health_state
    ON aws_health (org_id, state) WHERE valid_to IS NULL;
CREATE INDEX IF NOT EXISTS aws_health_arn
    ON aws_health (org_id, arn, valid_from);
CREATE INDEX IF NOT EXISTS aws_health_retention
    ON aws_health (org_id, valid_to) WHERE valid_to IS NOT NULL;

CREATE TABLE IF NOT EXISTS aws_security_flags (
    org_id     TEXT COLLATE "C" NOT NULL,
    account    TEXT COLLATE "C" NOT NULL,
    region     TEXT COLLATE "C" NOT NULL,
    service    TEXT COLLATE "C" NOT NULL,
    arn        TEXT COLLATE "C" NOT NULL,
    flag       TEXT COLLATE "C" NOT NULL,
    summary    TEXT             NOT NULL DEFAULT '',
    detail     JSONB,
    digest     TEXT             NOT NULL,
    valid_from TIMESTAMPTZ      NOT NULL,
    valid_to   TIMESTAMPTZ
);
CREATE UNIQUE INDEX IF NOT EXISTS aws_security_flags_current
    ON aws_security_flags (org_id, account, region, service, arn, flag) WHERE valid_to IS NULL;
CREATE INDEX IF NOT EXISTS aws_security_flags_history
    ON aws_security_flags (org_id, account, region, service, valid_from);
CREATE INDEX IF NOT EXISTS aws_security_flags_flag
    ON aws_security_flags (org_id, flag) WHERE valid_to IS NULL;
CREATE INDEX IF NOT EXISTS aws_security_flags_retention
    ON aws_security_flags (org_id, valid_to) WHERE valid_to IS NOT NULL;

CREATE TABLE IF NOT EXISTS aws_changes (
    org_id       TEXT COLLATE "C" NOT NULL,
    event_id     TEXT COLLATE "C" NOT NULL,
    account      TEXT COLLATE "C" NOT NULL,
    region       TEXT COLLATE "C" NOT NULL,
    service      TEXT COLLATE "C" NOT NULL DEFAULT '',
    event_time   TIMESTAMPTZ      NOT NULL,
    event_name   TEXT             NOT NULL DEFAULT '',
    actor_class  TEXT             NOT NULL DEFAULT '',
    actor        TEXT             NOT NULL DEFAULT '',
    resource_arn TEXT COLLATE "C" NOT NULL DEFAULT '',
    app          TEXT COLLATE "C" NOT NULL DEFAULT '',
    summary      TEXT             NOT NULL DEFAULT '',
    fields       JSONB,
    detail       JSONB,
    PRIMARY KEY (org_id, event_id)
);
CREATE INDEX IF NOT EXISTS aws_changes_time
    ON aws_changes (org_id, event_time);
CREATE INDEX IF NOT EXISTS aws_changes_resource
    ON aws_changes (org_id, resource_arn, event_time);
CREATE INDEX IF NOT EXISTS aws_changes_app
    ON aws_changes (org_id, app, event_time);
CREATE INDEX IF NOT EXISTS aws_changes_pair
    ON aws_changes (org_id, account, region, event_time);

CREATE TABLE IF NOT EXISTS aws_change_coverage (
    org_id        TEXT COLLATE "C" NOT NULL,
    account       TEXT COLLATE "C" NOT NULL,
    region        TEXT COLLATE "C" NOT NULL,
    covered_from  TIMESTAMPTZ      NOT NULL,
    covered_until TIMESTAMPTZ      NOT NULL,
    PRIMARY KEY (org_id, account, region, covered_from)
);
CREATE INDEX IF NOT EXISTS aws_change_coverage_until
    ON aws_change_coverage (org_id, account, region, covered_until);
