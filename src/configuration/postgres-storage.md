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

## Kubernetes history

Versus saves what it sees in your Kubernetes clusters (changes, crashes and
issues), so the Timeline, Changes, Crashes and historical Issues pages can show
the past, even for objects that no longer exist. On Postgres, the settings below
control how many connections history uses, how precisely it tracks ongoing
issues, and how long it is kept.

### Settings

| Key | Environment variable | Default | What it does |
|-----|----------------------|---------|--------------|
| `storage.postgres.max_open_conns` | `POSTGRES_MAX_OPEN_CONNS` | `20` | Most connections each Versus replica uses to save incidents, agent state and Kubernetes history. Accepts `1` to `1024`. |
| `storage.postgres.history_read_concurrency` | `POSTGRES_HISTORY_READ_CONCURRENCY` | `8` | Connections each replica keeps apart for reading history pages, so browsing history does not slow saving. Accepts `1` to `1024`. |
| `kubernetes.evidence.observation_heartbeat` | `KUBERNETES_EVIDENCE_OBSERVATION_HEARTBEAT` | `5m` | How often an open, unchanged issue is recorded as still happening. Shorter gives more precise timelines and uses more disk. Changes to an issue are always recorded at once. Accepts `1s` to `24h`. |
| `kubernetes.evidence.retention` | `KUBERNETES_EVIDENCE_RETENTION` | `"168h"` (7 days) | How long history is kept. `"0"` keeps everything. See [How long history is kept](#how-long-history-is-kept). |
| `kubernetes.evidence.save_chunk` | `KUBERNETES_EVIDENCE_SAVE_CHUNK` | `500` | Most records Versus saves at a time. Postgres only. Accepts `1` to `1024`. See [Large clusters](#large-clusters). |

- An environment variable overrides the same key in `config.yaml`.
- For `max_open_conns` and `history_read_concurrency`, a value outside `1` to
  `1024` is ignored: an environment value falls back to the `config.yaml` value,
  and a `config.yaml` value falls back to the default.
- For `save_chunk`, any value outside `1` to `1024` uses `500`.
- Every replica opens its own connections. Make sure Postgres
  `max_connections` allows `replicas × (max_open_conns + history_read_concurrency)`
  on top of your other clients.

### Configure

In `config.yaml`:

```yaml
storage:
  postgres:
    max_open_conns: 20
    history_read_concurrency: 8
kubernetes:
  evidence:
    observation_heartbeat: 5m
    retention: "168h"
    save_chunk: 500
```

With environment variables:

```bash
POSTGRES_MAX_OPEN_CONNS=20
POSTGRES_HISTORY_READ_CONCURRENCY=8
KUBERNETES_EVIDENCE_OBSERVATION_HEARTBEAT=5m
KUBERNETES_EVIDENCE_RETENTION=168h
KUBERNETES_EVIDENCE_SAVE_CHUNK=500
```

With Helm values:

```yaml
storage:
  postgres:
    maxOpenConns: 20
    historyReadConcurrency: 8
kubernetes:
  evidence:
    observationHeartbeat: 5m
    retention: "168h"
    saveChunk: 500
```

Restart Versus after a change. Quote `retention` in YAML (`"168h"`, `"0"`).

If a value is invalid, Versus stops at startup with a log line that contains one
of these messages:

| Message | What to do |
|---------|------------|
| `storage: evidence observation heartbeat must be between 1s and 24h` | Set `observation_heartbeat` to a duration from `1s` to `24h`, such as `5m`. |
| `kubernetes: invalid observation heartbeat` | Same fix as above. |
| `storage: invalid evidence retention duration` | Set `retention` to a duration in hours, such as `"168h"`, or to `"0"`. Negative values and day units such as `7d` are rejected. |

### How long history is kept

`retention` is a Go duration in hours, for example `"168h"` for 7 days. There
is no day unit.

#### Deleted by whole month

History is stored per calendar month. A month is deleted only once its last day
is older than the retention value. So the setting is a minimum: at 7 days, you
always keep at least the last 7 days, and sometimes up to about 5 weeks.

With the 7-day default:

| Date | What is deleted | What pages can still show |
|------|-----------------|---------------------------|
| Nov 7 | Nothing yet: Oct 31 is only 7 days ago | Oct 1 – Nov 7 (about 5 weeks) |
| Nov 8 | All of October | Nov 1 – Nov 8 (8 days) |
| Dec 7 | Nothing yet | Nov 1 – Dec 7 (about 5 weeks) |

A longer value works the same way: `720h` keeps at least 30 days, and up to
about 2 months.

#### Ongoing issues are thinned, not lost

While an issue stays open, Versus records that it is still happening every
[heartbeat](#settings) (5 minutes by default). Samples older than the retention
value are reduced to one per hour. Each issue's start, its end and every change
to it are always kept.

#### What each setting does

| Retention | Months deleted | Issue samples thinned |
|-----------|----------------|-----------------------|
| `"0"` (keep all) | Never | Never |
| `168h` (default) | Older than 7 days | Older than 7 days |
| `720h` | Older than 30 days | Older than 30 days |
| Under 7 days, e.g. `24h` | Older than 1 day | Older than 7 days (thinning never starts sooner) |

- Records that are already older than the retention value when Versus sees them
  are not saved, for example an old container termination that a Pod still
  lists.
- When retention deletes a month, history pages open on a later page start
  again; see [When a page link stops working](#when-a-page-link-stops-working).
  The start of history on Timeline, Changes and Crashes moves forward only when
  a month is deleted.
- Thinning issue samples does not reset Changes, Crashes or Timeline and does not
  add a gap. Only an Issues history view of a time more than about 5 days ago
  (with the default retention) may start again.
- Issue summaries and the capture log are never deleted by retention, so they
  keep growing slowly.

#### When a page link stops working

A page link stops working when retention deletes a month, or when it was saved
before an upgrade. Each page then behaves as follows:

- **Issues history** goes back to page 1. A page already on screen keeps its
  rows and shows "This list changed since this page loaded."
- **Timeline** shows "Timeline unavailable." with a **Restart Timeline**
  button. The resource Timeline shows "History could not be read" with a
  **Restart history** button.
- **Resource Crashes** shows "Crash evidence unavailable". Reopen the resource
  to start again.
- **API clients** get `400 cursor_expired` and must start again from the first
  page.

#### Keep history longer

| To keep | Set `retention` to |
|---------|--------------------|
| At least 30 days | `"720h"` |
| At least 13 months | `"9504h"` |
| Everything | `"0"` |

```yaml
kubernetes:
  evidence:
    retention: "9504h"
```

Raising the value keeps more from now on. It does not bring back history that
was already deleted.

#### Disk use

Disk use levels off once retention starts deleting months. With `"0"` it keeps
growing for as long as Versus runs.

| Cluster | Records per day | Issues open at any time | 7 days (default) | 13 months | Forever |
|---------|-----------------|-------------------------|------------------|-----------|---------|
| Small | 1,000 | 5 | about 0.2 GB | about 2.5 GB | about 2.5 GB a year |
| Medium | 10,000 | 25 | about 2 GB | about 24 GB | about 22 GB a year |
| Busy | 50,000 | 100 | about 10 GB | about 120 GB | about 110 GB a year |

Figures assume the default `5m` heartbeat and decimal units
(1 GB = 1,000,000,000 bytes). Issue summaries and the capture log add under
1 GB a year on a busy cluster. Plan your usual Postgres free-space headroom on
top.

To check how much space the database uses:

```sql
SELECT pg_size_pretty(pg_database_size(current_database()));
```

### Large clusters

**Saves time out.** Keep `save_chunk` at `500` unless the log often shows
`storage: evidence save part timed out; retrying it alone at N records`. Then
lower it, for example to `200`, and check the database for load.

**Many clusters or viewers.** Raise `max_open_conns` if you run many clusters
or a busy agent and saves queue. Raise `history_read_concurrency` when many
people view history at the same time. Lower either one if your database has a
small connection limit.

**Captures over the size limit.** One capture (one read of the cluster) holds
at most **25,000 records** and **15 MiB**, on every storage backend. A larger
capture, for example during a rollout that replaces thousands of Pods, is still
saved: Versus keeps open issues and crash status, then the newest changes that
fit, and drops the oldest. Pages show a normal save. The server writes one line
per such capture:

```text
kubernetes: history capture over the save limit, kept newest offered=… kept=… dropped=… dropped_from=… dropped_to=… dropped_changes=… dropped_pod_churn=…
```

`dropped_from` and `dropped_to` give the time span of the dropped records. No
action is needed. If open issues and crash status alone exceed the limit, the
capture is not saved and pages show `history_save_rejected`.

### When history is not saving

History pages (Issues, both Timelines, Changes, Crashes and the resource drawer)
keep showing saved rows when the latest capture is not saved, with a notice that
says why. Live pages stay current.

| Notice and code | Meaning | What to do |
|-----------------|---------|------------|
| "History is temporarily not saving — showing data from *time*" (`history_save_delayed`) | The latest capture is not saved yet because the database is slow or unreachable, or a save timed out. | Check database health and free connections. History catches up on its own once saves succeed. |
| "History could not save the latest data — showing data from *time*" (`history_save_rejected`) | The latest capture can never be saved. It is skipped, collection continues, and the missed interval shows as a gap. The notice clears after the next successful save. | Find the reason in the server log (see below). Restarting does not help. |
| "The Kubernetes connector is configured, but its credential cannot be resolved." (`connector_credential_unresolved`) | A real credential failure. A database problem never shows this notice. | Restore or rotate the connector credential. See [Kubernetes configuration](/agent/kubernetes/configuration#security-and-troubleshooting). |

When Versus has no saved time to show, the first two notices end with
"showing the last saved data" instead.

Log lines to act on:

| Log line | Meaning | What to do |
|----------|---------|------------|
| `kubernetes: capture stage=journal reason=storage_unavailable` | A save failed for a temporary reason. Pages show `history_save_delayed`. | Check database health and connections. |
| `kubernetes: capture stage=journal reason=storage_rejected`, then `kubernetes: history store rejected a capture; collection continues and the next capture records the gap reason=<reason>` | The database refused a capture for good. Pages show `history_save_rejected`. | Read `reason=` and see the next row. |
| `kubernetes: history capture refused before saving reason=<reason> class=<class>` | The capture was not saved. `kubernetes: history captures are prepared for saving again` follows once saving resumes. | See the reasons below. |
| `kubernetes: recovery stage=credential_recovery reason=credential_unavailable` (or `credential_rejected`) | A real credential failure. | Restore or rotate the connector credential. |
| `storage: evidence save timed out in one transaction; saving it in parts of N records` or `storage: evidence save part timed out; retrying it alone at N records` | A save was too slow. Occasional lines are fine. | If they are frequent, check database load and lower [`save_chunk`](#large-clusters). |
| `storage: evidence saves keep appending most records offered=… inserted=…` | History is growing faster than the cluster is changing. Brief bursts during a large rollout are expected. | If it keeps appearing on a quiet cluster, report it with the log line. |

Reasons in `reason=`:

| Reason | Pages show | What to do |
|--------|------------|------------|
| `authority_unverified` | `history_save_delayed` | The connector's read permissions are not confirmed yet. Wait for the next capture. If it persists, check the connector's [Kubernetes RBAC](/agent/kubernetes/configuration#kubernetes-rbac). |
| `incomplete_inventory` | `history_save_delayed` | The cluster listing was incomplete. Usually clears on the next capture. If it persists, check that the cluster API server is reachable. |
| `over_save_limit`, `unstorable_text`, `invalid_capture`, `store_unsupported` | `history_save_rejected` | The capture can never be saved. Restarting does not help. If it repeats on every capture, report it with the log line. |

Any other reason is treated as temporary, and pages show
`history_save_delayed`.

### Without Postgres

With file storage (`STORAGE_TYPE=file`), history is kept in a local SQLite
file, `kubernetes-evidence.sqlite`, in the data directory. It is bounded on its
own: for each cluster it keeps at most the last 24 hours, 5,000 records and
4 MiB of history, and drops the oldest first. `retention` and `save_chunk` do
not apply. Because the oldest history is dropped continuously at these limits,
a history list on a later page can start again, as described in
[When a page link stops working](#when-a-page-link-stops-working).

To back it up, use SQLite's online backup, or stop Versus first and copy the
file together with its WAL file. Persist the data directory so history survives
restarts.
