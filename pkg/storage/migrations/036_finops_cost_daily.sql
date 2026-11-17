-- The daily cost rollup: the cost lines without a resource ID summed per
-- payer, day, account, region, service, service name, usage group, record
-- type and team, over their usage types. A page total groups by these
-- columns or fewer, so it reads one row per group here instead of one row per
-- usage type in finops_cost_lines. A cost-line write replaces the rollup rows
-- of the same days in the same transaction.
--
-- Every key column is byte-ordered, usage_group included, so a grouping
-- ordered by the totals index key is read in index order: no sort and no
-- hash, whatever the planner estimates for a freshly written table.
-- overflowed marks a row whose sum did not fit a signed 64-bit amount; its
-- sums are zero and a total that touches it is read from the cost lines.
--
-- finops_cost_monthly sums the daily rows of one calendar month (month is its
-- first day; min_day is the group's first billing day). A total without the
-- day over a range holding whole months reads those months here and only the
-- remaining days from the daily rollup. It is rebuilt from the daily rollup
-- for every month a cost-line write touches, in the same transaction.

CREATE TABLE IF NOT EXISTS finops_cost_daily (
    org_id               TEXT COLLATE "C" NOT NULL,
    payer                TEXT COLLATE "C" NOT NULL,
    day                  DATE             NOT NULL,
    account              TEXT COLLATE "C" NOT NULL DEFAULT '',
    region               TEXT COLLATE "C" NOT NULL DEFAULT '',
    service              TEXT COLLATE "C" NOT NULL DEFAULT '',
    service_name         TEXT COLLATE "C" NOT NULL DEFAULT '',
    usage_group          TEXT COLLATE "C" NOT NULL DEFAULT '',
    record_type          TEXT COLLATE "C" NOT NULL DEFAULT '',
    team                 TEXT COLLATE "C" NOT NULL DEFAULT '',
    net_amortized_micros BIGINT           NOT NULL DEFAULT 0,
    quantity_micros      BIGINT           NOT NULL DEFAULT 0,
    lines                BIGINT           NOT NULL DEFAULT 0,
    estimated_team       BOOLEAN          NOT NULL DEFAULT FALSE,
    estimated_region     BOOLEAN          NOT NULL DEFAULT FALSE,
    overflowed           BOOLEAN          NOT NULL DEFAULT FALSE,
    PRIMARY KEY (org_id, payer, day, account, region, service, service_name, usage_group, record_type, team)
);
CREATE INDEX IF NOT EXISTS finops_cost_daily_totals
    ON finops_cost_daily (org_id, day, account, region, service, service_name, usage_group, record_type, team, payer)
    INCLUDE (net_amortized_micros, quantity_micros, lines, estimated_team, estimated_region, overflowed);
CREATE INDEX IF NOT EXISTS finops_cost_daily_account
    ON finops_cost_daily (org_id, account, day);
CREATE INDEX IF NOT EXISTS finops_cost_daily_service
    ON finops_cost_daily (org_id, service, day);

CREATE TABLE IF NOT EXISTS finops_cost_monthly (
    org_id               TEXT COLLATE "C" NOT NULL,
    payer                TEXT COLLATE "C" NOT NULL,
    month                DATE             NOT NULL,
    account              TEXT COLLATE "C" NOT NULL DEFAULT '',
    region               TEXT COLLATE "C" NOT NULL DEFAULT '',
    service              TEXT COLLATE "C" NOT NULL DEFAULT '',
    service_name         TEXT COLLATE "C" NOT NULL DEFAULT '',
    usage_group          TEXT COLLATE "C" NOT NULL DEFAULT '',
    record_type          TEXT COLLATE "C" NOT NULL DEFAULT '',
    team                 TEXT COLLATE "C" NOT NULL DEFAULT '',
    min_day              DATE             NOT NULL,
    net_amortized_micros BIGINT           NOT NULL DEFAULT 0,
    quantity_micros      BIGINT           NOT NULL DEFAULT 0,
    lines                BIGINT           NOT NULL DEFAULT 0,
    estimated_team       BOOLEAN          NOT NULL DEFAULT FALSE,
    estimated_region     BOOLEAN          NOT NULL DEFAULT FALSE,
    overflowed           BOOLEAN          NOT NULL DEFAULT FALSE,
    PRIMARY KEY (org_id, payer, month, account, region, service, service_name, usage_group, record_type, team)
);
CREATE INDEX IF NOT EXISTS finops_cost_monthly_totals
    ON finops_cost_monthly (org_id, month, account, region, service, service_name, usage_group, record_type, team, payer)
    INCLUDE (min_day, net_amortized_micros, quantity_micros, lines, estimated_team, estimated_region, overflowed);
