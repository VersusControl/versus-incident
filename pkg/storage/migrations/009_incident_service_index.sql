CREATE INDEX IF NOT EXISTS idx_incidents_org_service_created_at
    ON vs_incidents (org_id, service, created_at DESC);
