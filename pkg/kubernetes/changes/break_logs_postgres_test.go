package changes

import (
	"bytes"
	"errors"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/kubernetes/breaklogs"
	"github.com/VersusControl/versus-incident/pkg/kubernetes/index"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// Proves captured break logs and agent settings survive a fresh Postgres client,
// stay isolated by org, cluster and credential, and reject a stale capture owner.
func TestPostgresBreakLogsPersistIsolateAndFenceStaleOwner(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set")
	}
	open := func() storage.Provider {
		provider, err := storage.NewPostgres(storage.PostgresOptions{DSN: dsn})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = provider.Close() })
		return provider
	}
	first, second := open(), open()
	scope := index.Scope{OrgID: "break-logs-integration", ClusterID: strconv.FormatInt(time.Now().UnixNano(), 10), CredentialID: "reader"}
	leader, follower := NewStore(first, scope), NewStore(second, scope)
	settingsKey := breaklogs.SettingsKey(scope.OrgID, scope.ClusterID)
	t.Cleanup(func() {
		for _, key := range []string{leader.key, settingsKey} {
			current, err := first.ReadBlob(key)
			if errors.Is(err, storage.ErrNotFound) {
				continue
			}
			if err != nil {
				t.Error(err)
				continue
			}
			if swapped, err := first.(storage.BlobCAS).CompareAndSwapBlob(key, current, nil); err != nil || !swapped {
				t.Errorf("cleanup %s: swapped=%v err=%v", key, swapped, err)
			}
		}
	})
	now := time.Now().UTC().Truncate(time.Millisecond)
	if err := leader.ClaimLease("owner-a", now, time.Minute); err != nil {
		t.Fatal(err)
	}
	item := breaklogs.Snapshot{Cluster: scope.ClusterID, Namespace: "shop", RootUID: "root", Pod: "api-7d9f-x2k4q", PodUID: "pod-1", Container: "api", Slot: "previous", TerminationAt: now.Add(-time.Minute), Restarts: 1, CapturedAt: now, From: now.Add(-4 * time.Minute), To: now.Add(-time.Minute), Lines: []breaklogs.Line{{At: now.Add(-3 * time.Minute), Text: "payment client timeout"}, {At: now.Add(-2 * time.Minute), Text: "exiting with code 1"}}}
	item.ID = breaklogs.Identity(item)
	gap := breaklogs.Gap{From: now.Add(-5 * time.Minute), To: now.Add(-4 * time.Minute), Namespace: "shop", RootUID: "root", PodUID: "pod-2", Container: "api", Reason: "forbidden"}
	if err := leader.CommitBreakLogs([]breaklogs.Snapshot{item}, []breaklogs.Gap{gap}, "job-1", "owner-a", now); err != nil {
		t.Fatal(err)
	}
	if err := leader.CommitBreakLogs([]breaklogs.Snapshot{item}, nil, "job-1", "owner-a", now); err != nil {
		t.Fatal(err)
	}
	if err := breaklogs.UpdateSettings(first, settingsKey, func(state *breaklogs.SettingsState) error {
		state.Settings.BreakLogs.LinkedSource = "loki-prod"
		state.Settings.Learning.Logs = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	restarted := NewStore(open(), scope)
	state, err := restarted.BreakLogs(now)
	if err != nil || len(state.Snapshots) != 1 || len(state.Snapshots[0].Lines) != 2 || state.Snapshots[0].Lines[0].Text != "payment client timeout" || len(state.Gaps) != 1 || state.Gaps[0].Reason != "forbidden" {
		t.Fatalf("fresh client lost or duplicated captured logs: %+v err=%v", state, err)
	}
	settings, err := breaklogs.LoadSettings(open(), settingsKey)
	if err != nil || settings.Settings.BreakLogs.LinkedSource != "loki-prod" || !settings.Settings.Learning.Logs {
		t.Fatalf("fresh client lost settings: %+v err=%v", settings, err)
	}

	if err := follower.ClaimLease("owner-b", now.Add(2*time.Minute), time.Minute); err != nil {
		t.Fatal(err)
	}
	before, err := second.ReadBlob(follower.key)
	if err != nil {
		t.Fatal(err)
	}
	stale := item
	stale.Pod, stale.PodUID, stale.CapturedAt = "api-7d9f-stale", "pod-stale", now
	stale.ID = breaklogs.Identity(stale)
	if err := leader.CommitBreakLogs([]breaklogs.Snapshot{stale}, nil, "job-stale", "owner-a", now); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale owner admitted: %v", err)
	}
	if err := leader.CheckCaptureLease("owner-a"); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale owner lease check admitted: %v", err)
	}
	if after, err := second.ReadBlob(follower.key); err != nil || !bytes.Equal(before, after) {
		t.Fatal("stale owner changed captured logs")
	}

	for _, dimension := range []string{"org", "cluster", "credential"} {
		t.Run("isolate-"+dimension, func(t *testing.T) {
			other := scope
			switch dimension {
			case "org":
				other.OrgID += "-other"
			case "cluster":
				other.ClusterID += "-other"
			case "credential":
				other.CredentialID += "-restricted"
			}
			isolated := NewStore(second, other)
			state, err := isolated.BreakLogs(now)
			if err != nil || isolated.key == follower.key || len(state.Snapshots) != 0 || len(state.Gaps) != 0 {
				t.Fatalf("scope inherited captured logs: snapshots=%d gaps=%d err=%v", len(state.Snapshots), len(state.Gaps), err)
			}
			if dimension == "credential" {
				return
			}
			otherSettings, err := breaklogs.LoadSettings(second, breaklogs.SettingsKey(other.OrgID, other.ClusterID))
			if err != nil || otherSettings.Settings.BreakLogs.LinkedSource != "" || otherSettings.Settings.Learning.Logs {
				t.Fatalf("scope inherited settings: %+v err=%v", otherSettings.Settings, err)
			}
		})
	}
}
