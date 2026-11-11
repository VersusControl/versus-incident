package cost

import (
	"testing"

	"github.com/VersusControl/versus-incident/pkg/finops"
)

// A disconnected account deleted from settings stops showing its stored cost
// on the very next read, before its purge runs: the removal drops it from
// the unreachable payers and every read leaves tombstoned payers out.
func TestRemovedUnreachablePayerCostIsHiddenOnReadAtOnce(t *testing.T) {
	h, _, svc := natSpike(t, nil)
	payer := h.opts.Payers[0].AccountID
	lostAccess(t, h, h.clock.Now())
	if v, err := svc.Summary(ctx, finops.Query{}); err != nil || !v.Enabled || v.Total == 0 {
		t.Fatalf("stale stored cost must show before the removal: enabled=%v total=%d err=%v", v.Enabled, v.Total, err)
	}

	if err := h.adapter.MarkPayerRemoved(ctx, payer, 1); err != nil {
		t.Fatal(err)
	}
	if got := h.adapter.options().Unreachable; len(got) != 0 {
		t.Fatalf("removed payer still unreachable in the options: %+v", got)
	}
	v, err := svc.Summary(ctx, finops.Query{})
	if err != nil || v.Enabled || v.Reason != ReasonNoConnectedAccounts {
		t.Fatalf("after removal: enabled=%v reason=%q total=%d err=%v", v.Enabled, v.Reason, v.Total, err)
	}

	// Options read before the removal (an older settings snapshot) still
	// name the payer; the tombstone keeps its cost hidden.
	stale := h.adapter.options()
	stale.Unreachable = []UnreachablePayer{{AccountID: payer, Cause: "credentials_invalid", Since: h.clock.Now()}}
	stale.SettingsRevision = 1
	if err := h.adapter.Update(stale); err != nil {
		t.Fatal(err)
	}
	if v, err := svc.Summary(ctx, finops.Query{}); err != nil || v.Enabled {
		t.Fatalf("older options revived a removed payer: enabled=%v total=%d err=%v", v.Enabled, v.Total, err)
	}
}
