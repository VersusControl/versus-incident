# Migration to v1.4.37

v1.4.37 has one optional step: remove unused indexes from the Kubernetes
history table on PostgreSQL. You run it once, by hand, in any SQL client.

## Remove unused Kubernetes history indexes

### Who it is for

Installations that use [PostgreSQL storage](/configuration/postgres-storage.md)
with the Kubernetes connector. Installations using file storage need nothing.

The step applies to new installations too: a fresh database also creates these
indexes.

### Why

The Kubernetes history table carries 10 indexes that no query uses. Every save
still updates them, and on a busy installation they take several GB. On one
production installation, a single month of history had 9.2 GB of indexes, of
which 4.7 GB were these.

Removing them cuts write load and disk reads. Versus works the same with or
without them, so the step is optional but recommended.

### When

Pick a quiet period. Each removal briefly locks the history table, and every
month inside it, and waits at most 2 seconds for that lock. While it waits,
history saves and history page reads wait too. If it times out, run it again
later.

You run the step once. New months never get these indexes again.

### Steps

Connect any SQL client (`psql`, Navicat, …) to the Versus database.

Before you start, check that the history table is already split by month:

```sql
SELECT relkind FROM pg_class WHERE oid = 'kubernetes_evidence_events'::regclass;
```

It must return `p`. If it returns `r`, Versus is still converting its history in
the background after the upgrade. The steps would then remove the wrong indexes,
and the ones to remove would come back when the conversion finishes. Wait until
the query returns `p`; the conversion finishes on its own. Then run the steps.

1. **List what to remove.** The index names depend on when your installation was
   set up, so this query returns only the ones your database has:

   ```sql
   SELECT format('DROP INDEX IF EXISTS %I;', c.relname) AS statement
   FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid
   WHERE i.indrelid = 'kubernetes_evidence_events'::regclass
     AND c.relname IN ('evidence_partition_event_time','evidence_partition_event_rule','evidence_partition_event_priority','evidence_partition_event_uid','evidence_partition_event_pod','evidence_partition_current_type','evidence_partition_current_namespace','evidence_partition_current_root','evidence_partition_current_break','kubernetes_partition_termination_markers','kubernetes_history_termination_markers','kubernetes_evidence_current_type','kubernetes_evidence_current_namespace','kubernetes_evidence_current_root','kubernetes_evidence_current_break')
   ORDER BY 1;
   ```

   No rows means there is nothing to do.

2. **Remove them.** With autocommit on, so each statement commits on its own,
   run:

   ```sql
   SET lock_timeout = '2s';
   ```

   Then run each statement from step 1, one at a time. If a statement fails
   with a lock timeout, run it again. `IF EXISTS` makes every statement safe to
   repeat.

3. **Check the result.** Run the query from step 1 again. It should return no
   rows.

### Optional: compare index size

Run this before step 2 and after step 3 to see how much space was freed:

```sql
SELECT pg_size_pretty(sum(pg_indexes_size(inhrelid)))
FROM pg_inherits
WHERE inhparent = 'kubernetes_evidence_events'::regclass;
```

The history table is split into one partition per month, and the indexes are
stored in those partitions, so this query adds up their sizes. On a table that
is not split by month the query returns nothing; use this one instead:

```sql
SELECT pg_size_pretty(pg_indexes_size('kubernetes_evidence_events'::regclass));
```

On a table split by month, this second query reads `0 bytes`, because the table
itself stores no index data.

### Undo

Not expected to be needed: an index can be restored by recreating it from the
schema of the release you run.
