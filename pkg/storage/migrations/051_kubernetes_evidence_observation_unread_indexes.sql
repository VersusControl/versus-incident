-- An index on a partitioned table cannot be dropped concurrently, and a plain
-- drop locks every partition. Each attempt waits at most one second for the
-- lock, so saves and reads queue behind it only briefly, and tries again
-- after a pause when a long read holds a partition.
DO $$
BEGIN
 FOR attempt IN 1..60 LOOP
  BEGIN
   PERFORM set_config('lock_timeout','1s',true);
   DROP INDEX IF EXISTS evidence_partition_observation_snapshot, kubernetes_occurrence_snapshot;
   RETURN;
  EXCEPTION WHEN lock_not_available THEN
   PERFORM pg_sleep(1);
  END;
 END LOOP;
 RAISE EXCEPTION 'unread occurrence observations indexes stayed locked by other sessions; start again when long reads have finished';
END
$$;
