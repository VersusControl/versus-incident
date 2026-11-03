package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

func queryCurrentOccurrenceLookup(ctx context.Context, tx *sql.Tx, keys []string, query EvidenceQuery, page EvidencePage) (EvidencePage, error) {
	args := []any{keys}
	where := `i.scope_key=ANY($1::text[])`
	if query.OccurrenceID != "" {
		args = append(args, query.OccurrenceID)
		where += ` AND i.occurrence_id=$2`
	} else {
		args = append(args, query.RootUID)
		where += ` AND i.root_uid=$2`
		if query.Namespace != "" {
			args = append(args, query.Namespace)
			where += fmt.Sprintf(` AND i.namespace=$%d`, len(args))
		}
		if !query.Center.IsZero() {
			args = append(args, query.Center)
			where += fmt.Sprintf(` AND i.broke_at=$%d`, len(args))
		}
	}
	if len(keys) > 1 {
		where += ` AND(i.scope_key=($1::text[])[1] OR NOT EXISTS(SELECT 1 FROM kubernetes_evidence_issues higher WHERE higher.scope_key=($1::text[])[1] AND higher.occurrence_id=i.occurrence_id))`
	}
	allowed, err := evidenceAuthoritySQL(query, &args, "i.authorization_kind", "i.namespace")
	if err != nil {
		return page, err
	}
	limit := 1
	if query.OccurrenceID == "" && !query.Center.IsZero() {
		limit = 2
	}
	args = append(args, limit)
	rows, err := tx.QueryContext(ctx, `SELECT i.occurrence_id,i.first_seen,i.last_seen,COALESCE(`+allowed+`,FALSE),CASE WHEN `+allowed+` THEN i.payload ELSE '{}'::jsonb END
 FROM kubernetes_evidence_issues i WHERE `+where+fmt.Sprintf(` ORDER BY i.last_seen DESC,i.occurrence_id DESC LIMIT $%d`, len(args)), args...)
	if err != nil {
		return page, err
	}
	for rows.Next() {
		var id string
		var first, seen time.Time
		var authorized bool
		var raw []byte
		if err = rows.Scan(&id, &first, &seen, &authorized, &raw); err != nil {
			rows.Close()
			return page, err
		}
		record := EvidenceRecord{ID: id, At: first, ObservedAt: seen}
		if authorized {
			var issue EvidenceIssue
			if err = json.Unmarshal(raw, &issue); err != nil {
				rows.Close()
				return page, err
			}
			record = occurrenceRecord(issue, occurrenceObservation{At: seen, LastSeen: seen})
			record.At = first
		} else {
			record = projectEvidenceAuthority(record, EvidenceQuery{EnforceAuthority: true})
		}
		page.Items = append(page.Items, record)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return page, err
	}
	rows.Close()
	return page, tx.Commit()
}
