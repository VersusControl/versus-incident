CREATE INDEX IF NOT EXISTS idx_analyses_org_normalized
    ON vs_analyses ((COALESCE(NULLIF(data->>'org_id', ''), 'default')));