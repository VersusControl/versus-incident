SET LOCAL lock_timeout='2s';
CREATE TABLE kubernetes_evidence_partition_migration (
 singleton BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK(singleton),
 event_sequence BIGINT NOT NULL DEFAULT 0,
 observation_scope TEXT NOT NULL DEFAULT '',
 observation_occurrence TEXT NOT NULL DEFAULT '',
 observation_revision BIGINT NOT NULL DEFAULT 0,
 complete BOOLEAN NOT NULL DEFAULT FALSE
);
INSERT INTO kubernetes_evidence_partition_migration(singleton) VALUES(TRUE);

CREATE TABLE kubernetes_evidence_events_partitioned
 (LIKE kubernetes_evidence_events INCLUDING DEFAULTS,
 PRIMARY KEY(sequence,event_at),
 UNIQUE(scope_key,event_id,event_version,event_at),
 FOREIGN KEY(scope_key) REFERENCES kubernetes_evidence_scopes(scope_key) ON DELETE CASCADE)
 PARTITION BY RANGE(event_at);
CREATE TABLE kubernetes_evidence_observations_partitioned
 (LIKE kubernetes_evidence_occurrence_observations INCLUDING DEFAULTS,
 PRIMARY KEY(scope_key,occurrence_id,revision,observed_at),
 FOREIGN KEY(scope_key) REFERENCES kubernetes_evidence_scopes(scope_key) ON DELETE CASCADE,
 FOREIGN KEY(scope_key,payload_version) REFERENCES kubernetes_evidence_occurrence_versions(scope_key,payload_version))
 PARTITION BY RANGE(observed_at);

CREATE INDEX evidence_partition_event_time ON kubernetes_evidence_events_partitioned(scope_key,event_at DESC,event_id DESC);
CREATE INDEX evidence_partition_event_sequence ON kubernetes_evidence_events_partitioned(scope_key,sequence DESC);
CREATE INDEX evidence_partition_event_versions ON kubernetes_evidence_events_partitioned(scope_key,event_id,sequence DESC);
CREATE INDEX evidence_partition_event_namespace ON kubernetes_evidence_events_partitioned(scope_key,namespace,event_at DESC,event_id DESC);
CREATE INDEX evidence_partition_event_root ON kubernetes_evidence_events_partitioned(scope_key,root_uid,event_at DESC,event_id DESC);
CREATE INDEX evidence_partition_event_uid ON kubernetes_evidence_events_partitioned(scope_key,resource_uid,event_at DESC,event_id DESC);
CREATE INDEX evidence_partition_event_pod ON kubernetes_evidence_events_partitioned(scope_key,pod_uid,event_at DESC,event_id DESC);
CREATE INDEX evidence_partition_event_type ON kubernetes_evidence_events_partitioned(scope_key,event_type,event_at DESC,event_id DESC);
CREATE INDEX evidence_partition_event_rule ON kubernetes_evidence_events_partitioned(scope_key,rule,event_at DESC,event_id DESC);
CREATE INDEX evidence_partition_event_observed ON kubernetes_evidence_events_partitioned(scope_key,event_type,observed_at DESC,event_id DESC);
CREATE INDEX evidence_partition_event_priority ON kubernetes_evidence_events_partitioned(scope_key,event_id,priority DESC,observed_at DESC);
CREATE INDEX evidence_partition_gap_window ON kubernetes_evidence_events_partitioned(scope_key,gap_to DESC,event_id DESC) WHERE event_type='coverage_gap';
CREATE INDEX evidence_partition_issue_filter ON kubernetes_evidence_events_partitioned(scope_key,namespace,kind,root_uid,rule,event_at DESC,event_id DESC) WHERE event_type IN('issue_opened','issue_observed','issue_resolved');
CREATE INDEX evidence_partition_current_type ON kubernetes_evidence_events_partitioned(scope_key,event_type,event_at DESC,event_id DESC) WHERE superseded_seq IS NULL;
CREATE INDEX evidence_partition_current_namespace ON kubernetes_evidence_events_partitioned(scope_key,namespace,event_at DESC,event_id DESC) WHERE superseded_seq IS NULL;
CREATE INDEX evidence_partition_current_root ON kubernetes_evidence_events_partitioned(scope_key,root_uid,event_at DESC,event_id DESC) WHERE superseded_seq IS NULL;
CREATE INDEX evidence_partition_current_break ON kubernetes_evidence_events_partitioned(scope_key,is_break,event_at DESC,event_id DESC) WHERE superseded_seq IS NULL;
CREATE INDEX evidence_partition_observation_snapshot ON kubernetes_evidence_observations_partitioned(scope_key,occurrence_id,revision DESC);
CREATE INDEX evidence_partition_observation_window ON kubernetes_evidence_observations_partitioned(scope_key,observed_at DESC,revision DESC);

