package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"
)

func evidenceAuthoritySQL(query EvidenceQuery, args *[]any, kind, namespace string) (string, error) {
	if !query.EnforceAuthority {
		return "TRUE", nil
	}
	if len(query.Authorities) == 0 {
		*args = append(*args, query.AllowedKinds)
		return fmt.Sprintf(`((%s)<>'' AND %s=ANY($%d::text[]))`, kind, kind, len(*args)), nil
	}
	raw, err := json.Marshal(query.Authorities)
	if err != nil {
		return "", err
	}
	*args = append(*args, raw)
	parameter := fmt.Sprintf("$%d::jsonb", len(*args))
	global := "(" + parameter + "->(" + kind + "))"
	scoped := "(" + parameter + "->(" + kind + "||'@'||" + namespace + "))"
	return `((` + kind + `)<>'') AND (CASE WHEN ` + scoped + ` IS NOT NULL AND (` + global + ` IS NULL OR (` + scoped + `->>'proved_at')::timestamptz>(` + global + `->>'proved_at')::timestamptz
 OR ((` + scoped + `->>'proved_at')::timestamptz=(` + global + `->>'proved_at')::timestamptz AND ` + scoped + `->>'state'<>'allowed'))
 THEN ` + scoped + `->>'state'='allowed' ELSE ` + global + `->>'state'='allowed' END)`, nil
}

func (p *postgresProvider) FindEvidenceScope(parent context.Context, orgID, clusterID, binding string) (EvidenceScope, error) {
	ctx, cancel := context.WithTimeout(parent, 2*time.Second)
	defer cancel()
	tx, err := p.beginEvidenceRead(ctx)
	if err != nil {
		return EvidenceScope{}, err
	}
	defer tx.Rollback()
	scope := EvidenceScope{OrgID: NormalizeOrgID(orgID), ClusterID: clusterID}
	err = tx.QueryRowContext(ctx, `SELECT cluster_uid FROM kubernetes_evidence_scopes WHERE org_id=$1 AND cluster_id=$2 AND scope_binding=$3 ORDER BY updated_at DESC LIMIT 1`, scope.OrgID, clusterID, binding).Scan(&scope.ClusterUID)
	if err == sql.ErrNoRows {
		return EvidenceScope{}, ErrNotFound
	}
	if err != nil {
		return EvidenceScope{}, err
	}
	return scope, tx.Commit()
}

