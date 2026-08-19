-- migrate_org_to_enterprise.sql — one-time operator migration.
--
-- A community (OSS) deployment is single-tenant: every row and every
-- model-state blob is written under the org id "default"
-- (storage.DefaultOrgID). An ENTERPRISE binary is different — it derives the
-- one org it serves from the license claim at boot, and from that moment on
-- the org-scoped read paths (the Postgres pattern-catalog store, the analyze
-- tool catalog, the service-attribution overrides, the model-state namespace)
-- only ever look under THAT org.
--
-- So an operator who upgrades an existing OSS Postgres deployment to
-- Enterprise boots into what looks like an empty product: the incidents, the
-- learned log-pattern catalog, the discovered services and the learned model
-- state are all still filed under "default", while the binary is reading
-- under, say, "acme". Nothing is lost — it is simply out of view.
--
-- The Enterprise binary already re-keys ITS OWN state on first licensed boot
-- (audit log, role assignments, team roles, SSO policy, SSO connections). That
-- is enterprise-private state. NOTHING moves the OSS data plane. This script
-- is that move: it re-keys the OSS Postgres data from "default" to your
-- deployment org, once, during a maintenance window.
--
-- If you do NOT run this script nothing breaks and nothing is deleted — the
-- pre-upgrade data stays under "default", inert and invisible, and the
-- Enterprise binary starts learning from scratch under the license org.
--
--
-- HOW TO RUN
-- ==========
--
--   psql "$POSTGRES_DSN" -v ON_ERROR_STOP=1 -v target_org=acme \
--        -f scripts/postgres/migrate_org_to_enterprise.sql
--
-- `target_org` MUST be the org id in your license claim — the same value the
-- Enterprise binary logs at boot. It is a psql variable, never hardcoded here.
-- Passing nothing, an empty value, or "default" aborts the script.
--
-- Stop the Versus binary (all replicas) before running. The script takes brief
-- ACCESS EXCLUSIVE locks on vs_logs to flip its foreign keys, and a running
-- writer would otherwise interleave new "default" rows with the move.
--
--
-- WHAT MOVES
-- ==========
--
-- Org-scoped by COLUMN:
--   vs_incidents.org_id                 incident history
--   vs_patterns.org_id                  learned signal catalog root, PK (org_id, id)
--   vs_logs.org_id                      learned log properties, PK (org_id, pattern_id,
--                                       instance_index), FK -> vs_patterns
--   vs_services.org_id                  discovered/manual services, PK (org_id, name)
--
-- Org-scoped INSIDE a JSONB document:
--   vs_analyses.data->>'org_id'         AI analysis history. vs_analyses has no org_id
--                                       column — the whole AnalysisRecord (including its
--                                       org) is stored in `data`, so the org is rewritten
--                                       in place inside the document.
--   vs_blobs 'service-overrides'        the operator's service-attribution rules. One
--                                       versioned blob holding every org's rules in a
--                                       `rules[]` array, each entry carrying its own
--                                       org_id. Only the "default" entries are rewritten.
--
-- Org-scoped by BLOB NAME:
--   vs_blobs 'models/default/<agent>/<key>'  ->  'models/<target_org>/<agent>/<key>'
--                                       the learned model-state namespace. The org is a
--                                       path component of the blob name.
--
--                                       NOTE the deliberately narrow prefix. Runtime
--                                       SETTINGS blobs live at 'models/settings/<key>'
--                                       (report-settings, spike-baseline,
--                                       intake-settings). "settings" occupies the org
--                                       slot but is NOT an org — those blobs are
--                                       deployment-wide and MUST NOT move. Matching
--                                       'models/default/%' rather than 'models/%' leaves
--                                       them untouched by construction.
--
--
-- WHAT DOES NOT MOVE (and why that is correct)
-- ============================================
--
--   vs_shadow, vs_detect                the shadow/detect agent documents. Written under
--                                       the bare blob names "shadow" / "detect" with no
--                                       org component, and read back the same way by the
--                                       Enterprise binary. They carry over untouched.
--
--   vs_members, vs_teams                the OSS people directory ("members" / "teams"
--                                       bare blobs). Not org-keyed; they carry over
--                                       untouched. (The Enterprise SSO member directory
--                                       is a SEPARATE, org-namespaced blob that the
--                                       binary re-keys itself on first licensed boot.)
--
--   vs_blobs 'models/settings/%'        deployment-wide runtime settings — see above.
--
--   vs_blobs 'runbooks'                 runbook records carry an org_id field, but no
--                                       read path filters on it, so the corpus is visible
--                                       under any org. Left alone.
--
--   versus_schema_migrations            the migration ledger. Not org-scoped.
--
--   Enterprise tables (vs_metrics, vs_traces, vs_alert_*, …)
--                                       created by the ENTERPRISE binary on its first
--                                       boot, under the license org. They hold no
--                                       pre-existing OSS data, so there is nothing to
--                                       re-key. This script does not reference them and
--                                       runs fine on a database where they do not exist.
--
--
-- FOREIGN KEYS
-- ============
--
-- vs_logs references vs_patterns twice — (org_id, pattern_id) -> (org_id, id) and the
-- kind-carrying (org_id, pattern_id, kind) -> (org_id, id, kind) — both ON DELETE
-- CASCADE. There is no ON UPDATE CASCADE, so re-keying the parent's org_id would orphan
-- the children and be rejected at the end of the UPDATE statement; re-keying the children
-- first would point them at a parent that does not exist yet. Either order fails.
--
-- The fix is to defer the checks to COMMIT time so the intermediate state is legal:
-- the script flips both foreign keys to DEFERRABLE INITIALLY DEFERRED, runs
-- SET CONSTRAINTS ALL DEFERRED, moves parents and children, then forces the checks with
-- SET CONSTRAINTS ALL IMMEDIATE — INSIDE the transaction, so a violation aborts the whole
-- thing rather than surfacing at COMMIT — and finally restores both constraints to their
-- original (NOT DEFERRABLE) attributes. The constraint names are looked up from
-- pg_constraint rather than hardcoded, so a database whose FKs were auto-named
-- differently still migrates.
--
-- Alternatives that were rejected: dropping and re-adding the FKs (a crash between the
-- two leaves the schema unconstrained), and copy-then-delete (the ON DELETE CASCADE
-- makes the cleanup step a foot-gun).
--
--
-- RE-RUNNING IT
-- =============
--
-- The script is idempotent. Every statement targets only rows still filed under
-- "default", and after a successful run there are none, so a second run reports 0 rows
-- moved everywhere and commits cleanly. It is safe to re-run if you are not sure whether
-- the first attempt completed.
--
--
-- CONFLICTS: SKIP, NEVER OVERWRITE
-- ================================
--
-- If you already ran the Enterprise binary for a while before migrating, the target org
-- may already own rows with the same key. The policy is SKIP-IF-EXISTS — target-org data
-- is never overwritten and never deleted:
--
--   vs_patterns   a pattern id that already exists under the target org is LEFT under
--                 "default" (its live, re-learned twin wins).
--   vs_logs       the children of a skipped pattern are skipped with it, so parent and
--                 children never split across orgs. Children of a pattern that DOES move
--                 can never collide: the foreign key guarantees a child only exists where
--                 its parent does.
--   vs_services   a service name that already exists under the target org is LEFT under
--                 "default".
--   vs_blobs      a model-state blob whose target name is already taken is LEFT under
--                 'models/default/…'.
--   vs_incidents  cannot collide — the primary key is the incident id alone, so a given
--                 incident exists under exactly one org.
--   vs_analyses   cannot collide — same reason (primary key is the analysis id).
--
-- Skipped rows stay exactly where they are. Re-check the "before"/"after" reports below:
-- a non-zero "stays_under_default" count is the skip count, not an error.
--
--
-- OTHER STORAGE BACKENDS
-- ======================
--
-- This script is Postgres only — it is DML against the vs_* tables and will not run
-- anywhere else.
--
-- On the `file` backend there is nothing to run, because the equivalent move is a
-- directory rename. Model state is laid out per org on disk, so with the binary
-- stopped:
--
--   mv data/models/default data/models/acme
--
-- is the whole model-state migration. The remaining file-backend state either is not
-- org-keyed (data/shadow.json, data/detect.json, data/members.json, data/teams.json —
-- these carry over as-is, exactly like their Postgres counterparts) or carries the org
-- as a field inside a single document (data/incidents.json, data/analyses.json,
-- data/patterns.json, data/service-overrides.json), where a `jq` pass swapping
-- "default" for your license org in each record's org_id does the same job this script
-- does in SQL.
--
-- If you are moving to Enterprise you are almost certainly moving to Postgres too — do
-- that first, let the binary create the schema and ingest, then run this script.

