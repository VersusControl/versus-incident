package mount

import (
	"context"
	"testing"
	"time"

	awscollect "github.com/VersusControl/versus-incident/pkg/aws/collect"
	awssettings "github.com/VersusControl/versus-incident/pkg/aws/settings"
	awssnapshot "github.com/VersusControl/versus-incident/pkg/aws/snapshot"
	c "github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

// The agent's change list exists only while AWS change collection is on.
func TestChangeListBoundOnlyWhenEnabled(t *testing.T) {
	conn := c.AWSConnectorConfig{Enable: true, Collect: c.AWSCollectConfig{Enable: true}}
	feed := awssnapshot.NewChangeFeed(awssnapshot.New(storage.NewMemory(), tenancy.DefaultOrgScope()))
	bound := changeListFor(func() c.AWSConnectorConfig { return conn }, feed)
	if bound() == nil {
		t.Fatal("enabled connector has no change list")
	}
	conn.Collect.Enable = false
	if bound() != nil {
		t.Fatal("collection off still answers changes")
	}
	conn.Collect.Enable, conn.Enable = true, false
	if bound() != nil {
		t.Fatal("connector off still answers changes")
	}
}

// A selected region the account's health reports as not enabled is listed
// not_enabled with the gap reason region_not_enabled; it never makes the
// answer incomplete and is never read as quiet.
func TestChangeScopeListsNotEnabledRegions(t *testing.T) {
	provider := storage.NewMemory()
	scope := tenancy.DefaultOrgScope()
	settings := awssettings.NewStore(provider, scope)
	const accountID = "111122223333"
	// A fresh opt-in read confirms af-south-1 is off.
	if err := settings.SaveHealth(scope.Write, awssettings.HealthRecord{AccountID: accountID, Status: awssettings.StatusConnected,
		Regions:       []awssettings.RegionStatus{{Region: "us-east-1", Enabled: true}, {Region: "af-south-1", Enabled: false}},
		RegionsReadAt: time.Now(), RegionsFreshUntil: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	conn := c.AWSConnectorConfig{Enable: true, Accounts: []c.AWSAccountConfig{{AccountID: accountID, Regions: []string{"us-east-1", "af-south-1", "eu-west-1"}}}}
	accounts, ok, err := changeScope(settings, scope.Write, conn)
	if err != nil || !ok || len(accounts) != 1 {
		t.Fatalf("scope = %+v, %v, %v", accounts, ok, err)
	}
	if got := accounts[0]; len(got.NotEnabled) != 1 || got.NotEnabled[0] != "af-south-1" || len(got.Regions) != 2 {
		t.Fatalf("account scope = %+v", got)
	}
	feed := awssnapshot.NewChangeFeed(awssnapshot.New(provider, scope),
		awssnapshot.WithScope(func(context.Context) ([]awssnapshot.ScopeAccount, bool, error) { return accounts, true, nil }))
	answer, err := feed.Answer(context.Background(), awssnapshot.FeedRequest{})
	if err != nil {
		t.Fatal(err)
	}
	state := ""
	for _, region := range answer.Regions {
		if region.Region == "af-south-1" {
			state = region.State
		}
	}
	reason := ""
	for _, gap := range answer.Gaps {
		if gap.Region == "af-south-1" {
			reason = gap.Reason
		}
	}
	if state != awssnapshot.RegionNotEnabled || reason != string(awssnapshot.GapRegionNotEnabled) {
		t.Fatalf("af-south-1 state %q reason %q; regions %+v", state, reason, answer.Regions)
	}
}

// A health record without a fresh opt-in read says nothing about which
// regions are off: every selected region is polled and none is listed
// not enabled.
func TestChangeScopeWithoutFreshRegionReadListsNoneOff(t *testing.T) {
	provider := storage.NewMemory()
	scope := tenancy.DefaultOrgScope()
	settings := awssettings.NewStore(provider, scope)
	const accountID = "111122223333"
	if err := settings.SaveHealth(scope.Write, awssettings.HealthRecord{AccountID: accountID, Status: awssettings.StatusConnected,
		Regions: []awssettings.RegionStatus{{Region: "us-east-1", Enabled: true}, {Region: "af-south-1", Enabled: false}}}); err != nil {
		t.Fatal(err)
	}
	conn := c.AWSConnectorConfig{Enable: true, Accounts: []c.AWSAccountConfig{{AccountID: accountID, Regions: []string{"us-east-1", "af-south-1"}}}}
	accounts, _, err := changeScope(settings, scope.Write, conn)
	if err != nil || len(accounts) != 1 || len(accounts[0].NotEnabled) != 0 || len(accounts[0].Regions) != 2 {
		t.Fatalf("scope without a fresh read = %+v, %v", accounts, err)
	}
}

// The collector reads account health from the saved identity check: a
// per-region UnrecognizedClientException is a region that is not enabled
// when the check signs in, a credential failure when it is failing.
func TestCollectAccountHealthFromIdentityCheck(t *testing.T) {
	for _, tc := range []struct {
		record awssettings.HealthRecord
		want   awscollect.AccountHealth
	}{
		{awssettings.HealthRecord{}, awscollect.AccountHealthUnknown},
		{awssettings.HealthRecord{Status: awssettings.StatusConnected, AttemptedAt: time.Now(), AttemptOK: true}, awscollect.AccountHealthHealthy},
		{awssettings.HealthRecord{Status: awssettings.StatusChecking, AttemptedAt: time.Now()}, awscollect.AccountHealthFailing},
		{awssettings.HealthRecord{Status: awssettings.StatusDisconnected}, awscollect.AccountHealthFailing},
	} {
		if got := accountHealthOf(tc.record); got != tc.want {
			t.Errorf("%+v = %d, want %d", tc.record, got, tc.want)
		}
	}
}
