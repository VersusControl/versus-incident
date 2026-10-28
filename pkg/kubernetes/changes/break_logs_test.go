package changes

import (
	"errors"
	"github.com/VersusControl/versus-incident/pkg/kubernetes/breaklogs"
	"github.com/VersusControl/versus-incident/pkg/kubernetes/index"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"testing"
	"time"
)

func TestBreakLogsLeaseFencingPersistenceAndScopes(t *testing.T) {
	provider := storage.NewMemory()
	now := time.Now().UTC()
	scope := index.Scope{OrgID: "org", ClusterID: "cluster", CredentialID: "credential"}
	store := NewStore(provider, scope)
	item := breaklogs.Snapshot{Cluster: "cluster", Namespace: "shop", RootUID: "root", Pod: "api-1", PodUID: "pod", Container: "app", Slot: "previous", From: now.Add(-time.Minute), To: now, CapturedAt: now, Lines: []breaklogs.Line{{At: now, Text: "safe"}}}
	item.ID = breaklogs.Identity(item)
	if err := store.ClaimLease("owner", now, time.Minute); err != nil {
		t.Fatal(err)
	}
	for _, owner := range []string{"", "stale"} {
		if err := store.CommitBreakLogs([]breaklogs.Snapshot{item}, nil, "job", owner, now); !errors.Is(err, ErrLeaseLost) {
			t.Fatal("stale write", err)
		}
	}
	if err := store.CommitBreakLogs([]breaklogs.Snapshot{item}, nil, "job", "owner", now); err != nil {
		t.Fatal(err)
	}
	restarted := NewStore(provider, scope)
	state, err := restarted.BreakLogs(now)
	if err != nil || len(state.Snapshots) != 1 || state.Snapshots[0].Lines[0].Text != "safe" {
		t.Fatal("persist", err)
	}
	for _, other := range []index.Scope{{OrgID: "other", ClusterID: "cluster", CredentialID: "credential"}, {OrgID: "org", ClusterID: "other", CredentialID: "credential"}, {OrgID: "org", ClusterID: "cluster", CredentialID: "other"}} {
		state, err := NewStore(provider, other).BreakLogs(now)
		if err != nil || len(state.Snapshots) != 0 {
			t.Fatal("scope leak", err)
		}
	}
}
