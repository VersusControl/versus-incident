# Migration to v1.4.34

## Kubernetes history is prepared at startup

Versus v1.4.34 stores Kubernetes history (Timeline, Changes, Crashes and
historical Issues) in a faster indexed form. The first time v1.4.34 starts on a
Postgres database that already holds history from an earlier version, it
converts that history before serving it:

```text
storage: evidence read-model conversion started pending_scopes=1
storage: evidence read-model conversion complete batches=154 duration_s=205
```

If conversion does not finish within two minutes, Versus starts anyway and
continues in the background. Until a cluster's history is converted, its history
pages return a retryable `history_upgrading` status. Clusters with no earlier
history, and installations using file storage, need no conversion.

## The `read_models` setting is retired

Remove these from your configuration; they are ignored:

- `kubernetes.evidence.read_models` in `config.yaml`
- `KUBERNETES_EVIDENCE_READ_MODELS` environment variable
- `kubernetes.evidence.readModels` in Helm values

## Allow time for the first start

The first start applies new database migrations and converts existing history,
before Versus answers `/healthz`. On a large history this can take several
minutes. Make sure Kubernetes does not restart the Pod during that time, or
every restart begins the work again. Either raise `initialDelaySeconds` on the
readiness and liveness probes for the upgrade, or add a startup probe:

```yaml
startupProbe:
  httpGet:
    path: /healthz
    port: 3000
  periodSeconds: 10
  failureThreshold: 90   # allows up to 15 minutes
```

## Optional: clear old Kubernetes history

Clear the old history instead of converting it when it is large, when you do not
need it, or when conversion cannot make progress. Signs that conversion is stuck:

- The startup log shows `conversion deferred to background batches=0`.
- `storage: evidence read-model backfill deferred` repeats every minute.
- A capture log shows `stage=journal reason=timeout`, and the Kubernetes pages
  report that the connector credential cannot be resolved even though the
  credential is valid.

### What is removed

| Removed | Kept |
|---------|------|
| Kubernetes Timeline, Changes and Crashes history | Incidents, analyses and on-call state |
| Historical Kubernetes Issues and their occurrences | Agent patterns, services and settings |
| Saved history permissions and checkpoints | Kubernetes connector and cluster configuration |

Live Kubernetes pages keep working. New history starts with the next capture,
usually within a minute of Versus starting.

### Steps

1. **Back up the database.** Clearing history cannot be undone.
2. **Run v1.4.34 once** so its migrations are applied. The startup log reaches
   `conversion started`, or the Kubernetes pages show `history_upgrading`.
3. **Stop every Versus replica,** for example
   `kubectl -n <namespace> scale deployment/versus-incident --replicas=0`.
   Clearing history while Versus is writing to it fails or is undone.
4. **Run the SQL below** against the Versus database as the Versus database user.
5. **Start Versus** and scale back to your usual replica count.

```sql
BEGIN;
SET LOCAL lock_timeout = '10s';

-- Empty every Kubernetes history table together, including partitioned
-- tables and copies kept from earlier upgrades.
DO $$
DECLARE
  targets text;
BEGIN
  SELECT string_agg(format('%I.%I', n.nspname, c.relname), ', ')
    INTO targets
  FROM pg_class c
  JOIN pg_namespace n ON n.oid = c.relnamespace
  WHERE n.nspname = current_schema()
    AND c.relkind IN ('r', 'p')
    AND NOT c.relispartition
    AND c.relname LIKE 'kubernetes\_evidence\_%'
    AND c.relname <> 'kubernetes_evidence_partition_migration';
  IF targets IS NOT NULL THEN
    EXECUTE 'TRUNCATE TABLE ' || targets;
  END IF;

  -- Restart any unfinished table-layout upgrade from the beginning.
  IF to_regclass('kubernetes_evidence_partition_migration') IS NOT NULL THEN
    UPDATE kubernetes_evidence_partition_migration
       SET event_sequence = 0,
           observation_scope = '',
           observation_occurrence = '',
           observation_revision = 0
     WHERE singleton AND NOT complete;
  END IF;
END $$;

COMMIT;
```

`TRUNCATE` returns the disk space immediately; no `VACUUM` is needed. If the
statement fails with a lock timeout, a Versus replica is still running. Stop it
and run the SQL again.

### Check the result

Before starting Versus, every history table should be empty:

```sql
SELECT relname, n_live_tup
FROM pg_stat_user_tables
WHERE relname LIKE 'kubernetes\_evidence\_%'
ORDER BY relname;
```

After Versus starts, the log should not show `conversion started`, and once the
first capture completes the cluster's history is ready:

```sql
SELECT scope_key, read_model_ready FROM kubernetes_evidence_scopes;
-- read_model_ready = t
```
