package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
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

func writeEvidenceEvents(ctx context.Context, tx *sql.Tx, key string, records []EvidenceRecord) (uint64, error) {
	if len(records) == 0 {
		return 0, nil
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
		return 0, err
	}
	var high uint64
	err = tx.QueryRowContext(ctx, `WITH incoming AS (
 SELECT DISTINCT ON(event_id) * FROM jsonb_to_recordset($2::jsonb) AS x(
 event_id text,event_version text,event_type text,event_at timestamptz,observed_at timestamptz,
 namespace text,resource_name text,kind text,authorization_kind text,resource_uid text,root_uid text,
 pod_uid text,rule text,epoch text,payload jsonb,is_break boolean,priority integer,gap_from timestamptz,gap_to timestamptz)
 ORDER BY event_id,priority DESC,observed_at DESC
 ), inserted AS (
 INSERT INTO kubernetes_evidence_events(scope_key,event_id,event_version,event_type,event_at,observed_at,namespace,resource_name,kind,authorization_kind,resource_uid,root_uid,pod_uid,rule,epoch,payload,is_break,priority,gap_from,gap_to)
 SELECT $1,i.* FROM incoming i WHERE NOT EXISTS(
 SELECT 1 FROM kubernetes_evidence_events p WHERE p.scope_key=$1 AND p.event_id=i.event_id AND
 (p.priority>i.priority OR(p.priority=i.priority AND p.observed_at>i.observed_at)))
 ON CONFLICT DO NOTHING RETURNING sequence,event_id,event_type
 ), superseded AS (
 UPDATE kubernetes_evidence_events old SET superseded_seq=n.sequence FROM inserted n
 WHERE old.scope_key=$1 AND old.event_id=n.event_id AND old.sequence<n.sequence AND old.superseded_seq IS NULL
 RETURNING old.sequence,old.event_type
 ), deltas AS (
 SELECT event_type,SUM(delta)::bigint AS delta FROM(
 SELECT n.event_type,1::bigint AS delta FROM inserted n JOIN kubernetes_evidence_scopes s ON s.scope_key=$1 WHERE s.read_model_ready
 UNION ALL SELECT old.event_type,-1::bigint FROM superseded old JOIN kubernetes_evidence_scopes s ON s.scope_key=$1 WHERE s.read_model_ready OR old.sequence<=s.backfill_sequence
 ) changes GROUP BY event_type
 ), summary AS (
 UPDATE kubernetes_evidence_scopes s SET event_counts=s.event_counts||COALESCE(
 (SELECT jsonb_object_agg(d.event_type,GREATEST(0,COALESCE((s.event_counts->>d.event_type)::bigint,0)+d.delta)) FROM deltas d),'{}'::jsonb)
 WHERE s.scope_key=$1 RETURNING s.scope_key
 ) SELECT COALESCE(MAX(sequence),0) FROM inserted`, key, raw).Scan(&high)
	return high, err
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
		var payload struct {
			Severity string     `json:"severity"`
			BrokeAt  *time.Time `json:"broke_at"`
		}
		if err := json.Unmarshal(issue.Payload, &payload); err != nil {
			return err
		}
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

func writeTemporalOccurrenceModels(ctx context.Context, tx *sql.Tx, key string, state evidenceState) error {
	input := []map[string]any{}
	for version, issue := range state.OccurrenceVersions {
		var first occurrenceObservation
		found := false
		for _, observation := range state.Observations {
			if observation.Version == version && (!found || observation.Revision < first.Revision) {
				first, found = observation, true
			}
		}
		if !found {
			return ErrInvalidEvidence
		}
		var facts struct {
			Severity string     `json:"severity"`
			BrokeAt  *time.Time `json:"broke_at"`
		}
		if err := json.Unmarshal(issue.Payload, &facts); err != nil {
			return err
		}
		input = append(input, map[string]any{
			"occurrence_id": issue.OccurrenceID, "payload_version": version, "revision": first.Revision, "observed_at": first.At,
			"first_seen": issue.FirstSeen, "closed_at": issue.ClosedAt, "root_uid": issue.UID, "namespace": issue.Namespace,
			"kind": issue.Kind, "rule": issue.Rule, "severity": facts.Severity, "authorization_kind": issue.AuthorizationKind,
			"broken": issue.Broken, "observation_only": issue.ObservationOnly, "broke_at": facts.BrokeAt, "payload": issue,
		})
	}
	if len(input) == 0 {
		return nil
	}
	raw, err := json.Marshal(input)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `WITH incoming AS(
	 SELECT * FROM jsonb_to_recordset($2::jsonb) AS x(occurrence_id text,payload_version text,revision bigint,observed_at timestamptz,first_seen timestamptz,closed_at timestamptz,root_uid text,namespace text,kind text,rule text,severity text,authorization_kind text,broken boolean,observation_only boolean,broke_at timestamptz,payload jsonb)
	 ), inserted AS(
	 INSERT INTO kubernetes_evidence_occurrence_read_versions(scope_key,occurrence_id,payload_version,revision,observed_at,first_seen,closed_at,root_uid,namespace,kind,rule,severity,authorization_kind,broken,observation_only,broke_at,payload,superseded_revision,superseded_at)
	 SELECT $1,i.*,n.revision,n.observed_at FROM incoming i LEFT JOIN LATERAL(
	 SELECT revision,observed_at FROM(
	 SELECT revision,observed_at FROM kubernetes_evidence_occurrence_read_versions WHERE scope_key=$1 AND occurrence_id=i.occurrence_id AND revision>i.revision
	 UNION ALL SELECT revision,observed_at FROM incoming WHERE occurrence_id=i.occurrence_id AND revision>i.revision
	 ) future ORDER BY revision LIMIT 1)n ON TRUE
	 ON CONFLICT DO NOTHING RETURNING occurrence_id,revision,observed_at
	 ), prior AS(
	 SELECT old.payload_version,MIN(n.revision) AS revision FROM kubernetes_evidence_occurrence_read_versions old JOIN inserted n ON n.occurrence_id=old.occurrence_id AND n.revision>old.revision
	 WHERE old.scope_key=$1 AND (old.superseded_revision IS NULL OR old.superseded_revision>n.revision) GROUP BY old.payload_version
	 )
	 UPDATE kubernetes_evidence_occurrence_read_versions old SET superseded_revision=prior.revision,superseded_at=n.observed_at FROM prior JOIN inserted n ON n.revision=prior.revision
	 WHERE old.scope_key=$1 AND old.payload_version=prior.payload_version AND old.occurrence_id=n.occurrence_id`, key, raw)
	return err
}
