# PostgreSQL storage backend

By default Versus Incident stores incidents and agent state on the local
filesystem (`STORAGE_TYPE=file`). For durable, shared, multi-replica storage,
switch the backend to PostgreSQL.

## Enable Postgres

Set two environment variables:

| Variable       | Description |
|----------------|-------------|
| `STORAGE_TYPE` | Set to `postgres` to use the PostgreSQL backend (default `file`). |
| `POSTGRES_DSN` | Postgres connection string — **required when `STORAGE_TYPE=postgres`**. Keep the password out of source control — set the DSN via env only. |

Example DSN:

```
postgres://versus:your_strong_password@host:5432/versus_incident?sslmode=require
```

## Provision the database and role

When `STORAGE_TYPE=postgres`, Versus runs its schema migrations
**automatically on boot**, so the role in `POSTGRES_DSN` must be able to
`CREATE` tables in the target database. Provision the database and role once as
a Postgres superuser (`psql`) before the first start:

```sql
CREATE DATABASE versus_incident;
CREATE USER versus WITH PASSWORD 'your_strong_password';
GRANT ALL PRIVILEGES ON DATABASE versus_incident TO versus;
GRANT ALL ON SCHEMA public TO versus;   -- required on PostgreSQL 15+
```

Substitute your own database name, username, and a strong password.

- On **PostgreSQL 15+** the `public` schema no longer allows `CREATE` by
  default. That final grant is the usual fix for a startup error like
  `permission denied for schema public` during migration.
- The same failure now also prints this guidance to the container logs at
  startup (detected from the Postgres SQLSTATE), but **this page is the
  canonical reference** — follow the SQL here.

## Kubernetes persisted history

Versus saves what it observes in your Kubernetes clusters (changes, crashes and
issues) so the Timeline, Changes, Crashes and historical Issues pages can show
the past, even for objects that no longer exist. These settings control how that
history uses your database: how many connections it takes, how precisely it
tracks ongoing issues, and how long it is kept.

```yaml
storage:
  postgres:
    max_open_conns: 20
    history_read_concurrency: 8
kubernetes:
  evidence:
    observation_heartbeat: 5m
    retention: "0"
```

### `storage.postgres.max_open_conns`

Environment variable: `POSTGRES_MAX_OPEN_CONNS`. Default: `20`.

The maximum number of database connections Versus uses for saving data:
incidents, agent state and new Kubernetes history. Capping it protects your
Postgres server from being overloaded.

Raise it if you run many clusters or a busy agent and see writes queuing. Lower
it if your database has a small connection limit. Every Versus replica opens its
own connections, so the total load on Postgres is roughly
`replicas × (max_open_conns + history_read_concurrency)`.

### `storage.postgres.history_read_concurrency`

Environment variable: `POSTGRES_HISTORY_READ_CONCURRENCY`. Default: `8`.

History pages read from their own, separate pool of connections. This keeps
people browsing history from slowing down saving, and the other way round. Each
history read must finish within 2 seconds; a read that waits too long for a free
connection returns a retryable error instead of hanging.

Raise it when many people view history at the same time. Lower it to reserve
database capacity for writes.

### `kubernetes.evidence.observation_heartbeat`

Environment variable: `KUBERNETES_EVIDENCE_OBSERVATION_HEARTBEAT`. Default:
`5m`. Accepts `1s` to `24h`.

While an issue stays open and unchanged, Versus re-records it at this interval.
This is what lets history say an issue was still happening at a given time, and
it sets how precise "last seen" is.

A shorter interval gives more precise timelines but writes one row per open issue
every interval, so the database grows faster. A longer interval saves space but
history can only place an ongoing issue to within that interval. Changes to an
issue are always recorded immediately, whatever this setting.

### `kubernetes.evidence.retention`

Environment variable: `KUBERNETES_EVIDENCE_RETENTION`. Default: `"0"`.

How long Kubernetes history is kept, as a duration such as `720h` (30 days).
`0` keeps everything and never deletes or thins history automatically, so
storage keeps growing.

With a retention set, Versus removes history by whole calendar month once a
month is entirely older than the retention period. History can therefore be up
to one month older than the value you set. Older observations of ongoing issues
may also be thinned to one per hour; the start and end of each issue, and every
change to it, are always kept.

Set a retention to bound disk usage. Leave it at `0` when you need the complete
record, for example for audits.

### Memory and file storage

Without Postgres, history is kept in a local SQLite file,
`kubernetes-evidence.sqlite`. Back it up with SQLite's online backup, or stop
Versus first and copy the file together with its WAL file. Programs that embed
Versus and use this history must add
`import _ "github.com/VersusControl/versus-incident/pkg/storage/sqlitedriver"`.