CREATE FUNCTION versus_ensure_evidence_partition(parent REGCLASS, at_time TIMESTAMPTZ) RETURNS VOID
LANGUAGE plpgsql AS $$
DECLARE
 month_start TIMESTAMPTZ;
 month_end TIMESTAMPTZ;
 partition_name TEXT;
 parent_name TEXT;
 partition_kind "char";
BEGIN
 SELECT relname,relkind INTO parent_name,partition_kind FROM pg_class WHERE oid=parent;
 IF partition_kind <> 'p' THEN RETURN; END IF;
 month_start := date_trunc('month',at_time AT TIME ZONE 'UTC') AT TIME ZONE 'UTC';
 month_end := (date_trunc('month',at_time AT TIME ZONE 'UTC')+INTERVAL '1 month') AT TIME ZONE 'UTC';
 IF parent_name LIKE '%observations%' THEN
  partition_name := 'evidence_observation_month_'||to_char(month_start AT TIME ZONE 'UTC','YYYYMM');
 ELSE
  partition_name := 'evidence_event_month_'||to_char(month_start AT TIME ZONE 'UTC','YYYYMM');
 END IF;
 IF EXISTS(SELECT 1 FROM pg_inherits i JOIN pg_class c ON c.oid=i.inhrelid WHERE i.inhparent=parent AND c.relname=partition_name) THEN RETURN; END IF;
 PERFORM pg_advisory_xact_lock(hashtextextended(partition_name,0));
 IF NOT EXISTS(SELECT 1 FROM pg_inherits i JOIN pg_class c ON c.oid=i.inhrelid WHERE i.inhparent=parent AND c.relname=partition_name) THEN
  EXECUTE format('CREATE TABLE %I PARTITION OF %s FOR VALUES FROM (%L) TO (%L)',partition_name,parent,month_start,month_end);
  EXECUTE format('ALTER TABLE %I SET (autovacuum_vacuum_scale_factor=0.02)',partition_name);
 END IF;
END $$;

CREATE FUNCTION versus_mirror_evidence_event() RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP='DELETE' THEN
  DELETE FROM kubernetes_evidence_events_partitioned WHERE scope_key=OLD.scope_key AND sequence=OLD.sequence AND event_at=OLD.event_at;
  RETURN OLD;
 END IF;
 PERFORM versus_ensure_evidence_partition('kubernetes_evidence_events_partitioned',NEW.event_at);
 INSERT INTO kubernetes_evidence_events_partitioned SELECT NEW.*
 ON CONFLICT(scope_key,event_id,event_version,event_at) DO UPDATE SET superseded_seq=EXCLUDED.superseded_seq;
 RETURN NEW;
END $$;
CREATE TRIGGER evidence_event_partition_mirror AFTER INSERT OR UPDATE OR DELETE ON kubernetes_evidence_events
 FOR EACH ROW EXECUTE FUNCTION versus_mirror_evidence_event();

CREATE FUNCTION versus_mirror_evidence_observation() RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP='DELETE' THEN
  DELETE FROM kubernetes_evidence_observations_partitioned WHERE scope_key=OLD.scope_key AND occurrence_id=OLD.occurrence_id AND revision=OLD.revision AND observed_at=OLD.observed_at;
  RETURN OLD;
 END IF;
 PERFORM versus_ensure_evidence_partition('kubernetes_evidence_observations_partitioned',NEW.observed_at);
 INSERT INTO kubernetes_evidence_observations_partitioned SELECT NEW.* ON CONFLICT DO NOTHING;
 RETURN NEW;
END $$;
CREATE TRIGGER evidence_observation_partition_mirror AFTER INSERT OR UPDATE OR DELETE ON kubernetes_evidence_occurrence_observations
 FOR EACH ROW EXECUTE FUNCTION versus_mirror_evidence_observation();
