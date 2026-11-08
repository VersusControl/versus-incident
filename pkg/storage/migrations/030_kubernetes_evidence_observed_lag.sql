SET LOCAL lock_timeout='2s';
-- Crashes are read in the order they were observed, while the journal is kept
-- in monthly partitions by event time. A record is never observed before it
-- happened, and observed_lag is the longest any crash or coverage-gap record
-- of the scope was observed after it happened, so a read of what was observed
-- in a range needs only the months from the range's start minus this lag.
-- Every journal insert and update keeps it. observed_lag_scan is the month
-- before which a background pass still has to read history written before
-- this column existed (NULL when done); until then reads use no lag bound.
ALTER TABLE kubernetes_evidence_scopes ADD COLUMN IF NOT EXISTS observed_lag INTERVAL NOT NULL DEFAULT '0';
ALTER TABLE kubernetes_evidence_scopes ADD COLUMN IF NOT EXISTS observed_lag_scan TIMESTAMPTZ DEFAULT '9999-12-31 00:00:00+00';

CREATE OR REPLACE FUNCTION versus_evidence_observed_lag() RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
 UPDATE kubernetes_evidence_scopes s SET observed_lag=g.lag
 FROM (SELECT scope_key,MAX(observed_at-event_at) AS lag FROM fenced_rows WHERE event_type IN('crash','coverage_gap') GROUP BY scope_key) g
 WHERE s.scope_key=g.scope_key AND s.observed_lag<g.lag;
 RETURN NULL;
END $$;
CREATE OR REPLACE FUNCTION versus_install_evidence_observed_lag(journal REGCLASS) RETURNS VOID LANGUAGE plpgsql AS $$
BEGIN
 IF journal IS NULL THEN
  RETURN;
 END IF;
 EXECUTE format('DROP TRIGGER IF EXISTS evidence_observed_lag_insert ON %s', journal);
 EXECUTE format('DROP TRIGGER IF EXISTS evidence_observed_lag_update ON %s', journal);
 EXECUTE format('CREATE TRIGGER evidence_observed_lag_insert AFTER INSERT ON %s REFERENCING NEW TABLE AS fenced_rows FOR EACH STATEMENT EXECUTE FUNCTION versus_evidence_observed_lag()', journal);
 EXECUTE format('CREATE TRIGGER evidence_observed_lag_update AFTER UPDATE ON %s REFERENCING NEW TABLE AS fenced_rows FOR EACH STATEMENT EXECUTE FUNCTION versus_evidence_observed_lag()', journal);
END $$;
SELECT versus_install_evidence_observed_lag(to_regclass('kubernetes_evidence_events'));
SELECT versus_install_evidence_observed_lag(to_regclass('kubernetes_evidence_events_partitioned'));
DROP FUNCTION versus_install_evidence_observed_lag(REGCLASS);

-- While the journal is copied into its partitioned shadow, the shadow follows
-- every journal write. A crash status refreshed in place changes observed_at
-- and the payload's observed_at of the current row, so the copy takes those
-- too; otherwise the shadow, and the journal after the swap, would keep the
-- time the status was first seen.
CREATE OR REPLACE FUNCTION versus_mirror_evidence_event() RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP='DELETE' THEN
  DELETE FROM kubernetes_evidence_events_partitioned WHERE scope_key=OLD.scope_key AND sequence=OLD.sequence AND event_at=OLD.event_at;
  RETURN OLD;
 END IF;
 PERFORM versus_ensure_evidence_partition('kubernetes_evidence_events_partitioned',NEW.event_at);
 INSERT INTO kubernetes_evidence_events_partitioned SELECT NEW.*
 ON CONFLICT(scope_key,event_id,event_version,event_at) DO UPDATE SET superseded_seq=EXCLUDED.superseded_seq,observed_at=EXCLUDED.observed_at,payload=EXCLUDED.payload;
 RETURN NEW;
END $$;
