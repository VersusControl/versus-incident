package settings

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/limit"
	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/aws/snapshot"
	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
	"github.com/VersusControl/versus-incident/tests/fakeaws"
)

func collectingConfig(collect bool, accounts ...config.AWSAccountConfig) func() config.AWSConnectorConfig {
	return func() config.AWSConnectorConfig {
		return config.AWSConnectorConfig{
			Enable: true, HealthCheckInterval: "5m", Accounts: accounts,
			Budgets: config.AWSBudgetsConfig{MaxRequestsPerSecond: 1000},
			Collect: config.AWSCollectConfig{Enable: collect},
		}
	}
}

func syncService(t *testing.T, collect bool, accounts ...config.AWSAccountConfig) (*Service, *Store, *snapshot.Store) {
	t.Helper()
	fake := fakeaws.New(t, "new-account-connect")
	provider := storage.NewMemory()
	store := NewStore(provider, tenancy.DefaultOrgScope())
	snapshots := snapshot.New(provider, tenancy.DefaultOrgScope())
	service := NewService(store, WithConfig(collectingConfig(collect, accounts...)), WithHTTPClient(fake.HTTPClient()),
		WithBaseProvider(staticBase), WithLimiter(limit.NewManager(100)), WithSnapshots(snapshots))
	return service, store, snapshots
}

func syncUnit(service, region string) model.Unit {
	return model.Unit{Account: "111122223333", Region: region, Service: service}
}

func instanceCandidate() snapshot.Candidate {
	return snapshot.Candidate{Resources: []model.Resource{{ARN: "arn:aws:ec2:us-east-1:111122223333:instance/i-1", Service: "ec2", Type: "instance", Account: "111122223333", Region: "us-east-1"}}}
}

func TestSyncViewProgressesFromPendingToPartial(t *testing.T) {
	service, _, snapshots := syncService(t, true)
	if view := service.syncView("111122223333"); view.State != SyncStatePending || !view.FirstSync || view.Services == nil {
		t.Fatalf("no units = %+v", view)
	}
	units := []model.Unit{syncUnit("ec2", "us-east-1"), syncUnit("ec2", "eu-west-1"), syncUnit("lambda", "us-east-1")}
	snapshots.RecordPending(units)
	if view := service.syncView("111122223333"); view.State != SyncStatePending || view.Units != 3 || view.UnitsDone != 0 {
		t.Fatalf("pending = %+v", view)
	}
	snapshots.Commit(units[0], instanceCandidate())
	view := service.syncView("111122223333")
	if view.State != SyncStateSyncing || !view.FirstSync || view.UnitsDone != 1 || view.SyncedAt == nil {
		t.Fatalf("first sync = %+v", view)
	}
	if view.Services[0].Service != "ec2" || view.Services[0].State != SyncStateSyncing || view.Services[0].UnitsDone != 1 || view.Services[0].Units != 2 {
		t.Fatalf("ec2 progress = %+v", view.Services[0])
	}
	snapshots.Commit(units[1], snapshot.Candidate{})
	snapshots.RecordFailure(units[2], "denied")
	view = service.syncView("111122223333")
	if view.State != SyncStatePartial || view.FirstSync || view.UnitsDone != 3 {
		t.Fatalf("partial = %+v", view)
	}
	lambda := view.Services[1]
	if lambda.State != SyncStateFailed || len(lambda.Gaps) != 1 || lambda.Gaps[0] != string(model.GapDenied) || lambda.LastGoodAt != nil {
		t.Fatalf("lambda = %+v", lambda)
	}
	snapshots.Commit(units[2], snapshot.Candidate{})
	if view := service.syncView("111122223333"); view.State != SyncStateOK {
		t.Fatalf("all good = %+v", view)
	}
}

func TestSyncViewIsOffWithoutCollection(t *testing.T) {
	service, _, _ := syncService(t, false)
	if view := service.syncView("111122223333"); view.State != SyncStateOff || view.Services == nil {
		t.Fatalf("collection off = %+v", view)
	}
	bare, _, _ := fakeService(t, "new-account-connect")
	if view := bare.syncView("111122223333"); view.State != SyncStateOff {
		t.Fatalf("no snapshot store = %+v", view)
	}
}