func (p *postgresProvider) SetEvidenceAuthority(parent context.Context, scope EvidenceScope, principal string, proofs map[string]EvidenceAuthority) error {
	key, err := evidenceScopeKey(scope)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(parent, 2*time.Second)
	defer cancel()
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var raw []byte
	err = tx.QueryRowContext(ctx, `SELECT checkpoint FROM kubernetes_evidence_scopes WHERE scope_key=$1 FOR UPDATE`, key).Scan(&raw)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	var cp EvidenceCheckpoint
	if err = json.Unmarshal(raw, &cp); err != nil {
		return err
	}
	if cp.Principal != principal {
		return ErrEvidenceConflict
	}
	mergeEvidenceAuthority(&cp, proofs)
	cp.CommitRevision = max(cp.CommitRevision, cp.OccurrenceRevision) + 1
	raw, err = json.Marshal(cp)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE kubernetes_evidence_scopes SET checkpoint=$2 WHERE scope_key=$1`, key, raw); err != nil {
		return err
	}
	return tx.Commit()
}

// evidenceWriteStats describes one save of journal events. Offered counts the
// records a capture sent; inserted counts the new versions it appended.
type evidenceWriteStats struct {
	High      uint64
	Offered   int
	Inserted  int
	Refreshed int
	// Expired counts records older than the retention cutoff, not written.
	Expired int
	// Parts counts the save transactions before the final one.
	Parts int
}

// writeEvidenceEvents appends only real changes. Each incoming record is
// compared with its event's current version through the heads table, so the
// cost is proportional to the batch, not to how long the event has existed.
// That holds only while the scope's heads are complete. Until then (history
// from before heads, or a write by something that does not keep heads), every
// incoming event is compared with its newest journal row instead, and its head
// is rewritten from that row; that lookup filters nothing but the event's key
// before the LIMIT, so no plan can answer it through an index of the scope's
// current rows. It probes every monthly partition, so it is left out once
// the background pass has given every current row its head.
// An unchanged crash record refreshes its current row's observation time in
// place, at most once per heartbeat, instead of appending a copy. The rows to
// supersede or refresh are matched first (OFFSET 0 keeps them apart) and
// then reached by their full partition key, so no plan probes a partition for
// a record that changes nothing.
func writeEvidenceEvents(ctx context.Context, tx *sql.Tx, key string, records []EvidenceRecord, heartbeat time.Duration) (stats evidenceWriteStats, err error) {
	stats = evidenceWriteStats{Offered: len(records)}
	if len(records) == 0 {
		return stats, nil
	}
	input := make([]map[string]any, 0, len(records))
	for _, record := range records {
		input = append(input, map[string]any{
			"event_id": record.ID, "event_version": evidenceVersion(record), "event_type": record.Type,
			"event_at": record.At, "observed_at": record.ObservedAt, "namespace": record.Namespace, "resource_name": record.Name,
			"kind": record.Kind, "authorization_kind": record.AuthorizationKind, "resource_uid": record.UID,
			"root_uid": record.RootUID, "pod_uid": record.PodUID, "rule": record.Rule, "epoch": record.Epoch,
			"payload": record.Payload, "is_break": record.Broken, "priority": record.Priority,
			"gap_from": nullEvidenceTime(record.GapFrom), "gap_to": nullEvidenceTime(record.GapTo),
		})
	}
	raw, err := json.Marshal(input)
	if err != nil {
		return stats, err
	}
	complete, err := evidenceHeadsComplete(ctx, tx, key)
	if err != nil {
		return stats, err
	}
	targets := []string{"kubernetes_evidence_events"}
	current := `, current AS (
 SELECT i.event_id,l.sequence,l.event_at,l.event_version,l.priority,l.observed_at,TRUE AS seeded
 FROM incoming i CROSS JOIN LATERAL(
 SELECT p.sequence,p.event_at,p.event_version,p.priority,p.observed_at,p.superseded_seq FROM kubernetes_evidence_events p
 WHERE p.scope_key=$1 AND p.event_id=i.event_id ORDER BY p.sequence DESC LIMIT 1)l
 WHERE l.superseded_seq IS NULL
 )`
	if complete {
		current = `, current AS (
 SELECT h.event_id,h.sequence,h.event_at,h.event_version,h.priority,h.observed_at,FALSE AS seeded
 FROM incoming i JOIN kubernetes_evidence_event_heads h ON h.scope_key=$1 AND h.event_id=i.event_id
 )`
		// Saves plan every statement for its values. The rows a save may
		// supersede or refresh are its events' current versions, which the
		// heads name; their months, read (and the heads locked) first, are
		// the partitions the updates name, so the plan covers those months
		// only, and each row is still reached by its full key.
		ids := make([]string, 0, len(records))
		for _, record := range records {
			ids = append(ids, record.ID)
		}
		rows, headsErr := tx.QueryContext(ctx, `SELECT DISTINCT 'evidence_event_month_'||to_char(event_at AT TIME ZONE 'UTC','YYYYMM') FROM(
 SELECT event_at FROM kubernetes_evidence_event_heads WHERE scope_key=$1 AND event_id=ANY($2::text[]) FOR UPDATE)h`, key, ids)
		if headsErr != nil {
			return stats, headsErr
		}
		names := []string{}
		for rows.Next() {
			var name string
			if err = rows.Scan(&name); err != nil {
				rows.Close()
				return stats, err
			}
			names = append(names, name)
		}
		if err = rows.Err(); err != nil {
			rows.Close()
			return stats, err
		}
		rows.Close()
		// Only partitions of the journal itself are named; a journal not yet
		// partitioned is updated as it is. A month retention has dropped
		// holds none of these rows any more.
		var partitioned bool
		if err = tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT relkind='p' FROM pg_class WHERE oid=to_regclass('kubernetes_evidence_events')),FALSE)`).Scan(&partitioned); err != nil {
			return stats, err
		}
		if partitioned {
			present, presentErr := tx.QueryContext(ctx, `SELECT c.relname FROM pg_inherits i JOIN pg_class c ON c.oid=i.inhrelid
 WHERE i.inhparent=to_regclass('kubernetes_evidence_events') AND c.relname=ANY($1::text[]) ORDER BY 1`, names)
			if presentErr != nil {
				return stats, presentErr
			}
			targets = nil
			for present.Next() {
				var name string
				if err = present.Scan(&name); err != nil {
					present.Close()
					return stats, err
				}
				targets = append(targets, pgxIdentifier(name))
			}
			if err = present.Err(); err != nil {
				present.Close()
				return stats, err
			}
			present.Close()
		}
	}
	superseded, refreshed := evidenceUpdates(targets)
	err = tx.QueryRowContext(ctx, `WITH incoming AS (
 SELECT DISTINCT ON(event_id) * FROM jsonb_to_recordset($2::jsonb) AS x(
 event_id text,event_version text,event_type text,event_at timestamptz,observed_at timestamptz,
 namespace text,resource_name text,kind text,authorization_kind text,resource_uid text,root_uid text,
 pod_uid text,rule text,epoch text,payload jsonb,is_break boolean,priority integer,gap_from timestamptz,gap_to timestamptz)
 ORDER BY event_id,priority DESC,observed_at DESC
 )`+current+`, inserted AS (
 INSERT INTO kubernetes_evidence_events(scope_key,event_id,event_version,event_type,event_at,observed_at,namespace,resource_name,kind,authorization_kind,resource_uid,root_uid,pod_uid,rule,epoch,payload,is_break,priority,gap_from,gap_to)
 SELECT $1,i.* FROM incoming i LEFT JOIN current c ON c.event_id=i.event_id
 WHERE c.event_id IS NULL OR NOT(c.event_version=i.event_version OR c.priority>i.priority OR(c.priority=i.priority AND c.observed_at>i.observed_at))
 ON CONFLICT DO NOTHING RETURNING sequence,event_id,event_type,event_at,event_version,priority,observed_at
 )`+superseded+refreshed+`, heads AS (
 INSERT INTO kubernetes_evidence_event_heads AS head(scope_key,event_id,sequence,event_at,event_version,priority,observed_at)
 SELECT $1,n.event_id,n.sequence,n.event_at,n.event_version,n.priority,n.observed_at FROM inserted n
 UNION ALL
 SELECT $1,c.event_id,c.sequence,c.event_at,c.event_version,c.priority,COALESCE(r.observed_at,c.observed_at)
 FROM current c LEFT JOIN refreshed r ON r.event_id=c.event_id
 WHERE (c.seeded OR r.event_id IS NOT NULL) AND NOT EXISTS(SELECT 1 FROM inserted n WHERE n.event_id=c.event_id)
 ON CONFLICT(scope_key,event_id) DO UPDATE SET sequence=EXCLUDED.sequence,event_at=EXCLUDED.event_at,
 event_version=EXCLUDED.event_version,priority=EXCLUDED.priority,observed_at=EXCLUDED.observed_at
 RETURNING 1
 ), deltas AS (
 SELECT event_type,SUM(delta)::bigint AS delta FROM(
 SELECT n.event_type,1::bigint AS delta FROM inserted n JOIN kubernetes_evidence_scopes s ON s.scope_key=$1 WHERE s.read_model_ready
 UNION ALL SELECT old.event_type,-1::bigint FROM superseded old JOIN kubernetes_evidence_scopes s ON s.scope_key=$1 WHERE s.read_model_ready OR old.sequence<=s.backfill_sequence
 ) changes GROUP BY event_type
 ), summary AS (
 UPDATE kubernetes_evidence_scopes s SET event_counts=s.event_counts||COALESCE(
 (SELECT jsonb_object_agg(d.event_type,GREATEST(0,COALESCE((s.event_counts->>d.event_type)::bigint,0)+d.delta)) FROM deltas d),'{}'::jsonb),
 observed_lag=GREATEST(s.observed_lag,COALESCE((SELECT MAX(r.observed_at-r.event_at) FROM refreshed r),'0'))
 WHERE s.scope_key=$1 RETURNING s.scope_key
 ) SELECT (SELECT COALESCE(MAX(sequence),0) FROM inserted),(SELECT COUNT(*) FROM inserted),(SELECT COUNT(*) FROM refreshed)`,
		key, raw, max(heartbeat, time.Second).Seconds()).Scan(&stats.High, &stats.Inserted, &stats.Refreshed)
	return stats, err
}

