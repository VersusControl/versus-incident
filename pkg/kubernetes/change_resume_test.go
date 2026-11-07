package kubernetes

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/storage"
)

// A caller that shows only part of a Changes page (the assistant trims pages
// to a byte budget) resumes saved history right after the last change it
// showed: nothing is repeated and nothing is skipped.
func TestChangesResumeAfterThePartOfAPageACallerShowed(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	at := now.Add(-30 * time.Minute)
	service := issueHistoryService(t, storage.NewMemory())
	commitInventoryFixtureHistory(t, service, storage.EvidenceBatch{ID: "capture:resume-base", At: now, Epoch: "fixture"})
	records := []storage.EvidenceRecord{}
	for position := 0; position < 9; position++ {
		records = append(records, pagingChangeRecord(t, fmt.Sprintf("resume-%d", position), "shop", "root-r", at.Add(time.Duration(position)*time.Minute), now))
	}
	pagingCommitHistory(t, service, "capture:resume", now, records)
	all, err := service.Changes(t.Context(), ChangeQuery{Namespace: "shop", Since: at.Add(-time.Minute), Until: now, Limit: 9})
	if err != nil || len(all.Items) != 9 {
		t.Fatalf("full list: items=%d err=%v", len(all.Items), err)
	}
	page, err := service.Changes(t.Context(), ChangeQuery{Namespace: "shop", Since: at.Add(-time.Minute), Until: now, Limit: 5})
	if err != nil || len(page.Items) != 5 || page.Next == "" {
		t.Fatalf("page 1: items=%d next=%q err=%v", len(page.Items), page.Next, err)
	}
	if page.ResumeAfter(len(page.Items)) != page.Next {
		t.Fatal("resuming after every shown item is not the page's own next cursor")
	}
	shown := []string{}
	for _, change := range page.Items[:2] {
		shown = append(shown, change.ID)
	}
	for cursor := page.ResumeAfter(2); cursor != ""; {
		next, err := service.Changes(t.Context(), ChangeQuery{Namespace: "shop", Limit: 5, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		for _, change := range next.Items {
			shown = append(shown, change.ID)
		}
		cursor = next.Next
	}
	want := []string{}
	for _, change := range all.Items {
		want = append(want, change.ID)
	}
	if strings.Join(shown, ",") != strings.Join(want, ",") {
		t.Fatalf("resumed walk %v, want %v", shown, want)
	}
}
