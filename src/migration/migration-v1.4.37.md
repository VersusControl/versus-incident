# Migration to v1.4.37

v1.4.37 puts less load on the database when it saves Kubernetes history. On
PostgreSQL, the upgrade needs the `btree_gist` extension and changes some
history indexes on first start. After the upgrade, Kubernetes issues that
stayed open after their resource recovered are closed.

There is also one optional step: remove unused indexes from the Kubernetes
history table on PostgreSQL. You run it once, by hand, in any SQL client.

Installations using file storage need nothing before the upgrade; the changes
in [What you will notice](#what-you-will-notice) apply to them too.

## Before you upgrade

- Make sure the
  [startup probe](/migration/migration-v1.4.34.md#allow-time-for-the-first-start)
  recommended for v1.4.34 is in place. The index builds below run before
  Versus answers `/healthz`.
- On PostgreSQL, make sure the Versus role can add the `btree_gist` extension.

### Allow the `btree_gist` extension

The new history indexes need `btree_gist`. The extension ships with
PostgreSQL. On PostgreSQL 13 and later it is a trusted extension, so the role
in `POSTGRES_DSN` adds it on first start without being a superuser, as long as
it has `CREATE` on the database. The `GRANT ALL PRIVILEGES ON DATABASE` from
[Provision the database and role](/configuration/postgres-storage.md#provision-the-database-and-role)
includes it.

If the role can't add the extension, the first start stops with an error that
names `047_kubernetes_btree_gist.sql`.

**Check.** Connect to the Versus database as the Versus role and run these
read-only queries:

```sql
-- Already installed? One row means there is nothing to do.
SELECT extversion FROM pg_extension WHERE extname = 'btree_gist';

-- Available and trusted on this server?
SELECT v.version, v.trusted
FROM pg_available_extension_versions v
JOIN pg_available_extensions a ON a.name = v.name AND a.default_version = v.version
WHERE v.name = 'btree_gist';

-- Can this role create objects in the database?
SELECT has_database_privilege(current_user, current_database(), 'CREATE');
```

You are ready when the first query returns a row, or the second returns
`trusted = t` and the third returns `t`.

**Fix.** Do one of these:

- Give the Versus role `CREATE` on the database (as the database owner or a
  superuser; use your own database and role names):

  ```sql
  GRANT CREATE ON DATABASE versus_incident TO versus;
  ```

- Or add the extension yourself, connected to the Versus database as an admin
  role:

  ```sql
  CREATE EXTENSION IF NOT EXISTS btree_gist;
  ```

  Versus then finds it installed and goes on. Leave it in the default schema
  (`public`), where the Versus role finds it.

If the second check query returns no row, the server does not offer the
extension. Managed PostgreSQL services that keep an allow-list of extensions
must have `btree_gist` on it; for example, Azure Database for PostgreSQL
flexible server lists them in its `azure.extensions` server parameter. Add it
there, then run the checks again.

## What happens on first start

This section applies to PostgreSQL storage. Versus applies the changes below
on the first start of v1.4.37, once, before it answers `/healthz`. With several
replicas, one applies them and the others wait. Each change logs one line when
it is done:

```text
storage: migration 048_kubernetes_occurrence_scoped_spans.sql applied in …s
```

### New indexes

Versus builds these indexes with `CREATE INDEX CONCURRENTLY`. A build does not
block history saves or reads, but it reads the whole table, so it takes longer
on a large history.

| Index | Table |
|---|---|
| `kubernetes_evidence_open_issue` | `kubernetes_evidence_issues` |
| `kubernetes_occurrence_open_span_root` | `kubernetes_evidence_occurrence_read_versions` |
| `kubernetes_occurrence_open_span_namespace` | `kubernetes_evidence_occurrence_read_versions` |
| `kubernetes_occurrence_open_span_scope` | `kubernetes_evidence_occurrence_read_versions` |
| `kubernetes_occurrence_visible_span_scope` | `kubernetes_evidence_occurrence_read_versions` |

A concurrent build also waits for transactions that were already running when
it started. A long-running query or a session left idle in a transaction holds
it up.

If Versus stops during a build, the next start drops the unfinished index and
builds it again.

### How long it takes

As a guide, on a database with about 275,000 read versions and 175,000 issues,
on a 2 vCPU / 2 GiB PostgreSQL instance:

| Change | Time |
|---|---|
| `kubernetes_evidence_open_issue` | 0.47 s |
| `btree_gist` extension | 0.05 s |
| The four `kubernetes_occurrence_*_span_*` indexes | 5.47 s, 4.28 s, 2.81 s and 2.01 s |
| Index removals | under 0.01 s each |

That is about 15 s in all; `/healthz` answered 18 s after start. Larger
tables take longer, so keep the startup probe from
[Before you upgrade](#before-you-upgrade) in place for the upgrade.

### Removed indexes

Versus then removes indexes that no query uses. Each name exists only on some
installations, depending on when they were set up; a name that is not there is
skipped.

| Index | Table |
|---|---|
| `evidence_partition_current_namespace`, `kubernetes_evidence_current_namespace` | history events |
| `evidence_partition_issue_filter`, `kubernetes_history_issue_filter` | history events |
| `evidence_partition_event_versions`, `kubernetes_evidence_event_versions` | history events |
| `evidence_partition_observation_snapshot`, `kubernetes_occurrence_snapshot` | occurrence observations |

An index on a table split by month can't be removed concurrently, so each
removal locks the table and every month inside it. Versus waits at most
1 second for that lock. While it waits, history saves and reads of that table
wait too. If a long read holds the table, Versus pauses 1 second and tries
again, up to 60 times. If every try fails, the start stops with one of these
errors:

```text
unread kubernetes evidence events indexes stayed locked by other sessions; start again when long reads have finished
unread occurrence observations indexes stayed locked by other sessions; start again when long reads have finished
```

Nothing is removed in that case. Start Versus again once the long reads have
finished.

### Check progress

While Versus builds an index, this read-only query shows how far it is:

```sql
SELECT p.pid, t.relname AS table_name, i.relname AS index_name, p.phase,
       p.blocks_done, p.blocks_total, p.tuples_done, p.tuples_total
FROM pg_stat_progress_create_index p
JOIN pg_class t ON t.oid = p.relid
LEFT JOIN pg_class i ON i.oid = p.index_relid;
```

No rows means no build is running. A `phase` that starts with `waiting for`
means the build waits for older transactions. This query lists them, oldest
first:

```sql
SELECT pid, state, wait_event_type, now() - xact_start AS open_for, left(query, 80) AS query
FROM pg_stat_activity
WHERE datname = current_database() AND xact_start IS NOT NULL
ORDER BY xact_start;
```

## What you will notice

### Long-open issues close

Before v1.4.37, a Kubernetes issue could stay open long after its resource
recovered. v1.4.37 closes these issues after the upgrade:

- An issue counts as long-open only when an earlier saved capture had already
  missed it by more than 3 capture intervals. The capture interval is
  1 minute.
- Its close time is one capture interval after it was last seen, not the time
  of the upgrade.
- Each save closes at most 2,000 of them per cluster. A larger backlog closes
  over the next saves. Issues whose resource recovered recently keep closing
  at once.
- These closures are not counted as recoveries in break counts and do not
  appear in the change timeline. The issue itself shows as closed.
- On PostgreSQL, if the close time falls in a month that history retention
  has already removed, the issue is closed on the issue record itself and no
  history record is written for the closure.

Expect the count of open Kubernetes issues to fall over the first minutes
after the upgrade. You don't need to do anything.

After that, a pause between saves does not cause catch-up closures, for
example after a restart or a save backoff. An issue whose resource recovered
during the pause closes in the normal way, at the time the next save covers.

### Incomplete evidence no longer holds every issue open

When Versus has incomplete evidence for one resource, it keeps only that
resource's issues open. Issues of other resources in the cluster close as
usual. Before, one such resource stopped every issue in the cluster from
closing. A capture that is incomplete as a whole still closes nothing.

### Smaller saves after a failed save

When a history save times out or the database fails it, Versus does not retry
that save. It logs a warning that starts with:

```text
kubernetes: cluster "<cluster>" history is not saving (storage unavailable)
```

It waits before the next capture, 1 second at first and up to 5 minutes after
repeated failures, and records the missed stretch as a gap. Each failed save
in a row halves how many issue changes the next save may apply, down to 100.
Changes over that limit wait for later saves. After a save succeeds, the limit
returns to normal.

### Timeline totals count only listed rows

The timeline total and the change, crash and break counts now include only the
rows the timeline list can show. After the upgrade, a cluster's timeline total
can be a few lower than in v1.4.36 for the same window. Nothing was lost: the
earlier count also included rows the list never showed. When Versus can't
check every counted row, the timeline marks the count as unknown and shows no
number, instead of a guess.

## Rolling back

v1.4.36 runs on the upgraded database. To roll back, deploy v1.4.36; the new
indexes and the `btree_gist` extension don't affect it. Its history pages read
the closures described in [Long-open issues close](#long-open-issues-close)
without error, and leave them out of the timeline.

During a rolling update, replicas on v1.4.36 and v1.4.37 both serve history,
as long as every replica uses the same connector configuration, with the same
cluster endpoints. The history access check includes the endpoint.

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

Pick a quiet period. The removal locks the history table, and every month
inside it, until it finishes. It waits at most 2 seconds for that lock. While it
waits, history saves and history page reads wait too, so they can pause for up
to 2 seconds.

Once it has the lock, the removal is quick. On a test month of 120,000 records
it held the lock for 30 to 60 milliseconds. A larger table takes longer, because
the files of each index are deleted, but no data is read or copied. We have not
measured it on a production-sized table.

If it can't get the lock within 2 seconds, it removes nothing. Run it again
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

1. **See what will be removed.** The index names depend on when your
   installation was set up, so this query lists only the ones your database
   has. It changes nothing:

   ```sql
   SELECT c.relname AS index_name
   FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid
   WHERE i.indrelid = 'kubernetes_evidence_events'::regclass
     AND c.relname IN ('evidence_partition_event_time','evidence_partition_event_rule','evidence_partition_event_priority','evidence_partition_event_uid','evidence_partition_event_pod','evidence_partition_current_type','evidence_partition_current_namespace','evidence_partition_current_root','evidence_partition_current_break','kubernetes_partition_termination_markers','kubernetes_history_termination_markers','kubernetes_evidence_current_type','kubernetes_evidence_current_namespace','kubernetes_evidence_current_root','kubernetes_evidence_current_break')
   ORDER BY 1;
   ```

   No rows means there is nothing to do. The upgrade itself already removes
   `evidence_partition_current_namespace` and
   `kubernetes_evidence_current_namespace`, so after it they no longer appear
   here.

2. **Remove them.** Run this block once, with autocommit on. It removes the
   indexes from step 1 and nothing else:

   ```sql
   DO $$
   DECLARE
     index_name text;
     removed int := 0;
   BEGIN
     IF (SELECT relkind FROM pg_class WHERE oid = 'kubernetes_evidence_events'::regclass) <> 'p' THEN
       RAISE EXCEPTION 'kubernetes_evidence_events is not split by month yet; wait for the conversion to finish and run this again';
     END IF;
     PERFORM set_config('lock_timeout', '2s', true);
     FOR index_name IN
       SELECT c.relname FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid
       WHERE i.indrelid = 'kubernetes_evidence_events'::regclass
         AND c.relname IN ('evidence_partition_event_time','evidence_partition_event_rule','evidence_partition_event_priority','evidence_partition_event_uid','evidence_partition_event_pod','evidence_partition_current_type','evidence_partition_current_namespace','evidence_partition_current_root','evidence_partition_current_break','kubernetes_partition_termination_markers','kubernetes_history_termination_markers','kubernetes_evidence_current_type','kubernetes_evidence_current_namespace','kubernetes_evidence_current_root','kubernetes_evidence_current_break')
       ORDER BY 1
     LOOP
       EXECUTE format('DROP INDEX IF EXISTS %I', index_name);
       RAISE NOTICE 'removed %', index_name;
       removed := removed + 1;
     END LOOP;
     RAISE NOTICE 'removed % indexes', removed;
   END $$;
   ```

   The block runs as one transaction. The first removal waits at most 2 seconds
   for the table lock. The other removals already hold that lock, so they don't
   wait.

   If the wait times out, the block fails with the error
   `canceling statement due to lock timeout` and removes nothing. Run it again
   later. If the table is not split by month yet, the block stops with an error
   and removes nothing.

   The block reports each index it removes, then the total, as notices. `psql`
   prints them in its output; Navicat shows them in the Message tab.
   `removed 0 indexes` means there was nothing to do. Running the block again is
   safe.

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
