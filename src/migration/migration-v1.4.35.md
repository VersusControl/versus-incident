# Migration to v1.4.35

Kubernetes history is now kept for **7 days** by default. If you want to keep
more, set `retention` before you upgrade. This page lists what to do first and
what to expect afterwards.

## Before you upgrade

- If you are upgrading from a version earlier than v1.4.34, keep the
  [startup probe](/migration/migration-v1.4.34.md#allow-time-for-the-first-start)
  recommended for v1.4.34.
- Decide how much Kubernetes history to keep; see
  [Keep more history](#keep-more-history). With a rolling update, deploy
  v1.4.35 with `retention: "0"` and set the value you want once every replica
  runs v1.4.35. Otherwise, set it in the configuration you deploy v1.4.35 with.

## What happens on first start

- The first start takes about 3 seconds on a cluster with about 700,000 history
  rows. A background pass then runs, about 13 seconds for the same rows.
- While the pass runs, the log shows this line at most every 30 seconds:

  ```text
  storage: evidence heads backfill progress sequence=… pending_scopes=…
  ```

  The pass is finished when this line reports `pending_scopes=0`:

  ```text
  storage: evidence heads complete for a scope; its saves no longer search the journal pending_scopes=0
  ```

- Saves are slower until the pass finishes. Reads are not affected.
- Rolling updates are supported: v1.4.34 and v1.4.35 replicas can run together.
  A v1.4.34 replica that restarts reads the same configuration, so keep
  `retention: "0"` until every replica runs v1.4.35, then set the value you
  want (`"168h"` for the 7-day default). Do not edit, truncate or drop history
  partitions by hand during the update.

## New and changed settings

| Key | Environment variable | Helm value | Default | Change |
|-----|----------------------|------------|---------|--------|
| `kubernetes.evidence.save_chunk` | `KUBERNETES_EVIDENCE_SAVE_CHUNK` | `kubernetes.evidence.saveChunk` | `500` | New. How many records Versus saves at a time on Postgres. Accepts `1` to `1024`; any other value uses `500`. |
| `kubernetes.evidence.retention` | `KUBERNETES_EVIDENCE_RETENTION` | `kubernetes.evidence.retention` | `"168h"` (7 days) | The default was `"0"` (keep everything). |

With the new default, history older than 7 days is deleted by whole calendar
month, starting after the upgrade. Pages keep at least the last 7 days and up to
about 5 weeks. For the month-by-month example, see
[How long history is kept](/configuration/postgres-storage.md#how-long-history-is-kept).

### Keep more history

Set a longer value, or `"0"` to keep everything. Durations are in hours; there
is no day unit. For example, `"720h"` keeps at least 30 days and `"9504h"` at
least 13 months.

In `config.yaml`:

```yaml
kubernetes:
  evidence:
    retention: "9504h"   # or "0" to keep everything
```

With an environment variable:

```bash
KUBERNETES_EVIDENCE_RETENTION=9504h
```

With Helm values:

```yaml
kubernetes:
  evidence:
    retention: "9504h"
```

History that was already deleted does not come back when you raise the value.
For disk use at each setting, see
[Disk use](/configuration/postgres-storage.md#disk-use).

## What you will notice

- **Old page links stop working.** Page links saved before the upgrade for
  Issues, Workloads, Nodes, node Pods, Helm, GitOps, Changes, Timeline and
  Crashes no longer work. With retention on (the default), a history page link
  can also stop working during normal running, after old history is deleted. In
  both cases API clients get `400 cursor_expired` and must start again from the
  first page. For what each history page shows, see
  [When a page link stops working](/configuration/postgres-storage.md#when-a-page-link-stops-working).
- **History totals show `1,000+`.** History lists count up to 1,000 and show
  `1,000+` beyond that.
- **Save status notices.** When history cannot be saved, pages keep showing
  saved rows with "History is temporarily not saving" or "History could not
  save the latest data". A database problem is no longer shown as a credential
  problem.
- **Extra history rows, once.** On Postgres, the first save after the upgrade
  adds one row for each existing change record, up to roughly 52,000 rows on a
  large cluster. The next save adds nothing. No action is needed.

## Errors to watch

| Notice or log line | Meaning | What to do |
|--------------------|---------|------------|
| "History is temporarily not saving — showing data from *time*" (`history_save_delayed`) | The latest capture is not saved yet; the database is slow or unreachable, or a save timed out. | Check database health and free connections. History catches up on its own. |
| "History could not save the latest data — showing data from *time*" (`history_save_rejected`) | The latest capture can never be saved. Collection continues and the missed interval shows as a gap. | Find the reason in `kubernetes: history capture refused before saving reason=…` or `kubernetes: history store rejected a capture; collection continues and the next capture records the gap reason=<reason>`. Restarting does not help. |
| `400 cursor_expired` | The page link is no longer valid: it was saved before the upgrade, or retention deleted old history since it was issued. | Start again from the first page. |
| `storage: evidence save part timed out; retrying it alone at N records` | A save was too slow. Occasional lines are fine. | If frequent, check database load and lower [`save_chunk`](/configuration/postgres-storage.md#large-clusters). |
| A startup line that contains `storage: invalid evidence retention duration` | Versus did not start: `retention` is not a valid duration. | Use hours, such as `"168h"`, or `"0"`. Day units such as `7d` are rejected. |

For every notice and log line, see
[When history is not saving](/configuration/postgres-storage.md#when-history-is-not-saving).
