-- A view that filters cost lines by region otherwise scans the payer's whole
-- day range: the other cost-line indexes lead with day, team, service,
-- account or resource, and none of them can answer a region predicate.

CREATE INDEX IF NOT EXISTS finops_cost_lines_region
    ON finops_cost_lines (org_id, region, day);
