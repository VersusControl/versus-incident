SET LOCAL lock_timeout='2s';
-- A scope counts as incomplete unless a writer that maintains heads marks it
-- complete, so the column default stays 0 even where it was dropped before.
ALTER TABLE kubernetes_evidence_scopes ALTER COLUMN heads_backfill SET DEFAULT 0;
UPDATE kubernetes_evidence_scopes SET heads_backfill=0 WHERE heads_backfill IS NULL;

-- A writer that maintains heads declares it for its transaction with
-- SET LOCAL versus.evidence_heads='maintained'. Any other journal write, such
-- as one from a replica of an older release or a data-only restore, reopens
-- the scope from the first sequence it inserted or updated, drops the heads
-- of rows it updated or deleted, and a TRUNCATE drops every head and reopens
-- every scope, so the next save looks events up in the journal until the
-- background pass has caught up.
CREATE OR REPLACE FUNCTION versus_evidence_heads_fence() RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
 IF current_setting('versus.evidence_heads',true) IS NOT DISTINCT FROM 'maintained' THEN
  RETURN NULL;
 END IF;
 IF TG_OP='INSERT' THEN
  UPDATE kubernetes_evidence_scopes s SET heads_backfill=LEAST(COALESCE(s.heads_backfill,f.low),f.low)
  FROM (SELECT scope_key,MIN(sequence)-1 AS low FROM fenced_rows GROUP BY scope_key) f WHERE s.scope_key=f.scope_key;
 ELSIF TG_OP='UPDATE' THEN
  -- An update in place may change what a head records about its row, so the
  -- heads of every updated row are dropped and the scope reopened from there.
  DELETE FROM kubernetes_evidence_event_heads h USING fenced_rows f
  WHERE h.scope_key=f.scope_key AND h.event_id=f.event_id AND h.sequence=f.sequence;
  UPDATE kubernetes_evidence_scopes s SET heads_backfill=LEAST(COALESCE(s.heads_backfill,f.low),f.low)
  FROM (SELECT scope_key,MIN(sequence)-1 AS low FROM fenced_rows GROUP BY scope_key) f WHERE s.scope_key=f.scope_key;
 ELSIF TG_OP='TRUNCATE' THEN
  -- Truncating the journal removes every row the heads point at.
  DELETE FROM kubernetes_evidence_event_heads;
  UPDATE kubernetes_evidence_scopes SET heads_backfill=0;
 ELSE
  DELETE FROM kubernetes_evidence_event_heads h USING fenced_rows f
  WHERE h.scope_key=f.scope_key AND h.event_id=f.event_id AND h.sequence=f.sequence;
 END IF;
 RETURN NULL;
END $$;

-- A change to the heads themselves by anything but a heads writer, or by
-- the fence above, makes the scope's heads untrusted until the pass has read
-- its whole journal again.
CREATE OR REPLACE FUNCTION versus_evidence_heads_guard() RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
 IF pg_trigger_depth()>1 OR current_setting('versus.evidence_heads',true) IS NOT DISTINCT FROM 'maintained' THEN
  RETURN NULL;
 END IF;
 IF TG_OP='TRUNCATE' THEN
  UPDATE kubernetes_evidence_scopes SET heads_backfill=0;
 ELSE
  UPDATE kubernetes_evidence_scopes s SET heads_backfill=0 FROM (SELECT DISTINCT scope_key FROM fenced_rows) f WHERE s.scope_key=f.scope_key;
 END IF;
 RETURN NULL;
END $$;

CREATE OR REPLACE FUNCTION versus_install_evidence_heads_fence(journal REGCLASS) RETURNS VOID LANGUAGE plpgsql AS $$
BEGIN
 IF journal IS NULL THEN
  RETURN;
 END IF;
 EXECUTE format('DROP TRIGGER IF EXISTS evidence_heads_fence_insert ON %s', journal);
 EXECUTE format('DROP TRIGGER IF EXISTS evidence_heads_fence_update ON %s', journal);
 EXECUTE format('DROP TRIGGER IF EXISTS evidence_heads_fence_delete ON %s', journal);
 EXECUTE format('CREATE TRIGGER evidence_heads_fence_insert AFTER INSERT ON %s REFERENCING NEW TABLE AS fenced_rows FOR EACH STATEMENT EXECUTE FUNCTION versus_evidence_heads_fence()', journal);
 EXECUTE format('CREATE TRIGGER evidence_heads_fence_update AFTER UPDATE ON %s REFERENCING NEW TABLE AS fenced_rows FOR EACH STATEMENT EXECUTE FUNCTION versus_evidence_heads_fence()', journal);
 EXECUTE format('CREATE TRIGGER evidence_heads_fence_delete AFTER DELETE ON %s REFERENCING OLD TABLE AS fenced_rows FOR EACH STATEMENT EXECUTE FUNCTION versus_evidence_heads_fence()', journal);
 EXECUTE format('DROP TRIGGER IF EXISTS evidence_heads_fence_truncate ON %s', journal);
 EXECUTE format('CREATE TRIGGER evidence_heads_fence_truncate AFTER TRUNCATE ON %s FOR EACH STATEMENT EXECUTE FUNCTION versus_evidence_heads_fence()', journal);
