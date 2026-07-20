# Migrating to v1.4.13

This guide explains how Versus Incident v1.4.13 stores incidents and how to
run the one-time PostgreSQL migration that comes with it. Most deployments
need **no manual steps** — the app upgrades its own schema on boot. The only
people who run a script are operators on an **existing Postgres deployment
with incident history from before the upgrade**.

If you use the `file` storage backend, or you're setting up Postgres fresh, there is nothing to run.

## Upgrading

```bash
# Docker
docker pull ghcr.io/versuscontrol/versus-incident:v1.4.13

# Helm
helm repo update
helm upgrade versus-incident oci://ghcr.io/versuscontrol/charts/versus-incident \
  --version 1.4.13
```

Restart the service to apply the changes. The embedded `008_incident_columns`
migration runs on boot and adds the new columns automatically. For general
Postgres setup and permissions, see
[PostgreSQL storage backend](../configuration/postgres-storage.md).

For any issues with the migration, please
[open an issue](https://github.com/VersusControl/versus-incident/issues) on
GitHub.


## Key Changes in v1.4.13

### 1. Each incident property now has its own database column

Previously the Postgres backend stored a whole incident as one big JSON blob
in a column called `data`. As of v1.4.13, every property lives in its own
dedicated column on the `vs_incidents` table — `title`, `source`, `service`,
`origin`, `resolved`, `resolved_at`, `channels`, on-call / notify status,
assignments, `content`, and so on.

New code reads and writes **only these columns**. It no longer touches the old
`data` blob.

### 2. The incident list is now fast

Because the fields are real columns, the incident list API loads a **bounded,
newest-first page** straight from the database instead of pulling the entire
table into memory on every page load. The home-page counter is now a cheap
separate query, and it counts only **unresolved (open)** incidents.

### 3. The new columns are added automatically

On upgrade, Versus runs an embedded migration (`008_incident_columns`) on boot
that adds the new columns for you. **Every new incident immediately uses
them.** You do not run anything for this part — it just happens.

## Who Needs to Act

| Your setup | What to do |
|------------|-----------|
| **`file` storage backend** | **Nothing.** The file backend already stores every field on disk. This change is Postgres-only. |
| **Fresh Postgres install** | **Nothing.** The columns are created automatically on boot and there is no old data to move. |
| **Existing Postgres with incident history** | **Run the one-time script once** (below) to move your pre-upgrade incidents into the new columns and drop the old `data` column. |

> If you're in the third row and skip the script, nothing crashes — new
> incidents work fine. But **incidents created before the upgrade will render
> empty** until you migrate them, and the unused `data` column keeps taking up
> space.

## How to Migrate (existing Postgres only)

You'll run the script
[`scripts/postgres/migrate_incident_columns.sql`](https://github.com/VersusControl/versus-incident/blob/main/scripts/postgres/migrate_incident_columns.sql)
**once**. In one sentence: it copies every field out of the old JSON into the
new columns, then drops the `data` column. It's wrapped in a transaction, so
it either fully succeeds or changes nothing.

### Step 1 — Back up the database first

The last thing the script does is **drop** the `data` column. That is
**irreversible**. Take a backup before you start so you can restore if
anything goes wrong:

```bash
pg_dump "$POSTGRES_DSN" > versus_incident_backup.sql
```

`$POSTGRES_DSN` is the same connection string your app uses. Keep the backup
file somewhere safe until you've confirmed the migration worked.

### Step 2 — Pick a maintenance window

The script rewrites every incident row, so run it during a quiet period. For
most deployments this is quick, but it's good practice to migrate when traffic
is low.

### Step 3 — Run the script once

Point it at the **same database** as your `POSTGRES_DSN`. Either of these
works — pick whichever you're comfortable with.

From your shell:

```bash
psql "$POSTGRES_DSN" -f scripts/postgres/migrate_incident_columns.sql
```

Or from inside a `psql` session already connected to the database:

```sql
\i scripts/postgres/migrate_incident_columns.sql
```

The script backfills every column from the old JSON, then drops the `data`
column — all inside a single transaction (all-or-nothing).

### Step 4 — What success looks like

A successful run **commits without any error**.

If you run it a **second** time, it will intentionally fail at the
`DROP COLUMN` step with an error saying the `data` column doesn't exist. **That
is expected** — it's the "already migrated" signal, not a problem. It means the
work was already done the first time.

### Step 5 — Verify

- Open the UI. Your **old incidents should now show their details** (title,
  service, status) instead of appearing empty.
- Optionally, run a quick sanity check in `psql`:

  ```sql
  SELECT count(*) FROM vs_incidents WHERE title IS NOT NULL;
  ```

  A non-zero count means your history was backfilled into the new columns.

### If you skip this

Nothing breaks and new incidents work normally, but **pre-upgrade incidents
render empty** and the unused `data` column lingers. You can run the script
later whenever you're ready.

### Rollback

The `DROP COLUMN` in the final step is irreversible, so there is no
undo query. To roll back, **restore the backup you took in Step 1**:

```bash
psql "$POSTGRES_DSN" < versus_incident_backup.sql
```
