package settings

import (
	"testing"

	"github.com/VersusControl/versus-incident/pkg/aws/model"
)

// TestSyncViewWhenEveryUnitFails: an account whose every unit failed reads
// "failed", never "ok" or "partial", and a first sync that never committed
// anything has no "Synced N s ago" time. Once one unit commits, the account
// is partial and carries that unit's time while the others keep their gaps.
func TestSyncViewWhenEveryUnitFails(t *testing.T) {
	service, _, snapshots := syncService(t, true)
	units := []model.Unit{syncUnit("ec2", "us-east-1"), syncUnit("lambda", "us-east-1")}
	if err := snapshots.RecordPending(units); err != nil {
		t.Fatal(err)
	}
	if err := snapshots.RecordFailure(units[0], "throttled"); err != nil {
		t.Fatal(err)
	}
	if err := snapshots.RecordFailure(units[1], "denied"); err != nil {
		t.Fatal(err)
	}

	view := service.syncView("111122223333")
	if view.State != SyncStateFailed || view.FirstSync || view.UnitsDone != 2 || view.Units != 2 {
		t.Fatalf("all units failed = %+v", view)
	}
	if view.SyncedAt != nil {
		t.Fatalf("an account that never committed claims it synced at %v", view.SyncedAt)
	}
	reasons := map[string]string{}
	for _, entry := range view.Services {
		if entry.State != SyncStateFailed || entry.LastGoodAt != nil || len(entry.Gaps) != 1 {
			t.Fatalf("service %s = %+v", entry.Service, entry)
		}
		reasons[entry.Service] = entry.Gaps[0]
	}
	if reasons["ec2"] != string(model.GapThrottled) || reasons["lambda"] != string(model.GapDenied) {
		t.Fatalf("gap reasons = %v", reasons)
	}

	if _, err := snapshots.Commit(units[0], instanceCandidate()); err != nil {
		t.Fatal(err)
	}
	view = service.syncView("111122223333")
	if view.State != SyncStatePartial || view.SyncedAt == nil {
		t.Fatalf("one unit recovered = %+v", view)
	}
}