\set ON_ERROR_STOP on

\if :{?target_org}
\else
\echo '****'
\echo '**** target_org is not set. Re-run with the org id from your license claim, e.g.:'
\echo '****   psql "$POSTGRES_DSN" -v ON_ERROR_STOP=1 -v target_org=acme -f scripts/postgres/migrate_org_to_enterprise.sql'
\echo '****'
\set target_org ''
\endif

BEGIN;

-- ---------------------------------------------------------------------------
-- 0. Validate the target org. Anything that is not a well-formed, non-default
--    org id aborts before a single row is touched. The pattern is the same one
--    the platform enforces when it mints an org id, so a value that passes here
--    is a value a license can actually carry.
--
--    psql does not interpolate its variables inside dollar-quoted blocks, so the
--    value is handed to PL/pgSQL through a transaction-local setting instead of
--    being pasted into the block body.
-- ---------------------------------------------------------------------------

SELECT set_config('versus.target_org', :'target_org', true) AS migrating_to_org;

DO $$
DECLARE
    target CONSTANT text := current_setting('versus.target_org');
BEGIN
    IF btrim(target) = '' THEN
        RAISE EXCEPTION
            'target_org is empty or unset — pass the org id from your license claim: psql ... -v target_org=acme -f %',
            'scripts/postgres/migrate_org_to_enterprise.sql';
    END IF;
    IF target = 'default' THEN
        RAISE EXCEPTION
            'target_org is "default" — that is the org this script migrates AWAY from. Pass your license org instead.';
    END IF;
    IF target !~ '^[a-z0-9][a-z0-9-]{0,63}$' THEN
        RAISE EXCEPTION
            'target_org % is not a valid org id (expected ^[a-z0-9][a-z0-9-]{0,63}$)', quote_literal(target);
    END IF;
