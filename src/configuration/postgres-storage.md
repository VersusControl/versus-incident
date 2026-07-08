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
