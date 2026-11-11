-- The cost totals query groups by day, account, region, service, service
-- name, usage group, record type, team and payer and sums two amount
-- columns. Every other cost-line index leads with day, team, service,
-- account, region or resource, so the aggregation reads one table row per
-- line. This index holds the grouping key in the order the statement groups
-- by and carries every remaining column the statement reads, so the
-- aggregation is answered from the index alone: it never visits the table.

CREATE INDEX IF NOT EXISTS finops_cost_lines_totals
    ON finops_cost_lines (org_id, day, account, region, service, service_name, usage_group, record_type, team, payer)
    INCLUDE (net_amortized_micros, quantity_micros, estimated_team, estimated_region, usage_type, resource_id);