END
$$;

-- ---------------------------------------------------------------------------
-- 1. BEFORE: what is filed where. Read this before you commit to the run.
-- ---------------------------------------------------------------------------

\echo ''
\echo '=== BEFORE ==='

SELECT 'vs_incidents'                    AS scope,
       count(*) FILTER (WHERE org_id = 'default')       AS under_default,
       count(*) FILTER (WHERE org_id = :'target_org')   AS under_target
FROM vs_incidents
UNION ALL
SELECT 'vs_patterns',
       count(*) FILTER (WHERE org_id = 'default'),
       count(*) FILTER (WHERE org_id = :'target_org')
FROM vs_patterns
UNION ALL
SELECT 'vs_logs',
       count(*) FILTER (WHERE org_id = 'default'),
       count(*) FILTER (WHERE org_id = :'target_org')
FROM vs_logs
UNION ALL
SELECT 'vs_services',
       count(*) FILTER (WHERE org_id = 'default'),
       count(*) FILTER (WHERE org_id = :'target_org')
FROM vs_services
UNION ALL
SELECT 'vs_analyses (data->>org_id)',
       count(*) FILTER (WHERE coalesce(nullif(data->>'org_id', ''), 'default') = 'default'),
       count(*) FILTER (WHERE data->>'org_id' = :'target_org')
FROM vs_analyses
UNION ALL
SELECT 'vs_blobs models/<org>/…',
       count(*) FILTER (WHERE name LIKE 'models/default/%'),
       count(*) FILTER (WHERE name LIKE 'models/' || :'target_org' || '/%')
FROM vs_blobs
UNION ALL
SELECT 'vs_blobs models/settings/… (must not move)',
       count(*) FILTER (WHERE name LIKE 'models/settings/%'),
       0
FROM vs_blobs;

