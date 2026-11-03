SET LOCAL lock_timeout='2s';
ALTER TABLE kubernetes_evidence_occurrence_observations ADD COLUMN IF NOT EXISTS measurements JSONB;
DO $$
BEGIN
 IF to_regclass('kubernetes_evidence_observations_partitioned') IS NOT NULL THEN
  ALTER TABLE kubernetes_evidence_observations_partitioned ADD COLUMN IF NOT EXISTS measurements JSONB;
 END IF;
END $$;
