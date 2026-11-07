package kubernetes

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/storage"
)

// A Changes page link issued before retention removed the records it points
// into reaches the API as cursor_expired, an invalid argument that the HTTP
// layer answers with 400, so the page restarts at page 1. A malformed link is
// invalid_arguments and never cursor_expired. Without a prune in between the
// same link keeps paging, so the expiry is caused by the prune.
func TestRetentionPrunedChangesLinkAnswersCursorExpired(t *testing.T) {
	for _, prune := range []bool{false, true} {
		t.Run(fmt.Sprintf("prune=%t", prune), func(t *testing.T) {
			provider, err := storage.NewFile(storage.FileOptions{DataDir: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = provider.Close() })
			service := issueHistoryService(t, provider)
			now := time.Now().UTC().Truncate(time.Second)
			oldAt := now.Add(-30 * time.Hour)
			commitInventoryFixtureHistory(t, service, storage.EvidenceBatch{ID: "capture:retention-base", At: oldAt.Add(-time.Minute), Epoch: "fixture"})
			records := []storage.EvidenceRecord{}
			for i := 0; i < 4; i++ {
				at := oldAt.Add(-time.Duration(i+1) * time.Second)
				records = append(records, pagingChangeRecord(t, fmt.Sprintf("old-%d", i), "shop", "root-r", at, oldAt))
			}
			pagingCommitHistory(t, service, "capture:retention-old", oldAt, records)
			query := ChangeQuery{Since: oldAt.Add(-time.Hour), Until: now, Namespace: "shop", Limit: 1}
			page, err := service.Changes(t.Context(), query)
			if err != nil || page.Next == "" {
				t.Fatalf("page 1: next=%q err=%v", page.Next, err)
			}
			laterAt := oldAt.Add(time.Hour)
			if prune {
				laterAt = now
			}
			pagingCommitHistory(t, service, "capture:retention-new", laterAt, []storage.EvidenceRecord{pagingChangeRecord(t, "new-0", "shop", "root-r", laterAt, laterAt)})
			next := query
			next.Cursor = page.Next
			_, err = service.Changes(t.Context(), next)
			if prune {
				if !errors.Is(err, ErrCursorExpired) || !errors.Is(err, ErrInvalidArguments) || DiagnoseError(err).Code != "cursor_expired" {
					t.Fatalf("link issued before the retention prune answered %q (%v), want cursor_expired", DiagnoseError(err).Code, err)
				}
			} else if err != nil {
				t.Fatalf("link with no prune in between answered %q (%v), want the next page", DiagnoseError(err).Code, err)
			}
			bad := query
			bad.Cursor = "not a cursor"
			if _, err = service.Changes(t.Context(), bad); errors.Is(err, ErrCursorExpired) || DiagnoseError(err).Code != "invalid_arguments" {
				t.Fatalf("malformed link answered %q (%v), want invalid_arguments", DiagnoseError(err).Code, err)
			}
		})
	}
}
