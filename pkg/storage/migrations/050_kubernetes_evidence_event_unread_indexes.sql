-- An index on a partitioned table cannot be dropped concurrently, and a plain
-- drop locks every partition. Each attempt waits at most one second for the
-- lock, so saves and reads queue behind it only briefly, and tries again
-- after a pause when a long read holds a partition.
DO $$
BEGIN
 FOR attempt IN 1..60 LOOP
  BEGIN
   PERFORM set_config('lock_timeout','1s',true);
   DROP INDEX IF EXISTS evidence_partition_current_namespace, evidence_partition_issue_filter, evidence_partition_event_versions, kubernetes_evidence_current_namespace, kubernetes_history_issue_filter, kubernetes_evidence_event_versions;
   RETURN;
  EXCEPTION WHEN lock_not_available THEN
   PERFORM pg_sleep(1);
  END;
 END LOOP;
 RAISE EXCEPTION 'unread kubernetes evidence events indexes stayed locked by other sessions; start again when long reads have finished';
END
$$;
