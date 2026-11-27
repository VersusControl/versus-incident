package storage

import "context"

// EvidenceSubject names one root an occurrence read is narrowed to, and the
// namespace it lives in when it has one.
type EvidenceSubject struct {
	RootUID, Namespace string
}

// EvidenceSubjectReader answers the first page of one occurrence read for
// several roots at once. Page i equals what QueryEvidence answers for the
// query narrowed to subjects[i] (RootUID and Namespace set from it).
type EvidenceSubjectReader interface {
	QueryEvidenceSubjects(context.Context, EvidenceScope, EvidenceQuery, []EvidenceSubject) ([]EvidencePage, error)
}

// evidenceSubjectsBatchable reports whether a read can be answered for many
// subjects in one statement: a first page of current occurrences, without
// totals, narrowed only by the subject and the shared filters.
func evidenceSubjectsBatchable(q EvidenceQuery) bool {
	return q.Type == "occurrence" && !q.PointInTime && !q.Lookup && q.Cursor == "" && q.ItemsOnly && q.OccurrenceID == "" && q.Center.IsZero() &&
		q.RootUID == "" && q.UID == "" && q.ResourceUID == "" && q.Namespace == "" && q.Name == "" && q.PodUID == ""
}

// QueryEvidenceSubjects reads the first page of an occurrence read for each
// subject. A store that reads them together does so in one read; any other
// store is asked once per subject. Either way page i is the page of
// subjects[i].
func QueryEvidenceSubjects(ctx context.Context, store EvidenceStore, scope EvidenceScope, query EvidenceQuery, subjects []EvidenceSubject) ([]EvidencePage, error) {
	if reader, ok := store.(EvidenceSubjectReader); ok && evidenceSubjectsBatchable(query) && len(subjects) > 0 {
		return reader.QueryEvidenceSubjects(ctx, scope, query, subjects)
	}
	pages := make([]EvidencePage, len(subjects))
	for i, subject := range subjects {
		q := query
		q.RootUID, q.Namespace = subject.RootUID, subject.Namespace
		page, err := store.QueryEvidence(ctx, scope, q)
		if err != nil {
			return nil, err
		}
		pages[i] = page
	}
	return pages, nil
}
