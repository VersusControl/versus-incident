package kubernetes

import (
	"testing"
	"time"

	kubechanges "github.com/VersusControl/versus-incident/pkg/kubernetes/changes"
	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

func TestCaptureOlderCachedGenerationDoesNotRegressHandoffCoverage(t *testing.T) {
	service := NewService(nil, Scope{ClusterID: "cluster"}, 0)
	service.SetChangeStorage(storage.NewMemory())
	now := time.Now().UTC()
	checkpoint := now.Add(-time.Second)
	store := service.changeStore()
	if err := store.ClaimLease("owner", now, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := store.Commit(nil, nil, map[string]kubechanges.Gap{"Pod": {From: checkpoint, To: checkpoint}}, "owner", now); err != nil {
		t.Fatal(err)
	}
	loop := &captureLoop{ctx: t.Context(), owner: "owner", owned: true, status: CaptureStatus{Mode: "continuous"}, gapFrom: map[string]time.Time{"Pod": checkpoint}}
	service.changes.captures[service.indexScope()] = loop
	view := kubeindex.Snapshot{Kinds: map[string]kubeindex.KindStatus{"Pod": {State: "ready", ObservedAt: now.Add(-5 * time.Second)}}}
	if err := service.captureCycle(store, nil, view, []string{"Pod"}, now, "owner"); err != nil {
		t.Fatalf("older last-good generation made handoff invalid: %v", err)
	}
	page, err := store.ReadWindow(now.Add(-time.Minute), now, now)
	if err != nil || !page.Coverage["Pod"].To.Equal(checkpoint) || len(page.Gaps) != 0 {
		t.Fatalf("coverage regressed or fabricated a gap: %+v %v", page, err)
	}
	if loop.status.Behind {
		t.Fatal("valid already-covered cached observation marked owner behind")
	}
}