func TestCollectTargetsSkipsUnusableAccounts(t *testing.T) {
	service, store, _ := syncService(t, true,
		config.AWSAccountConfig{AccountID: "111122223333", Regions: []string{"us-east-1"}, Auth: config.AWSAuthConfig{Mode: "default_chain"}},
		config.AWSAccountConfig{AccountID: "444455556666", Regions: []string{"eu-west-1"}, Auth: config.AWSAuthConfig{Mode: "default_chain"}},
		config.AWSAccountConfig{AccountID: "777788889999", Auth: config.AWSAuthConfig{Mode: "default_chain"}},
	)
	if err := store.SaveHealth("default", HealthRecord{AccountID: "444455556666", Status: StatusDisconnected, AttemptedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	targets := service.CollectTargets(context.Background())
	if len(targets) != 1 || targets[0].AccountID != "111122223333" || targets[0].Credentials == nil || targets[0].Regions[0] != "us-east-1" {
		t.Fatalf("targets = %+v", targets)
	}
	if allowlist := service.TagValueAllowlist(); len(allowlist) == 0 {
		t.Fatal("no tag allowlist")
	}
}

// An account set to "auto" regions has none until its first health check.
// The collectors wait for them, but cost is global and is read through the
// account at once. A disconnected account is never collected; cost lists it
// with its cause and no credentials, so its stored cost stays visible.
func TestCostTargetsIncludeAutoRegionAccountBeforeFirstHealthCheck(t *testing.T) {
	service, store, _ := syncService(t, true,
		config.AWSAccountConfig{AccountID: "111122223333", Auth: config.AWSAuthConfig{Mode: "default_chain"}},
		config.AWSAccountConfig{AccountID: "444455556666", Auth: config.AWSAuthConfig{Mode: "default_chain"}},
	)
	if err := store.SaveHealth("default", HealthRecord{AccountID: "444455556666", Status: StatusDisconnected, AttemptedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if targets := service.CollectTargets(ctx); len(targets) != 0 {
		t.Fatalf("collectors wait for regions: %+v", targets)
	}
	targets := service.CostTargets(ctx)
	if len(targets) != 2 || targets[0].AccountID != "111122223333" || targets[0].Credentials == nil || targets[0].Cause != "" ||
		targets[1].AccountID != "444455556666" || targets[1].Credentials != nil || targets[1].Cause != CauseAccountDisconnected {
		t.Fatalf("cost targets = %+v", targets)
	}
}

// A lost account's cause: credentials that worked before and are now
// rejected read credentials_invalid; a server that never had an identity
// reads no_server_identity; anything else account_disconnected. Since is
// when the account disconnected.
func TestCostTargetsNameWhyALostAccountCannotBeRead(t *testing.T) {
	since := time.Date(2026, 11, 11, 6, 25, 0, 0, time.UTC)
	cases := []struct {
		name   string
		health HealthRecord
		want   string
	}{
		{"expired after working", HealthRecord{ErrorCode: "no_server_identity", CheckedAt: since.Add(-time.Hour)}, CauseCredentialsInvalid},
		{"never had an identity", HealthRecord{ErrorCode: "no_server_identity"}, CauseNoServerIdentity},
		{"role trust removed", HealthRecord{ErrorCode: "trust_denied", CheckedAt: since.Add(-time.Hour)}, CauseAccountDisconnected},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			service, store, _ := syncService(t, true,
				config.AWSAccountConfig{AccountID: "111122223333", Auth: config.AWSAuthConfig{Mode: "default_chain"}})
			h := c.health
			h.AccountID, h.Status, h.DisconnectedSince, h.AttemptedAt = "111122223333", StatusDisconnected, since, since
			if err := store.SaveHealth("default", h); err != nil {
				t.Fatal(err)
			}
			targets := service.CostTargets(context.Background())
			if len(targets) != 1 || targets[0].Cause != c.want || !targets[0].Since.Equal(since) || targets[0].Credentials != nil {
				t.Fatalf("cost targets = %+v", targets)
			}
		})
	}
}

func TestSettingsViewCarriesSync(t *testing.T) {
	service, _, snapshots := syncService(t, true, config.AWSAccountConfig{AccountID: "111122223333", Regions: []string{"us-east-1"}, Auth: config.AWSAuthConfig{Mode: "default_chain"}})
	snapshots.Commit(syncUnit("ec2", "us-east-1"), instanceCandidate())
	view, err := service.Connection(context.Background(), Principal{Org: "default", CanWrite: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Accounts) != 1 || view.Accounts[0].Sync.State != SyncStateOK || view.Accounts[0].Sync.SyncedAt == nil {
		t.Fatalf("account sync = %+v", view.Accounts)
	}
}

func TestCommitTriggersFirstSync(t *testing.T) {
	added := make(chan string, 1)
	connector, _, _ := fakeService(t, "new-account-connect")
	WithAccountAdded(func(accountID string) { added <- accountID })(connector)
	ctx := context.Background()
	start, err := connector.CreateDraft(ctx, "default", "cloudformation")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := connector.Verify("default", start.DraftID, "arn:aws:iam::444455556666:role/versus-reader"); err != nil {
		t.Fatal(err)
	}
	waitForVerified(t, connector, start.DraftID)
	if _, err := connector.DiscoverRegions(ctx, "default", start.DraftID); err != nil {
		t.Fatal(err)
	}
	if _, err := connector.Commit(ctx, "default", start.DraftID, "Staging", []string{"us-east-1"}); err != nil {
		t.Fatal(err)
	}
	select {
	case accountID := <-added:
		if accountID != "444455556666" {
			t.Fatalf("hook account = %s", accountID)
		}
	default:
		t.Fatal("commit did not start the first sync")
	}
}

// TestDeleteAndRegionRemovalPurgeSnapshots: deleting a wizard account and
// dropping one of its regions remove the stored data immediately.
func TestDeleteAndRegionRemovalPurgeSnapshots(t *testing.T) {
	service, store, snapshots := syncService(t, true)
	record := AccountRecord{AccountID: "111122223333", DisplayName: "Prod", Regions: []string{"eu-west-1", "us-east-1"}, Source: SourceWizard, CreatedAt: time.Now()}
	if err := store.SaveAccount("default", record); err != nil {
		t.Fatal(err)
	}
	store.SaveHealth("default", HealthRecord{AccountID: "111122223333", Status: StatusConnected, Regions: []RegionStatus{{Region: "us-east-1", Enabled: true}, {Region: "eu-west-1", Enabled: true}}})
	snapshots.Commit(syncUnit("ec2", "us-east-1"), instanceCandidate())
	snapshots.Commit(syncUnit("ec2", "eu-west-1"), snapshot.Candidate{})

	if _, err := service.UpdateAccount("default", "111122223333", "", []string{"us-east-1"}); err != nil {
		t.Fatal(err)
	}
	if regions, _ := snapshots.Regions("111122223333"); len(regions) != 1 || regions[0] != "us-east-1" {
		t.Fatalf("regions after update = %v", regions)
	}
	if _, err := service.DeleteAccount(context.Background(), "default", "111122223333"); err != nil {
		t.Fatal(err)
	}
	if accounts, _ := snapshots.Accounts(); len(accounts) != 0 {
		t.Fatalf("snapshots after delete = %v", accounts)
	}
	result, _ := snapshots.Resources(context.Background(), snapshot.Query{})
	if len(result.Resources) != 0 {
		t.Fatalf("deleted account still read: %+v", result.Resources)
	}
	if ids, err := service.AccountIDs(context.Background()); err != nil || len(ids) != 0 {
		t.Fatalf("account ids = %v, %v", ids, err)
	}
}

// TestUnreadableAccountRecordBlocksPurgeList: a record that cannot be
// decoded makes the account list incomplete, so the list used for purging
// fails instead of silently omitting that account.
func TestUnreadableAccountRecordBlocksPurgeList(t *testing.T) {
	service, store, _ := syncService(t, true)
	if err := store.SaveAccount("default", AccountRecord{AccountID: "111122223333", DisplayName: "Prod", Regions: []string{"us-east-1"}, Source: SourceWizard, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if ids, err := service.AccountIDs(context.Background()); err != nil || len(ids) != 1 {
		t.Fatalf("readable list = %v, %v", ids, err)
	}
	if err := store.provider.WriteBlob(AccountPrefix("default")+"444455556666", []byte("{not json")); err != nil {
		t.Fatal(err)
	}
	if _, err := service.AccountIDs(context.Background()); !errors.Is(err, ErrUnreadableAccounts) {
		t.Fatalf("list with an unreadable record = %v", err)
	}
	// The settings view still lists what it can read.
	if accounts, err := store.ListAccounts("default"); err != nil || len(accounts) != 1 {
		t.Fatalf("tolerant list = %v, %v", accounts, err)
	}
	// A deleted record (empty blob) is not unreadable.
	store.provider.WriteBlob(AccountPrefix("default")+"444455556666", nil)
	if ids, err := service.AccountIDs(context.Background()); err != nil || len(ids) != 1 {
		t.Fatalf("list after delete = %v, %v", ids, err)
	}
}

// A service with no endpoint in one of the account's regions is shown as
// not available there, not as a failure: the service and the account read
// as synced, and the gap names the region gap in words.
func TestSyncViewShowsAServiceMissingInARegionAsNotAvailable(t *testing.T) {
	service, _, snapshots := syncService(t, true)
	missing := syncUnit("appregistry", "ap-southeast-5")
	present := syncUnit("appregistry", "us-east-1")
	snapshots.RecordPending([]model.Unit{missing, present})
	snapshots.Commit(present, snapshot.Candidate{})
	gap := model.Gap{Account: missing.Account, Region: missing.Region, Service: missing.Service, Reason: model.GapNotInRegion}
	if err := snapshots.RecordSuccess(missing, []model.Gap{gap}); err != nil {
		t.Fatal(err)
	}
	view := service.syncView("111122223333")
	if view.State != SyncStateOK || len(view.Services) != 1 {
		t.Fatalf("account = %+v, want ok", view)
	}
	appregistry := view.Services[0]
	if appregistry.State != SyncStateOK || len(appregistry.Gaps) != 1 || appregistry.Gaps[0] != "not_available_in_region" {
		t.Fatalf("appregistry = %+v, want ok with the not-available gap", appregistry)
	}
}
