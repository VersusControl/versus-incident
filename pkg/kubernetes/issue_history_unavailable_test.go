package kubernetes

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/storage"
)

// stageTimeoutStore answers like a history store whose reads come back with a
// bare context.DeadlineExceeded at one stage of an occurrence lookup: the
// scope lookup, the scope record, the checkpoint read before the query, or
// the checkpoint read after it. Every other read goes to the real store.
type stageTimeoutStore struct {
	storage.Provider
	storage.EvidenceStore
	stage       string
	scopeKey    string
	hits        *atomic.Int32
	occurrences *atomic.Int32
}

func (store stageTimeoutStore) FindEvidenceScope(ctx context.Context, orgID, clusterID, binding string) (storage.EvidenceScope, error) {
	if store.stage == "scope_lookup" {
		store.hits.Add(1)
		return storage.EvidenceScope{}, context.DeadlineExceeded
	}
	return storage.EvidenceScope{}, storage.ErrUnsupported
}

func (store stageTimeoutStore) ReadBlob(name string) ([]byte, error) {
	if store.stage == "scope_record" && name == store.scopeKey {
		store.hits.Add(1)
		return nil, context.DeadlineExceeded
	}
	return store.Provider.ReadBlob(name)
}

func (store stageTimeoutStore) EvidenceCheckpoint(ctx context.Context, scope storage.EvidenceScope) (storage.EvidenceCheckpoint, error) {
	// Before any occurrence query the read is the pre-query checkpoint; once
	// one has run it is the post-query recheck.
	if store.stage == "checkpoint" && store.occurrences.Load() == 0 || store.stage == "latest_checkpoint" && store.occurrences.Load() > 0 {
		store.hits.Add(1)
		return storage.EvidenceCheckpoint{}, context.DeadlineExceeded
	}
	return store.EvidenceStore.EvidenceCheckpoint(ctx, scope)
}

func (store stageTimeoutStore) QueryEvidence(ctx context.Context, scope storage.EvidenceScope, query storage.EvidenceQuery) (storage.EvidencePage, error) {
	if query.Type == "occurrence" {
		store.occurrences.Add(1)
	}
	return store.EvidenceStore.QueryEvidence(ctx, scope, query)
}

// A deadline that the store returns unwrapped, at any stage of the occurrence
// lookup, degrades the live page instead of failing it.
func TestLiveIssuesDegradeWhenAnyHistoryReadStageTimesOut(t *testing.T) {
	for _, stage := range []string{"scope_lookup", "scope_record", "checkpoint", "latest_checkpoint"} {
		t.Run(stage, func(t *testing.T) {
			service, store, _ := reviewHistoryService(t)
			reviewHistoryCycle(t, service)
			ctx := liveBrokenPodsContext(t.Context(), "api-0", "api-1")
			hits, occurrences := &atomic.Int32{}, &atomic.Int32{}
			service.SetChangeStorage(stageTimeoutStore{Provider: service.usageProvider(), EvidenceStore: store, stage: stage, scopeKey: service.historyScopeKey(), hits: hits, occurrences: occurrences})
			// The cached capture scope would skip the scope reads; drop it so
			// those stages are reached.
			service.changes.mu.Lock()
			delete(service.changes.inventories, service.indexScope())
			service.changes.mu.Unlock()
			page, err := service.Issues(ctx, IssueOptions{})
			if hits.Load() == 0 {
				t.Fatalf("the %s stage was never read; the fixture does not reach it", stage)
			}
			if err != nil {
				t.Fatalf("a bare deadline at the %s stage failed the live page: %v", stage, err)
			}
			history := 0
			for _, failure := range page.Partial {
				if failure.Class == "historical_evidence_unavailable" {
					history++
				}
			}
			if len(page.Items) == 0 || !page.Sync.Partial || history != 1 {
				t.Fatalf("want live items, a partial page and one history failure, got items=%d partial=%t history=%d", len(page.Items), page.Sync.Partial, history)
			}
			for _, item := range page.Items {
				if item.Broken && (!item.Partial || item.OccurrenceID != "" || item.BrokeAt != nil) {
					t.Fatalf("a root kept an occurrence selector after the %s stage timed out: %+v", stage, item)
				}
			}
		})
	}
}

// Incomplete stored history for several roots is still one page-level
// history failure, not one per root.
func TestLiveIssuesReportIncompleteHistoryOncePerPage(t *testing.T) {
	service, store, _ := reviewHistoryService(t)
	reviewHistoryCycle(t, service)
	gap := storage.EvidencePage{Items: []storage.EvidenceRecord{{Type: "coverage_gap", Payload: json.RawMessage(`{}`)}}}
	service.SetChangeStorage(occurrenceLookupFailureStore{Provider: service.usageProvider(), EvidenceStore: store, page: gap})
	pods := []string{"api-0", "api-1", "api-2"}
	page, err := service.Issues(liveBrokenPodsContext(t.Context(), pods...), IssueOptions{})
	if err != nil {
		t.Fatalf("incomplete history failed the live page: %v", err)
	}
	history, broken := 0, 0
	for _, failure := range page.Partial {
		if failure.Class == "historical_evidence_unavailable" {
			history++
		}
	}
	for _, item := range page.Items {
		if !item.Broken {
			continue
		}
		broken++
		if !item.Partial || item.OccurrenceID != "" || item.BrokeAt != nil {
			t.Fatalf("a root with incomplete history kept an occurrence selector: %+v", item)
		}
	}
	if broken != len(pods) || !page.Sync.Partial || history != 1 {
		t.Fatalf("want %d partial roots and one history failure, got broken=%d partial=%t history=%d: %+v", len(pods), broken, page.Sync.Partial, history, page.Partial)
	}
}