// occurrenceFacts are the payload fields the read models index as columns.
type occurrenceFacts struct {
	Severity string     `json:"severity"`
	BrokeAt  *time.Time `json:"broke_at"`
}

// occurrencePayloadFacts extracts the indexed fields from an issue payload. A
// payload that does not decode as an object yields empty facts rather than an
// error: the memory and file stores keep such an issue as written and the
// service reports it unavailable on read, so one malformed retained issue must
// not fail the whole capture here either.
func occurrencePayloadFacts(raw json.RawMessage) occurrenceFacts {
	var facts occurrenceFacts
	if json.Unmarshal(raw, &facts) != nil {
		return occurrenceFacts{}
	}
	return facts
}

func writeOccurrenceReadModels(ctx context.Context, tx *sql.Tx, key string, state evidenceState) error {
	if len(state.Occurrences) == 0 {
		return writeTemporalOccurrenceModels(ctx, tx, key, state)
	}
	input := make([]map[string]any, 0, len(state.Occurrences))
	for _, issue := range state.Occurrences {
		issueKey := issue.Key
		if issue.ObservationOnly {
			issueKey = issue.OccurrenceID
		}
		payload := occurrencePayloadFacts(issue.Payload)
		input = append(input, map[string]any{
			"occurrence_id": issue.OccurrenceID, "issue_key": issueKey, "first_seen": issue.FirstSeen,
			"last_seen": issue.LastSeen, "closed_at": issue.ClosedAt, "payload": issue,
			"payload_version": state.OccurrenceHeads[issue.OccurrenceID], "root_uid": issue.UID,
			"namespace": issue.Namespace, "kind": issue.Kind, "rule": issue.Rule,
			"authorization_kind": issue.AuthorizationKind, "broken": issue.Broken, "severity": payload.Severity,
			"broke_at": payload.BrokeAt, "last_observed_at": nullEvidenceTime(state.LastObservations[issue.OccurrenceID]),
			"observation_only": issue.ObservationOnly,
		})
	}
	raw, err := json.Marshal(input)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO kubernetes_evidence_issues AS old
 (scope_key,occurrence_id,issue_key,first_seen,last_seen,closed_at,payload,payload_version,root_uid,namespace,kind,rule,authorization_kind,broken,severity,broke_at,last_observed_at,observation_only,model_ready)
 SELECT $1,x.*,TRUE FROM jsonb_to_recordset($2::jsonb) AS x(
 occurrence_id text,issue_key text,first_seen timestamptz,last_seen timestamptz,closed_at timestamptz,payload jsonb,
 payload_version text,root_uid text,namespace text,kind text,rule text,authorization_kind text,broken boolean,severity text,
 broke_at timestamptz,last_observed_at timestamptz,observation_only boolean)
 ON CONFLICT(scope_key,occurrence_id) DO UPDATE SET
 last_seen=EXCLUDED.last_seen,closed_at=EXCLUDED.closed_at,payload=EXCLUDED.payload,payload_version=EXCLUDED.payload_version,
 root_uid=EXCLUDED.root_uid,namespace=EXCLUDED.namespace,kind=EXCLUDED.kind,rule=EXCLUDED.rule,
 authorization_kind=EXCLUDED.authorization_kind,broken=EXCLUDED.broken,severity=EXCLUDED.severity,
 broke_at=EXCLUDED.broke_at,last_observed_at=EXCLUDED.last_observed_at,observation_only=EXCLUDED.observation_only,model_ready=TRUE
 WHERE old.last_seen<=EXCLUDED.last_seen AND (old.last_seen,old.closed_at,old.payload,old.payload_version,old.last_observed_at,old.model_ready)
 IS DISTINCT FROM (EXCLUDED.last_seen,EXCLUDED.closed_at,EXCLUDED.payload,EXCLUDED.payload_version,EXCLUDED.last_observed_at,TRUE)`, key, raw)
	if err != nil {
		return err
	}
	return writeTemporalOccurrenceModels(ctx, tx, key, state)
}

// writeTemporalOccurrenceModels records each new occurrence version with the
// revision that supersedes it, and supersedes the stored versions it follows.
// Its cost is proportional to the versions this capture adds: every stored
// version is reached through an occurrence's own revision index, and the
// versions of one capture are linked to each other here rather than by
// searching the capture's own rows in SQL. The stored versions to supersede
// are collected first and then updated by key, joined on the scope they were
// found in rather than the bound scope: without statistics for a new scope
// the planner would otherwise start from all of the scope's versions and
// search the capture's versions once for each.
func writeTemporalOccurrenceModels(ctx context.Context, tx *sql.Tx, key string, state evidenceState) error {
	first := make(map[string]occurrenceObservation, len(state.OccurrenceVersions))
	for _, observation := range state.Observations {
		if _, wanted := state.OccurrenceVersions[observation.Version]; !wanted {
			continue
		}
		if prior, found := first[observation.Version]; !found || observation.Revision < prior.Revision {
			first[observation.Version] = observation
		}
	}
	type temporalVersion struct {
		version     string
		issue       EvidenceIssue
		observation occurrenceObservation
	}
	byOccurrence := map[string][]temporalVersion{}
	for version, issue := range state.OccurrenceVersions {
		observation, found := first[version]
		if !found {
			return ErrInvalidEvidence
		}
		byOccurrence[issue.OccurrenceID] = append(byOccurrence[issue.OccurrenceID], temporalVersion{version, issue, observation})
	}
	input := make([]map[string]any, 0, len(state.OccurrenceVersions))
	for _, versions := range byOccurrence {
		sort.Slice(versions, func(i, j int) bool { return versions[i].observation.Revision < versions[j].observation.Revision })
		for i, entry := range versions {
			facts := occurrencePayloadFacts(entry.issue.Payload)
			row := map[string]any{
				"occurrence_id": entry.issue.OccurrenceID, "payload_version": entry.version, "revision": entry.observation.Revision, "observed_at": entry.observation.At,
				"first_seen": entry.issue.FirstSeen, "closed_at": entry.issue.ClosedAt, "root_uid": entry.issue.UID, "namespace": entry.issue.Namespace,
				"kind": entry.issue.Kind, "rule": entry.issue.Rule, "severity": facts.Severity, "authorization_kind": entry.issue.AuthorizationKind,
				"broken": entry.issue.Broken, "observation_only": entry.issue.ObservationOnly, "broke_at": facts.BrokeAt, "payload": entry.issue,
			}
			// The next version of the same occurrence in this capture.
			for _, next := range versions[i+1:] {
				if next.observation.Revision > entry.observation.Revision {
					row["next_revision"], row["next_at"] = next.observation.Revision, next.observation.At
					break
				}
			}
			input = append(input, row)
		}
	}
	if len(input) == 0 {
		return nil
	}
	raw, err := json.Marshal(input)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `WITH incoming AS MATERIALIZED(
	 SELECT * FROM jsonb_to_recordset($2::jsonb) AS x(occurrence_id text,payload_version text,revision bigint,observed_at timestamptz,first_seen timestamptz,closed_at timestamptz,root_uid text,namespace text,kind text,rule text,severity text,authorization_kind text,broken boolean,observation_only boolean,broke_at timestamptz,payload jsonb,next_revision bigint,next_at timestamptz)
	 ), inserted AS(
	 INSERT INTO kubernetes_evidence_occurrence_read_versions(scope_key,occurrence_id,payload_version,revision,observed_at,first_seen,closed_at,root_uid,namespace,kind,rule,severity,authorization_kind,broken,observation_only,broke_at,payload,superseded_revision,superseded_at)
	 SELECT $1,i.occurrence_id,i.payload_version,i.revision,i.observed_at,i.first_seen,i.closed_at,i.root_uid,i.namespace,i.kind,i.rule,i.severity,i.authorization_kind,i.broken,i.observation_only,i.broke_at,i.payload,
	 CASE WHEN n.revision IS NULL OR i.next_revision<n.revision THEN i.next_revision ELSE n.revision END,
	 CASE WHEN n.revision IS NULL OR i.next_revision<n.revision THEN i.next_at ELSE n.observed_at END
	 FROM incoming i LEFT JOIN LATERAL(SELECT revision,observed_at FROM kubernetes_evidence_occurrence_read_versions
	 WHERE scope_key=$1 AND occurrence_id=i.occurrence_id AND revision>i.revision ORDER BY revision LIMIT 1)n ON TRUE
	 ON CONFLICT DO NOTHING RETURNING occurrence_id,revision,observed_at
	 ), touched AS(
	 SELECT occurrence_id,array_agg(revision ORDER BY revision) AS revisions,array_agg(observed_at ORDER BY revision) AS ats FROM inserted GROUP BY occurrence_id
	 ), targets AS MATERIALIZED(
	 SELECT $1::text AS scope_key,c.payload_version,t.revisions,t.ats FROM touched t CROSS JOIN LATERAL(
	 (SELECT payload_version FROM kubernetes_evidence_occurrence_read_versions WHERE scope_key=$1 AND occurrence_id=t.occurrence_id AND revision<t.revisions[1] ORDER BY revision DESC LIMIT 1)
	 UNION ALL
	 (SELECT payload_version FROM kubernetes_evidence_occurrence_read_versions WHERE scope_key=$1 AND occurrence_id=t.occurrence_id AND revision>t.revisions[1] AND revision<t.revisions[cardinality(t.revisions)])
	 )c
	 )
	 UPDATE kubernetes_evidence_occurrence_read_versions old SET (superseded_revision,superseded_at)=(
	 SELECT r.revision,r.at FROM unnest(g.revisions,g.ats) AS r(revision,at) WHERE r.revision>old.revision ORDER BY r.revision LIMIT 1)
	 FROM targets g WHERE old.scope_key=g.scope_key AND old.payload_version=g.payload_version
	 AND (old.superseded_revision IS NULL OR old.superseded_revision>(SELECT MIN(r) FROM unnest(g.revisions) r WHERE r>old.revision))`, key, raw)
	return err
}

// amplifiedWriteStreak is how many consecutive large saves must append most of
// what they were offered before the amplification warning is logged.
const amplifiedWriteStreak = 10

// noteEvidenceWrite warns when saves keep appending most of what captures
// offer. A steady cluster appends almost nothing, so a long streak means some
// producer is re-recording unchanged evidence and history will grow with time.
func (p *postgresProvider) noteEvidenceWrite(stats evidenceWriteStats) {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	if stats.Offered < 100 || stats.Inserted*2 <= stats.Offered {
		p.amplifiedWrites = 0
		return
	}
	p.amplifiedWrites++
	if p.amplifiedWrites%amplifiedWriteStreak == 0 {
		log.Printf("storage: evidence saves keep appending most records offered=%d inserted=%d refreshed=%d expired=%d parts=%d streak=%d", stats.Offered, stats.Inserted, stats.Refreshed, stats.Expired, stats.Parts+1, p.amplifiedWrites)
	}
}

// evidenceUpdates are the write statement's supersede and refresh steps over
// the given relations: the journal, or the monthly partitions that hold the
// events' current versions. A refresh made through a partition is not seen by
// the journal's own triggers, so the statement keeps the scope's observation
// delay itself (in its summary step).
func evidenceUpdates(targets []string) (string, string) {
	if len(targets) == 0 {
		return `, superseded AS (SELECT NULL::bigint AS sequence,NULL::text AS event_type WHERE FALSE)`,
			`, refreshed AS (SELECT NULL::text AS event_id,NULL::timestamptz AS observed_at,NULL::timestamptz AS event_at WHERE $3::double precision IS NULL)`
	}
	supersede, refresh, supersededParts, refreshedParts := "", "", []string{}, []string{}
	for i, target := range targets {
		// A step over one month takes only the rows of that month, so each
		// row is looked up once, in the partition that holds it.
		month, within := "TRUE", ""
		if at, err := time.Parse("200601", strings.TrimSuffix(strings.TrimPrefix(target, `"evidence_event_month_`), `"`)); err == nil {
			month = fmt.Sprintf("c.event_at>='%s' AND c.event_at<'%s'", at.Format(time.RFC3339), at.AddDate(0, 1, 0).Format(time.RFC3339))
			within = " AND " + month
		}
		supersede += fmt.Sprintf(`, superseded_%d AS (
 UPDATE %s old SET superseded_seq=s.next FROM(
 SELECT c.sequence,c.event_at,n.sequence AS next FROM inserted n JOIN current c ON c.event_id=n.event_id WHERE %s OFFSET 0) s
 WHERE old.scope_key=$1 AND old.sequence=s.sequence AND old.event_at=s.event_at AND old.superseded_seq IS NULL
 RETURNING old.sequence,old.event_type)`, i, target, month)
		refresh += fmt.Sprintf(`, refreshed_%d AS (
 UPDATE %s live SET observed_at=r.observed_at,
 payload=CASE WHEN jsonb_typeof(live.payload)='object' AND live.payload ? 'observed_at' THEN jsonb_set(live.payload,'{observed_at}',to_jsonb(r.observed_at)) ELSE live.payload END
 FROM(SELECT c.sequence,c.event_at,i.observed_at FROM incoming i JOIN current c ON c.event_id=i.event_id
 WHERE i.event_type='crash' AND i.event_version=c.event_version AND i.priority=c.priority
 AND i.observed_at>=c.observed_at+make_interval(secs=>$3::double precision)%s OFFSET 0) r
 WHERE live.scope_key=$1 AND live.sequence=r.sequence AND live.event_at=r.event_at AND live.superseded_seq IS NULL
 RETURNING live.event_id,live.observed_at,live.event_at)`, i, target, within)
		supersededParts = append(supersededParts, fmt.Sprintf("SELECT * FROM superseded_%d", i))
		refreshedParts = append(refreshedParts, fmt.Sprintf("SELECT * FROM refreshed_%d", i))
	}
	return supersede + `, superseded AS (` + strings.Join(supersededParts, " UNION ALL ") + `)`,
		refresh + `, refreshed AS (` + strings.Join(refreshedParts, " UNION ALL ") + `)`
}

// pgxIdentifier quotes a partition name for a statement.
func pgxIdentifier(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}
