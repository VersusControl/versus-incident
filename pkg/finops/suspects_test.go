package finops

import (
	"context"
	"errors"
	"testing"
)

type failingFinder struct{}

func (failingFinder) Suspect(context.Context, Detected) (SuspectView, *UsageSignal, error) {
	return SuspectView{}, nil, errors.New("store unavailable")
}

type rankedFinder struct{}

func (rankedFinder) Suspect(context.Context, Detected) (SuspectView, *UsageSignal, error) {
	return SuspectView{}, nil, nil
}

func (rankedFinder) Suspects(context.Context, Detected) ([]SuspectView, *UsageSignal, error) {
	var out []SuspectView
	for _, ref := range []string{"a", "b", "c", "d"} {
		out = append(out, SuspectView{State: "found", Summary: ref, Confidence: "possible", Ref: ref})
	}
	return out, nil, nil
}

// TestSuspectStatesFromTheFinder: a finder that cannot read the history
// says so (never "no change found"); a ranker shows at most three.
func TestSuspectStatesFromTheFinder(t *testing.T) {
	src, cfg := natStory()
	svc := NewService(src, newMemStore(), cfg).WithSuspectFinder(failingFinder{})
	if _, err := svc.SyncIncreases(context.Background()); err != nil {
		t.Fatal(err)
	}
	list, err := svc.Increases(context.Background(), Query{Period: "2026-10", Status: "all"})
	if err != nil || len(list.Items) == 0 {
		t.Fatal(err)
	}
	for _, it := range list.Items {
		if it.Suspect.State != "history_incomplete" || it.Suspect.Note == "" || len(it.Suspects) != 0 {
			t.Fatalf("finder error: %+v", it.Suspect)
		}
	}
	svc = NewService(src, newMemStore(), cfg).WithSuspectFinder(rankedFinder{})
	if _, err := svc.SyncIncreases(context.Background()); err != nil {
		t.Fatal(err)
	}
	list, _ = svc.Increases(context.Background(), Query{Period: "2026-10", Status: "all"})
	it := list.Items[0]
	if len(it.Suspects) != MaxSuspects || it.Suspect.Ref != "a" || it.Suspects[2].Ref != "c" || it.Suspects[0].ConfidenceWord != "Possible" {
		t.Fatalf("ranked: %+v", it.Suspects)
	}
}
