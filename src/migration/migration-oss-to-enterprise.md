# Migrating OSS Data to Enterprise

_Enterprise_

This guide applies whenever you replace the
community (OSS) binary with the Enterprise one and want to keep the data you
already have.

## The problem: your data is still under `default`

A community deployment is **single-tenant**. Every row and every model-state
blob is written under the org id **`default`**.

The Enterprise binary is different: it resolves the **one org it serves** from
the **license claim**, once, at boot — and from that moment every org-scoped
read path only ever looks under **that** org.

So an operator who upgrades an existing OSS Postgres deployment boots into what
looks like an **empty product**: the incidents, AI analyses, learned pattern
catalog, discovered services and learned model state are all still filed under
`default`, while the binary is reading under, say, `acme`.

**Nothing is lost — it is simply out of view.** If you never run this migration
nothing breaks and nothing is deleted; the pre-upgrade data stays under
`default`, inert and invisible, and Enterprise starts learning from scratch.

## First: check whether you need to migrate at all

If your **deployment org is `default`**, there is **nothing to migrate** — the
Enterprise binary is reading exactly where the OSS binary wrote.

Ask the running binary which org it serves:

```bash
curl -s http://localhost:3000/enterprise/api/sso/deployment
# {"org":"acme"}
```

| Response | What it means |
|---|---|
| `{"org":"acme"}` (or any non-`default` id) | **Migrate.** That id is your `target_org` below. |
| `{"org":"default"}` | **Nothing to do.** Your data is already where the binary reads. |
| `403` | The binary is running **unlicensed** (community mode). It uses `default` — nothing to migrate. |

> A **licensed** Enterprise binary whose license carries **no org claim** does
> not fall back to `default` — it **refuses to start**:
> `enterprise: license carries no org claim; refusing to start`. If you see
> that, fix the license, don't migrate.

## If something goes wrong

Take a backup before you start:

```bash
pg_dump "$POSTGRES_DSN" > versus_pre_org_migration.sql
```

## The script

[`scripts/postgres/migrate_org_to_enterprise.sql`](https://github.com/VersusControl/versus-incident/blob/main/scripts/postgres/migrate_org_to_enterprise.sql)
re-keys the OSS Postgres data from `default` to your deployment org, once.

```bash
psql "$POSTGRES_DSN" -v ON_ERROR_STOP=1 -v target_org=acme \
     -f scripts/postgres/migrate_org_to_enterprise.sql
```

Replace `acme` with your own org id. Before you run it:

| Requirement | Why |
|---|---|
| `target_org` **must equal the org id in your license claim** | It is what the binary reads under. The script validates the format and refuses an empty value or `default`, but it cannot know your license — a typo migrates your data into another invisible org. |
| **Stop the Versus binary — all replicas** | The script takes brief `ACCESS EXCLUSIVE` locks to flip foreign keys, and a running writer would interleave new `default` rows with the move. |
| **Postgres only** | It is DML against the `vs_*` tables. See [file backend](#file-backend-no-script) below. |

It is **transactional** (all-or-nothing) and **re-runnable** — every statement
targets only rows still filed under `default`, so after a successful run a
second run reports 0 rows moved everywhere and commits cleanly. Run it again if
you are unsure whether the first attempt completed.

### Verifying the run

The script prints a **BEFORE** and an **AFTER** report — a row per table with the
counts under `default` and under your target org:

```
=== BEFORE ===
             scope              | under_default | under_target
--------------------------------+---------------+--------------
 vs_incidents                   |          1284 |            0
 vs_patterns                    |           412 |            0
 ...
```

A successful migration shows those counts moved from `under_default` to
`under_target` in the AFTER report. Open the UI afterwards — your incident
history, pattern catalog and services should be back.

## What moves

| Data | Where it lives |
|---|---|
| **Incidents** | `vs_incidents.org_id` |
| **AI analyses** | `vs_analyses` — the org lives *inside* the JSON document, and is rewritten in place |
| **Pattern catalog** | `vs_patterns`, `vs_logs`, `vs_services` |
| **Learned model state** | `vs_blobs` — the org is a path component of the blob name, `models/default/…` → `models/<org>/…` |
| **Service-attribution overrides** | `vs_blobs` — only the `default` entries in the rules list are rewritten |

## Conflict policy: skip, never overwrite

If you ran the Enterprise binary for a while before migrating, the target org may
already own a row with the same key — a re-learned pattern, a re-discovered
service, a rewritten model blob.

The policy is **skip-if-exists**. Target-org data is **never overwritten and
never deleted**; the old row is **left under `default`** and its live twin wins.
The children of a skipped pattern are skipped with it, so a pattern and its logs
never split across orgs.

> A non-zero **"stays under default"** count in the AFTER report is a **skip, not
> an error**.

The script is transactional, so a failure leaves the database untouched. Please
[open an issue](https://github.com/VersusControl/versus-incident/issues) with the
BEFORE report and the error.
