-- FinOps cost lines and decisions. Every row carries its organization; key
-- columns use byte order so reads sort the same on every backend. An empty
-- team or resource ID means none, so the key never holds NULL. The provider's
-- service name is part of the key, so two provider services that map to one
-- neutral service stay separate lines.

CREATE TABLE IF NOT EXISTS finops_cost_lines (
    org_id               TEXT COLLATE "C" NOT NULL,
    payer                TEXT COLLATE "C" NOT NULL,
    day                  DATE             NOT NULL,
    account              TEXT COLLATE "C" NOT NULL DEFAULT '',
    region               TEXT COLLATE "C" NOT NULL DEFAULT '',
    service              TEXT COLLATE "C" NOT NULL DEFAULT '',
    service_name         TEXT COLLATE "C" NOT NULL DEFAULT '',
    usage_type           TEXT COLLATE "C" NOT NULL DEFAULT '',
    record_type          TEXT COLLATE "C" NOT NULL DEFAULT '',
    team                 TEXT COLLATE "C" NOT NULL DEFAULT '',
    resource_id          TEXT COLLATE "C" NOT NULL DEFAULT '',
    usage_group          TEXT             NOT NULL DEFAULT '',
    app                  TEXT             NOT NULL DEFAULT '',
    env                  TEXT             NOT NULL DEFAULT '',
    currency             TEXT             NOT NULL DEFAULT '',
    net_amortized_micros BIGINT           NOT NULL DEFAULT 0,
    quantity_micros      BIGINT           NOT NULL DEFAULT 0,
    unit                 TEXT             NOT NULL DEFAULT '',
    estimated_team       BOOLEAN          NOT NULL DEFAULT FALSE,
    estimated_region     BOOLEAN          NOT NULL DEFAULT FALSE,
    refreshed_at         TIMESTAMPTZ,
    PRIMARY KEY (org_id, payer, day, account, region, service, service_name, usage_type, record_type, team, resource_id)
);
CREATE INDEX IF NOT EXISTS finops_cost_lines_day
    ON finops_cost_lines (org_id, day);
CREATE INDEX IF NOT EXISTS finops_cost_lines_team
    ON finops_cost_lines (org_id, team, day);
CREATE INDEX IF NOT EXISTS finops_cost_lines_service
    ON finops_cost_lines (org_id, service, day);
CREATE INDEX IF NOT EXISTS finops_cost_lines_account
    ON finops_cost_lines (org_id, account, day);
CREATE INDEX IF NOT EXISTS finops_cost_lines_resource
    ON finops_cost_lines (org_id, resource_id, day) WHERE resource_id <> '';

CREATE TABLE IF NOT EXISTS finops_increases (
    org_id                TEXT COLLATE "C" NOT NULL,
    id                    TEXT COLLATE "C" NOT NULL,
    payer                 TEXT COLLATE "C" NOT NULL DEFAULT '',
    account               TEXT COLLATE "C" NOT NULL DEFAULT '',
    service               TEXT COLLATE "C" NOT NULL DEFAULT '',
    usage_type            TEXT COLLATE "C" NOT NULL DEFAULT '',
    record_type           TEXT COLLATE "C" NOT NULL DEFAULT '',
    resource_id           TEXT COLLATE "C" NOT NULL DEFAULT '',
    started_on            DATE             NOT NULL,
    resolved_on           DATE,
    status                TEXT COLLATE "C" NOT NULL DEFAULT '',
    owner_team            TEXT COLLATE "C" NOT NULL DEFAULT '',
    owner_rule            TEXT             NOT NULL DEFAULT '',
    owner_estimated       BOOLEAN          NOT NULL DEFAULT FALSE,
    monthly_impact_micros BIGINT           NOT NULL DEFAULT 0,
    expected_reason       TEXT             NOT NULL DEFAULT '',
    expected_until        DATE,
    alerted_at            TIMESTAMPTZ,
    suspects              JSONB,
    detail                JSONB,
    PRIMARY KEY (org_id, id)
);
CREATE INDEX IF NOT EXISTS finops_increases_status
    ON finops_increases (org_id, status, started_on);
CREATE INDEX IF NOT EXISTS finops_increases_owner
    ON finops_increases (org_id, owner_team);
CREATE INDEX IF NOT EXISTS finops_increases_service
    ON finops_increases (org_id, service);

CREATE TABLE IF NOT EXISTS finops_increase_events (
    org_id      TEXT COLLATE "C" NOT NULL,
    increase_id TEXT COLLATE "C" NOT NULL,
    seq         INTEGER          NOT NULL,
    at          TIMESTAMPTZ      NOT NULL,
    actor       TEXT             NOT NULL DEFAULT '',
    from_status TEXT             NOT NULL DEFAULT '',
    to_status   TEXT             NOT NULL DEFAULT '',
    reason      TEXT             NOT NULL DEFAULT '',
    until_day   DATE,
    PRIMARY KEY (org_id, increase_id, seq)
);
CREATE INDEX IF NOT EXISTS finops_increase_events_at
    ON finops_increase_events (org_id, increase_id, at);

CREATE TABLE IF NOT EXISTS finops_reports (
    org_id       TEXT COLLATE "C" NOT NULL,
    month        TEXT COLLATE "C" NOT NULL,
    frozen_at    TIMESTAMPTZ,
    frozen_by    TEXT             NOT NULL DEFAULT '',
    total_micros BIGINT           NOT NULL DEFAULT 0,
    model        JSONB            NOT NULL,
    PRIMARY KEY (org_id, month)
);

CREATE TABLE IF NOT EXISTS finops_saving_decisions (
    org_id    TEXT COLLATE "C" NOT NULL,
    saving_id TEXT COLLATE "C" NOT NULL,
    state     TEXT             NOT NULL DEFAULT '',
    reason    TEXT             NOT NULL DEFAULT '',
    actor     TEXT             NOT NULL DEFAULT '',
    at        TIMESTAMPTZ,
    PRIMARY KEY (org_id, saving_id)
);