-- ---------------------------------------------------------------------------
-- 2. Decide which patterns and services may move, BEFORE anything is updated.
--    Once vs_patterns has been rewritten the "does the target already own this
--    id?" question can no longer be asked, so the answer is captured here.
--    ON COMMIT DROP ties these to the transaction.
-- ---------------------------------------------------------------------------

CREATE TEMP TABLE _org_move_patterns ON COMMIT DROP AS
SELECT p.id
FROM vs_patterns p
WHERE p.org_id = 'default'
  AND NOT EXISTS (
        SELECT 1 FROM vs_patterns t
        WHERE t.org_id = :'target_org' AND t.id = p.id
      );

CREATE TEMP TABLE _org_move_services ON COMMIT DROP AS
SELECT s.name
FROM vs_services s
WHERE s.org_id = 'default'
  AND NOT EXISTS (
        SELECT 1 FROM vs_services t
        WHERE t.org_id = :'target_org' AND t.name = s.name
      );

-- ---------------------------------------------------------------------------
-- 3. Defer the vs_logs -> vs_patterns foreign keys. See the FOREIGN KEYS note
--    in the header for why this is required and what the alternatives cost.
--    The original attributes are captured so they can be restored in step 6.
-- ---------------------------------------------------------------------------

CREATE TEMP TABLE _org_move_fks ON COMMIT DROP AS
SELECT conname, condeferrable, condeferred
FROM pg_constraint
WHERE conrelid = 'vs_logs'::regclass
  AND contype = 'f';

DO $$
DECLARE
    fk record;
BEGIN
    FOR fk IN SELECT conname FROM _org_move_fks LOOP
        EXECUTE format(
            'ALTER TABLE vs_logs ALTER CONSTRAINT %I DEFERRABLE INITIALLY DEFERRED',
            fk.conname);
    END LOOP;
END
$$;

SET CONSTRAINTS ALL DEFERRED;

-- ---------------------------------------------------------------------------
-- 4. Move the data.
-- ---------------------------------------------------------------------------

-- 4a. Incidents. The primary key is the id alone, so the org flip is
--     collision-free.
UPDATE vs_incidents
SET org_id = :'target_org'
WHERE org_id = 'default';

-- 4b. Learned log properties, then their catalog roots. Order is irrelevant
--     while the foreign keys are deferred; children first simply reads better.
UPDATE vs_logs
SET org_id = :'target_org'
WHERE org_id = 'default'
  AND pattern_id IN (SELECT id FROM _org_move_patterns);

UPDATE vs_patterns
SET org_id = :'target_org'
WHERE org_id = 'default'
  AND id IN (SELECT id FROM _org_move_patterns);

-- 4c. Discovered / manually curated services.
UPDATE vs_services
SET org_id = :'target_org'
WHERE org_id = 'default'
  AND name IN (SELECT name FROM _org_move_services);

-- 4d. Analyses. The org lives inside the stored AnalysisRecord, not in a
--     column. A record written before the org field existed has no 'org_id'
--     key at all; that is the same as "default" and is rewritten too.
UPDATE vs_analyses
SET data = jsonb_set(data, '{org_id}', to_jsonb(:'target_org'::text), true)
WHERE coalesce(nullif(data->>'org_id', ''), 'default') = 'default';

-- 4e. Model-state blobs: the org is a path component of the blob name.
--     'models/default/' is 15 characters, so the agent/key tail starts at 16.
--     Skips any name already taken under the target org.
UPDATE vs_blobs b
SET name       = 'models/' || :'target_org' || '/' || substring(b.name FROM 16),
    updated_at = NOW()
WHERE b.name LIKE 'models/default/%'
  AND NOT EXISTS (
        SELECT 1 FROM vs_blobs t
        WHERE t.name = 'models/' || :'target_org' || '/' || substring(b.name FROM 16)
      );

-- 4f. Service-attribution overrides: one blob, one rules[] array, each rule
--     carrying its own org_id. Rewrite only the "default" rules and leave any
--     rule already filed under another org alone. The IS DISTINCT FROM guard
--     keeps a second run at 0 rows instead of a no-change rewrite.
UPDATE vs_blobs b
SET data       = convert_to(x.new_doc::text, 'UTF8'),
    updated_at = NOW()