END $$;
SELECT versus_install_evidence_heads_fence(to_regclass('kubernetes_evidence_events'));
SELECT versus_install_evidence_heads_fence(to_regclass('kubernetes_evidence_events_partitioned'));
DROP FUNCTION versus_install_evidence_heads_fence(REGCLASS);

CREATE TRIGGER evidence_heads_guard_insert AFTER INSERT ON kubernetes_evidence_event_heads REFERENCING NEW TABLE AS fenced_rows FOR EACH STATEMENT EXECUTE FUNCTION versus_evidence_heads_guard();
CREATE TRIGGER evidence_heads_guard_update AFTER UPDATE ON kubernetes_evidence_event_heads REFERENCING NEW TABLE AS fenced_rows FOR EACH STATEMENT EXECUTE FUNCTION versus_evidence_heads_guard();
CREATE TRIGGER evidence_heads_guard_delete AFTER DELETE ON kubernetes_evidence_event_heads REFERENCING OLD TABLE AS fenced_rows FOR EACH STATEMENT EXECUTE FUNCTION versus_evidence_heads_guard();
CREATE TRIGGER evidence_heads_guard_truncate AFTER TRUNCATE ON kubernetes_evidence_event_heads FOR EACH STATEMENT EXECUTE FUNCTION versus_evidence_heads_guard();

-- A coverage gap is usually stored at its end. gap_reach is the earliest time
-- any gap of the scope is stored before its end, so a read of gaps that
-- overlap a range can start at the earlier of the range's start and this
-- time, and skip the months before it. Every journal insert keeps it.
ALTER TABLE kubernetes_evidence_scopes ADD COLUMN IF NOT EXISTS gap_reach TIMESTAMPTZ;
UPDATE kubernetes_evidence_scopes s SET gap_reach=(SELECT MIN(e.event_at) FROM kubernetes_evidence_events e
 WHERE e.scope_key=s.scope_key AND e.event_type='coverage_gap' AND e.event_at<e.gap_to);
CREATE OR REPLACE FUNCTION versus_evidence_gap_reach() RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
 UPDATE kubernetes_evidence_scopes s SET gap_reach=LEAST(COALESCE(s.gap_reach,g.reach),g.reach)
 FROM (SELECT scope_key,MIN(event_at) AS reach FROM fenced_rows WHERE event_type='coverage_gap' AND event_at<gap_to GROUP BY scope_key) g
 WHERE s.scope_key=g.scope_key AND (s.gap_reach IS NULL OR s.gap_reach>g.reach);
 RETURN NULL;
END $$;
CREATE OR REPLACE FUNCTION versus_install_evidence_gap_reach(journal REGCLASS) RETURNS VOID LANGUAGE plpgsql AS $$
BEGIN
 IF journal IS NULL THEN
  RETURN;
 END IF;
 EXECUTE format('DROP TRIGGER IF EXISTS evidence_gap_reach_insert ON %s', journal);
 EXECUTE format('CREATE TRIGGER evidence_gap_reach_insert AFTER INSERT ON %s REFERENCING NEW TABLE AS fenced_rows FOR EACH STATEMENT EXECUTE FUNCTION versus_evidence_gap_reach()', journal);
END $$;
SELECT versus_install_evidence_gap_reach(to_regclass('kubernetes_evidence_events'));
SELECT versus_install_evidence_gap_reach(to_regclass('kubernetes_evidence_events_partitioned'));
DROP FUNCTION versus_install_evidence_gap_reach(REGCLASS);