FROM (
    SELECT s.doc AS old_doc,
           jsonb_set(s.doc, '{rules}', coalesce(a.rules, '[]'::jsonb), true) AS new_doc
    FROM (
        SELECT convert_from(data, 'UTF8')::jsonb AS doc
        FROM vs_blobs
        WHERE name = 'service-overrides'
    ) s
    CROSS JOIN LATERAL (
        SELECT jsonb_agg(
                   CASE
                       WHEN coalesce(nullif(r->>'org_id', ''), 'default') = 'default'
                       THEN jsonb_set(r, '{org_id}', to_jsonb(:'target_org'::text), true)
                       ELSE r
                   END
                   ORDER BY ord) AS rules
        FROM jsonb_array_elements(s.doc->'rules') WITH ORDINALITY AS t(r, ord)
    ) a
    WHERE jsonb_typeof(s.doc->'rules') = 'array'
) x
WHERE b.name = 'service-overrides'
  AND x.new_doc IS DISTINCT FROM x.old_doc;

-- ---------------------------------------------------------------------------
-- 5. Force the deferred foreign-key checks NOW, while the transaction is still
--    open, so a violation rolls the whole migration back with a usable error
--    instead of blowing up at COMMIT.
-- ---------------------------------------------------------------------------

SET CONSTRAINTS ALL IMMEDIATE;

-- ---------------------------------------------------------------------------
-- 6. Restore the foreign keys to the attributes they had in step 3.
-- ---------------------------------------------------------------------------

DO $$
DECLARE
    fk record;
BEGIN
    FOR fk IN SELECT conname, condeferrable, condeferred FROM _org_move_fks LOOP
        EXECUTE format(
            'ALTER TABLE vs_logs ALTER CONSTRAINT %I %s %s',
            fk.conname,
            CASE WHEN fk.condeferrable THEN 'DEFERRABLE' ELSE 'NOT DEFERRABLE' END,
            CASE WHEN fk.condeferred  THEN 'INITIALLY DEFERRED' ELSE 'INITIALLY IMMEDIATE' END);
    END LOOP;
END
$$;

-- ---------------------------------------------------------------------------
-- 7. AFTER: prove it moved. `stays_under_default` should be 0 everywhere
--    except where the skip-if-exists policy deliberately left rows behind
--    (see CONFLICTS in the header), and the settings row must be unchanged.
-- ---------------------------------------------------------------------------

\echo ''
\echo '=== AFTER ==='

SELECT 'vs_incidents'                    AS scope,
       count(*) FILTER (WHERE org_id = 'default')       AS stays_under_default,
       count(*) FILTER (WHERE org_id = :'target_org')   AS now_under_target
FROM vs_incidents
UNION ALL
SELECT 'vs_patterns',
       count(*) FILTER (WHERE org_id = 'default'),
       count(*) FILTER (WHERE org_id = :'target_org')
FROM vs_patterns
UNION ALL
SELECT 'vs_logs',
       count(*) FILTER (WHERE org_id = 'default'),
       count(*) FILTER (WHERE org_id = :'target_org')
FROM vs_logs
UNION ALL
SELECT 'vs_services',
       count(*) FILTER (WHERE org_id = 'default'),
       count(*) FILTER (WHERE org_id = :'target_org')
FROM vs_services
UNION ALL
SELECT 'vs_analyses (data->>org_id)',
       count(*) FILTER (WHERE coalesce(nullif(data->>'org_id', ''), 'default') = 'default'),
       count(*) FILTER (WHERE data->>'org_id' = :'target_org')
FROM vs_analyses
UNION ALL
SELECT 'vs_blobs models/<org>/…',
       count(*) FILTER (WHERE name LIKE 'models/default/%'),
       count(*) FILTER (WHERE name LIKE 'models/' || :'target_org' || '/%')
FROM vs_blobs
UNION ALL
SELECT 'vs_blobs models/settings/… (must not move)',
       count(*) FILTER (WHERE name LIKE 'models/settings/%'),
       0
FROM vs_blobs;

COMMIT;

\echo ''
\echo 'Done. Start the Enterprise binary and confirm its boot log reports the same org.'
