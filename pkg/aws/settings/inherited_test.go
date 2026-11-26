package settings

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/VersusControl/versus-incident/pkg/aws/cost"
	"github.com/VersusControl/versus-incident/pkg/aws/limit"
	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/aws/snapshot"
	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/finops"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

// A deployment that adds a licence writes under its licensed organization
// and reads the open-source default organization after it. These tests
// connect accounts and save settings as the open-source build does, then
// read and change them through a licensed scope over the same storage, and
// finally read them again as the open-source build would after the licence
// lapses.

var inheritedAccountSeq atomic.Uint64

// inheritedAccountID is an account ID unique to one run, so Postgres runs
// sharing the default organization never meet each other's accounts.
func inheritedAccountID() string {
	n := uint64(time.Now().UnixNano()/1000) + inheritedAccountSeq.Add(1)
	return fmt.Sprintf("%012d", n%1_000_000_000_000)
}

type licenceFixture struct {
	provider storage.Provider
	licensed string
	oss      *Store
	store    *Store
	service  *Service

	mu      sync.Mutex
	assumed map[string]string // role ARN -> external ID it was assumed with
}

func newLicenceFixture(t *testing.T, candidate backend) *licenceFixture {
	t.Helper()
	provider := candidate.provider(t)
	fixture := &licenceFixture{
		provider: provider,
		licensed: "acme-" + strconv.FormatInt(time.Now().UnixNano(), 36),
		oss:      NewStore(provider, tenancy.DefaultOrgScope()),
		assumed:  map[string]string{},
	}
	fixture.store = NewStore(provider, tenancy.NewOrgScope(fixture.licensed, tenancy.DefaultOrgID))
	assume := func(_ context.Context, base aws.CredentialsProvider, roleARN, externalID, _ string) (aws.CredentialsProvider, error) {
		fixture.mu.Lock()
		fixture.assumed[roleARN] = externalID
		fixture.mu.Unlock()
		return base, nil
	}
	fixture.service = NewService(fixture.store, WithConfig(collectingConfig(false)), WithBaseProvider(staticBase),
		WithAssumeRole(assume), WithLimiter(limit.NewManager(100)))
	return fixture
}

// connectInOSS stores an account the way the open-source wizard does, with
// a connected health record.
func (fixture *licenceFixture) connectInOSS(t *testing.T, name string) AccountRecord {
	t.Helper()
	id := inheritedAccountID()
	record := AccountRecord{
		AccountID:   id,
		DisplayName: name,
		RoleARN:     "arn:aws:iam::" + id + ":role/versus-reader",
		ExternalID:  "vrs-ext-" + id + "abcdefghijkl",
		Regions:     []string{"us-east-1"},
		CreatedAt:   time.Now().UTC().Truncate(time.Second),
	}
	if err := fixture.oss.SaveAccount(tenancy.DefaultOrgID, record); err != nil {
		t.Fatal(err)
	}
	if err := fixture.oss.SaveHealth(tenancy.DefaultOrgID, HealthRecord{AccountID: id, Status: StatusConnected, CheckedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	return record
}

func (fixture *licenceFixture) defaultBlobs(t *testing.T) map[string][]byte {
	t.Helper()
	blobs, err := fixture.provider.ListBlobs("aws/" + tenancy.DefaultOrgID + "/")
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[string][]byte, len(blobs))
	for _, blob := range blobs {
		out[blob.Name] = blob.Data
	}
	return out
}

func sameBlobs(left, right map[string][]byte) bool {
	if len(left) != len(right) {
		return false
	}
	for name, data := range left {
		other, ok := right[name]
		if !ok || !bytes.Equal(data, other) {
			return false
		}
	}
	return true
}

func listedIDs(t *testing.T, store *Store, org string) []string {
	t.Helper()
	accounts, err := store.ListAccountsStrict(org)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(accounts))
	for _, account := range accounts {
		ids = append(ids, account.AccountID)
	}
	return ids
}

func targetIDs(targets []CollectTarget) []string {
	ids := make([]string, 0, len(targets))
	for _, target := range targets {
		ids = append(ids, target.AccountID)
	}
	return ids
}

// An account connected before the licence is still on the Settings page,
// in the connected-account list and collected after it, through the role
// and external ID it was connected with.
func TestInheritedAccountStaysConnectedAfterALicence(t *testing.T) {
	for _, candidate := range revisionBackends() {
		t.Run(candidate.name, func(t *testing.T) {
			fixture := newLicenceFixture(t, candidate)
			record := fixture.connectInOSS(t, "Prod")
			ctx := context.Background()

			view, err := fixture.service.Connection(ctx, Principal{Org: fixture.licensed, CanWrite: true, Agent: true})
			if err != nil {
				t.Fatal(err)
			}
			var row *AccountView
			for i := range view.Accounts {
				if view.Accounts[i].AccountID == record.AccountID {
					row = &view.Accounts[i]
				}
			}
			if row == nil || row.DisplayName != "Prod" || row.Status != StatusConnected {
				t.Fatalf("settings rows = %+v; want the inherited account, connected", view.Accounts)
			}
			ids, err := fixture.service.AccountIDs(ctx)
			if err != nil || !slices.Contains(ids, record.AccountID) {
				t.Fatalf("connected accounts = %v, %v", ids, err)
			}
			if targets := fixture.service.CollectTargets(ctx); !slices.Contains(targetIDs(targets), record.AccountID) {
				t.Fatalf("collect targets = %v; want the inherited account", targetIDs(targets))
			}
			fixture.mu.Lock()
			externalID, assumed := fixture.assumed[record.RoleARN]
			fixture.mu.Unlock()
			if !assumed || externalID != record.ExternalID {
				t.Fatalf("assumed roles = %v; want %s with its stored external ID", fixture.assumed, record.RoleARN)
			}
		})
	}
}

// Editing, removing and reconnecting an inherited account under the licence
// changes only the licensed organization: the default organization's
// records are never written. When the licence lapses, the open-source build
// reads the default organization as it was before the licence.
func TestLicensedChangesNeverWriteTheInheritedAccounts(t *testing.T) {
	for _, candidate := range revisionBackends() {
		t.Run(candidate.name, func(t *testing.T) {
			fixture := newLicenceFixture(t, candidate)
			edited := fixture.connectInOSS(t, "Prod")
			removed := fixture.connectInOSS(t, "Staging")
			ctx := context.Background()
			before := fixture.defaultBlobs(t)

			// Edit: the licensed copy answers and keeps the credentials.
			if _, err := fixture.service.UpdateAccount(fixture.licensed, edited.AccountID, "Production", nil); err != nil {
				t.Fatalf("edit inherited account: %v", err)
			}
			got, err := fixture.store.GetAccount(fixture.licensed, edited.AccountID)
			if err != nil || got.DisplayName != "Production" || got.RoleARN != edited.RoleARN || got.ExternalID != edited.ExternalID {
				t.Fatalf("licensed account = %+v, %v", got, err)
			}
			if _, state, _ := fixture.store.storedAccount(fixture.licensed, edited.AccountID); state != accountStored {
				t.Fatal("the edit was not stored under the licensed organization")
			}

			// Delete: gone from Settings, the connected list and collection.
			if _, err := fixture.service.DeleteAccount(ctx, fixture.licensed, removed.AccountID); err != nil {
				t.Fatalf("delete inherited account: %v", err)
			}
			if ids := listedIDs(t, fixture.store, fixture.licensed); slices.Contains(ids, removed.AccountID) {
				t.Fatalf("removed account still listed: %v", ids)
			}
			if _, err := fixture.store.GetAccount(fixture.licensed, removed.AccountID); err != ErrNotFound {
				t.Fatalf("removed account read = %v, want ErrNotFound", err)
			}
			if ids, err := fixture.service.AccountIDs(ctx); err != nil || slices.Contains(ids, removed.AccountID) {
				t.Fatalf("removed account still connected: %v, %v", ids, err)
			}
			if targets := fixture.service.CollectTargets(ctx); slices.Contains(targetIDs(targets), removed.AccountID) {
				t.Fatalf("removed account still collected: %v", targetIDs(targets))
			}
			if !sameBlobs(before, fixture.defaultBlobs(t)) {
				t.Fatal("a licensed edit or removal wrote the default organization's records")
			}

			// Reconnect: the wizard stores it again and it is back.
			reconnected := removed
			reconnected.DisplayName = "Staging again"
			if err := fixture.store.SaveAccount(fixture.licensed, reconnected); err != nil {
				t.Fatal(err)
			}
			got, err = fixture.store.GetAccount(fixture.licensed, removed.AccountID)
			if err != nil || got.DisplayName != "Staging again" {
				t.Fatalf("reconnected account = %+v, %v", got, err)
			}
			if ids, _ := fixture.service.AccountIDs(ctx); !slices.Contains(ids, removed.AccountID) {
				t.Fatalf("reconnected account not connected: %v", ids)
			}

			// A licensed-only account, then the licence lapses.
			added := inheritedAccountID()
			if err := fixture.store.SaveAccount(fixture.licensed, AccountRecord{AccountID: added, DisplayName: "Licensed"}); err != nil {
				t.Fatal(err)
			}
			if !sameBlobs(before, fixture.defaultBlobs(t)) {
				t.Fatal("a licensed reconnect or add wrote the default organization's records")
			}
			lapsed := NewStore(fixture.provider, tenancy.DefaultOrgScope())
			ids := listedIDs(t, lapsed, tenancy.DefaultOrgID)
			if !slices.Contains(ids, edited.AccountID) || !slices.Contains(ids, removed.AccountID) || slices.Contains(ids, added) {
				t.Fatalf("after a lapse the default organization lists %v; want the two accounts it held, not the licensed one", ids)
			}
			if got, err := lapsed.GetAccount(tenancy.DefaultOrgID, edited.AccountID); err != nil || got.DisplayName != "Prod" {
				t.Fatalf("after a lapse the edited account reads %+v, %v; want its open-source name", got, err)
			}
		})
	}
}

// A change to one privacy field under the licence keeps the tag-value
// allowlist the deployment saved before it.
func TestLicensedPrivacyChangeKeepsTheInheritedAllowlist(t *testing.T) {
	for _, candidate := range revisionBackends() {
		t.Run(candidate.name, func(t *testing.T) {
			fixture := newLicenceFixture(t, candidate)
			saved := []string{"team-" + fixture.licensed, "env"}
			if err := fixture.oss.UpdatePrivacy(tenancy.DefaultOrgID, func(privacy *privacyRecord) {
				privacy.TagValueAllowlist = saved
				privacy.Saved = true
			}); err != nil {
				t.Fatal(err)
			}
			if got := fixture.service.TagValueAllowlist(); !slices.Equal(got, SanitizeAllowlist(saved)) {
				t.Fatalf("allowlist after the licence = %v, want the saved %v", got, saved)
			}
			if err := fixture.store.UpdatePrivacy(fixture.licensed, func(privacy *privacyRecord) {
				privacy.Samples = nil
			}); err != nil {
				t.Fatal(err)
			}
			if got := fixture.service.TagValueAllowlist(); !slices.Equal(got, SanitizeAllowlist(saved)) {
				t.Fatalf("allowlist after a licensed privacy change = %v, want the saved %v", got, saved)
			}
			own, err := fixture.provider.ReadBlob(PrivacyKey(fixture.licensed))
			if err != nil || len(own) == 0 {
				t.Fatalf("the licensed change was not stored under the licensed organization: %v", err)
			}
		})
	}
}

// The licensed revision starts above every revision the default
// organization reached, and still grows with each licensed change.
func TestLicensedAccountsRevisionStartsAboveTheInheritedOne(t *testing.T) {
	for _, candidate := range revisionBackends() {
		t.Run(candidate.name, func(t *testing.T) {
			fixture := newLicenceFixture(t, candidate)
			record := fixture.connectInOSS(t, "Prod")
			if err := fixture.oss.RemoveAccount(tenancy.DefaultOrgID, record.AccountID, nil); err != nil {
				t.Fatal(err)
			}
			inherited, err := fixture.oss.AccountsRevision(tenancy.DefaultOrgID)
			if err != nil || inherited < 2 {
				t.Fatalf("default revision = %d, %v", inherited, err)
			}
			start, err := fixture.store.AccountsRevision(fixture.licensed)
			if err != nil || start < inherited {
				t.Fatalf("licensed revision = %d, %v; want at least the inherited %d", start, err, inherited)
			}
			if err := fixture.store.SaveAccount(fixture.licensed, record); err != nil {
				t.Fatal(err)
			}
			after, err := fixture.store.AccountsRevision(fixture.licensed)
			if err != nil || after != start+1 || after <= inherited {
				t.Fatalf("licensed revision after a save = %d, %v; want %d", after, err, start+1)
			}
			var purged []uint64
			if err := fixture.store.RemoveAccount(fixture.licensed, record.AccountID, func(revision uint64) error {
				purged = append(purged, revision)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if len(purged) != 1 || purged[0] != after {
				t.Fatalf("removal purged at %v; want the revision %d it read", purged, after)
			}
			if again, _ := fixture.oss.AccountsRevision(tenancy.DefaultOrgID); again != inherited {
				t.Fatalf("the default revision moved from %d to %d under the licence", inherited, again)
			}
		})
	}
}

// inheritedReaders reads one account back through every reader the AWS
// pages and agent tools use: the snapshot store's resources and health, its change listing and the change feed, and the cost source.
type inheritedReaders struct {
	snapshots *snapshot.Store
	costs     *cost.Adapter
	connected func(context.Context) (map[string]bool, bool)
	revision  func() uint64
	since     time.Time
	until     time.Time
}

// shows names the readers that return something for account.
func (readers inheritedReaders) shows(t *testing.T, account string) []string {
	t.Helper()
	ctx := context.Background()
	var out []string
	resources, err := readers.snapshots.Resources(ctx, snapshot.Query{})
	if err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(resources.Resources, func(resource model.Resource) bool { return resource.Account == account }) {
		out = append(out, "resources")
	}
	health, err := readers.snapshots.Health(ctx, snapshot.Query{})
	if err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(health.Health, func(item model.Health) bool { return item.Account == account }) {
		out = append(out, "health")
	}
	listed, err := readers.snapshots.Changes(ctx, snapshot.Query{}, readers.since, readers.until)
	if err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(listed.Changes, func(change model.Change) bool { return change.Account == account }) {
		out = append(out, "changes")
	}
	answer, err := snapshot.NewChangeFeed(readers.snapshots).Answer(ctx, snapshot.FeedRequest{Since: readers.since, Until: readers.until})
	if err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(answer.Items, func(item snapshot.FeedItem) bool { return item.Account == account }) {
		out = append(out, "feed")
	}
	// The cost source serves the connected payers, as the cost refresh
	// sets them from the settings listing.
	var payers []cost.Payer
	if connected, ok := readers.connected(ctx); ok {
		for id := range connected {
			payers = append(payers, cost.Payer{AccountID: id})
		}
	}
	if err := readers.costs.Update(cost.Options{Payers: payers, SettingsRevision: readers.revision()}); err != nil {
		t.Fatal(err)
	}
	day, _ := finops.ParseDate("2026-01-01")
	lines, err := readers.costs.Source().Lines(ctx, finops.DateRange{Start: day, End: day})
	if err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(lines, func(line finops.CostLine) bool { return line.Account == account }) {
		out = append(out, "cost")
	}
	return out
}

var allInheritedReaders = []string{"resources", "health", "changes", "feed", "cost"}

// seedInOSS stores what the open-source build collected for an account:
// one resource with its health, one change and one day of cost.
func (fixture *licenceFixture) seedInOSS(t *testing.T, account string, at time.Time) {
	t.Helper()
	oss := snapshot.New(fixture.provider, tenancy.DefaultOrgScope())
	unit := model.Unit{Account: account, Region: "us-east-1", Service: "ec2"}
	candidate := snapshot.Candidate{
		Resources: []model.Resource{{ARN: "arn:aws:ec2:us-east-1:" + account + ":instance/i-1", Service: "ec2", Type: "instance", Account: account, Region: "us-east-1"}},
		Health:    []model.Health{{Resource: "arn:aws:ec2:us-east-1:" + account + ":instance/i-1", Account: account, Region: "us-east-1", Source: "ec2", Status: model.HealthOK, Summary: "running"}},
	}
	if _, err := oss.Commit(unit, candidate); err != nil {
		t.Fatal(err)
	}
	change := model.Change{ID: "event-" + account, Time: at, Account: account, Region: "us-east-1", Service: "ec2", EventName: "RunInstances"}
	if err := oss.AppendChanges(account, "us-east-1", []model.Change{change}, nil, ""); err != nil {
		t.Fatal(err)
	}
	tables, ok := storage.FinOps(fixture.provider)
	if !ok {
		t.Fatal("provider has no FinOps tables")
	}
	line := storage.FinOpsCostLine{Day: "2026-01-01", Account: account, Service: "compute", ServiceName: "Amazon EC2", RecordType: "Usage", NetAmortizedMicros: 10}
	if err := tables.ReplaceFinOpsCostDays(context.Background(), tenancy.DefaultOrgScope(), account, []string{"2026-01-01"}, []storage.FinOpsCostLine{line}); err != nil {
		t.Fatal(err)
	}
}

// An account removed under the licence stops showing what the default
// organization collected for it on every page reader and agent tool; a
// connected inherited account keeps showing it; reconnecting the removed
// one shows it again; and a connected-account list that cannot be read
// hides nothing.
func TestRemovedAccountHidesItsInheritedRows(t *testing.T) {
	for _, candidate := range revisionBackends() {
		t.Run(candidate.name, func(t *testing.T) {
			fixture := newLicenceFixture(t, candidate)
			removed := fixture.connectInOSS(t, "Staging")
			kept := fixture.connectInOSS(t, "Prod")
			at := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
			fixture.seedInOSS(t, removed.AccountID, at)
			fixture.seedInOSS(t, kept.AccountID, at)

			scope := fixture.store.Scope()
			var service *Service
			connected := func(ctx context.Context) (map[string]bool, bool) { return service.ConnectedAccountSet(ctx) }
			snaps := snapshot.New(fixture.provider, scope, snapshot.WithConnectedAccounts(connected))
			costs, err := cost.New(fixture.provider, scope, cost.Options{})
			if err != nil {
				t.Fatal(err)
			}
			service = NewService(fixture.store, WithConfig(collectingConfig(true)), WithBaseProvider(staticBase),
				WithLimiter(limit.NewManager(100)), WithSnapshots(snaps), WithPayerRemover(costs))
			revision := func() uint64 {
				revision, err := service.AccountsRevision(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				return revision
			}
			readers := inheritedReaders{snapshots: snaps, costs: costs, connected: connected, revision: revision, since: at.Add(-time.Hour), until: at.Add(time.Hour)}

			if got := readers.shows(t, removed.AccountID); !slices.Equal(got, allInheritedReaders) {
				t.Fatalf("before the removal the account shows on %v, want %v", got, allInheritedReaders)
			}
			if _, err := service.DeleteAccount(context.Background(), fixture.licensed, removed.AccountID); err != nil {
				t.Fatal(err)
			}
			if got := readers.shows(t, removed.AccountID); len(got) != 0 {
				t.Fatalf("after the removal the account still shows on %v", got)
			}
			if got := readers.shows(t, kept.AccountID); !slices.Equal(got, allInheritedReaders) {
				t.Fatalf("the connected inherited account shows on %v, want %v", got, allInheritedReaders)
			}

			// A list that cannot be read hides nothing.
			unknown := inheritedReaders{snapshots: snapshot.New(fixture.provider, scope, snapshot.WithConnectedAccounts(
				func(context.Context) (map[string]bool, bool) { return nil, false })), costs: costs, connected: connected, revision: revision, since: readers.since, until: readers.until}
			if got := unknown.shows(t, removed.AccountID); !slices.Equal(got, allInheritedReaders[:4]) {
				t.Fatalf("with the connected list unknown the account shows on %v, want every snapshot reader", got)
			}
			// A record another writer stored unreadably; its write raised
			// the revision, as every account write does.
			if err := fixture.provider.WriteBlob(AccountKey(fixture.licensed, "999999999999"), []byte("not json")); err != nil {
				t.Fatal(err)
			}
			if _, err := fixture.store.raiseRevision(fixture.licensed); err != nil {
				t.Fatal(err)
			}
			if _, ok := service.ConnectedAccountSet(context.Background()); ok {
				t.Fatal("an unreadable account record still answers a connected list")
			}
			got := readers.shows(t, removed.AccountID)
			if !slices.Equal(got, allInheritedReaders[:4]) {
				t.Fatalf("with an unreadable settings record the account shows on %v, want every snapshot reader", got)
			}
			if err := fixture.provider.WriteBlob(AccountKey(fixture.licensed, "999999999999"), []byte{}); err != nil {
				t.Fatal(err)
			}

			// Reconnect: the inherited rows show again.
			if err := fixture.store.SaveAccount(fixture.licensed, removed); err != nil {
				t.Fatal(err)
			}
			if got := readers.shows(t, removed.AccountID); !slices.Equal(got, allInheritedReaders) {
				t.Fatalf("after reconnecting the account shows on %v, want %v", got, allInheritedReaders)
			}
		})
	}
}

// A removed account's inherited health never comes back: after removal and
// reconnection under the licence it reads as never checked, and the first
// failed check counts from zero instead of disconnecting it at once.
func TestReconnectedAccountStartsWithCleanHealth(t *testing.T) {
	for _, candidate := range revisionBackends() {
		t.Run(candidate.name, func(t *testing.T) {
			fixture := newLicenceFixture(t, candidate)
			record := fixture.connectInOSS(t, "Staging")
			if err := fixture.oss.SaveHealth(tenancy.DefaultOrgID, HealthRecord{AccountID: record.AccountID, Status: StatusChecking,
				ConsecutiveFailures: 1, ErrorCode: "assume_role_failed", FailureSince: time.Now().UTC()}); err != nil {
				t.Fatal(err)
			}
			if got, err := fixture.store.GetHealth(fixture.licensed, record.AccountID); err != nil || got.ConsecutiveFailures != 1 {
				t.Fatalf("inherited health before the removal = %+v, %v; want the default organization's", got, err)
			}
			if _, err := fixture.service.DeleteAccount(context.Background(), fixture.licensed, record.AccountID); err != nil {
				t.Fatal(err)
			}
			if err := fixture.store.SaveAccount(fixture.licensed, record); err != nil {
				t.Fatal(err)
			}
			health, err := fixture.store.GetHealth(fixture.licensed, record.AccountID)
			if err != nil || health.Status != StatusUnknown || health.ConsecutiveFailures != 0 || health.ErrorCode != "" {
				t.Fatalf("health after reconnecting = %+v, %v; want never checked", health, err)
			}
			failing := NewService(fixture.store, WithConfig(collectingConfig(false)), WithBaseProvider(staticBase),
				WithAssumeRole(func(_ context.Context, base aws.CredentialsProvider, _, _, _ string) (aws.CredentialsProvider, error) {
					return base, nil
				}),
				WithIdentity(func(context.Context, aws.CredentialsProvider, string) (string, string, error) {
					return "", "", errors.New("denied")
				}),
				WithLimiter(limit.NewManager(100)))
			checked, err := failing.CheckAccount(context.Background(), record)
			if err != nil || checked.ConsecutiveFailures != 1 || checked.Status == StatusDisconnected {
				t.Fatalf("first check after reconnecting = %+v, %v; want one failure, not disconnected", checked, err)
			}
		})
	}
}

// With the connector off the connected list is still the stored listing
// with its removals, so an account removed under the licence stays hidden.
func TestConnectorOffKeepsRemovedAccountsHidden(t *testing.T) {
	for _, candidate := range revisionBackends() {
		t.Run(candidate.name, func(t *testing.T) {
			fixture := newLicenceFixture(t, candidate)
			removed := fixture.connectInOSS(t, "Staging")
			kept := fixture.connectInOSS(t, "Prod")
			if _, err := fixture.service.DeleteAccount(context.Background(), fixture.licensed, removed.AccountID); err != nil {
				t.Fatal(err)
			}
			off := NewService(fixture.store, WithConfig(func() config.AWSConnectorConfig { return config.AWSConnectorConfig{} }))
			set, ok := off.ConnectedAccountSet(context.Background())
			if !ok || set[removed.AccountID] || !set[kept.AccountID] {
				t.Fatalf("connected with the connector off = %v, %v; want the kept account and not the removed one", set, ok)
			}
		})
	}
}

// An account added and removed under the licence alone leaves nothing that
// hides it later: after a lapse, the open-source build connects it again,
// and when the licence returns the account shows.
func TestLicensedOnlyRemovalDoesNotHideALaterInheritedAccount(t *testing.T) {
	for _, candidate := range revisionBackends() {
		t.Run(candidate.name, func(t *testing.T) {
			fixture := newLicenceFixture(t, candidate)
			id := inheritedAccountID()
			record := AccountRecord{AccountID: id, DisplayName: "Licensed", Regions: []string{"us-east-1"}}
			if err := fixture.store.SaveAccount(fixture.licensed, record); err != nil {
				t.Fatal(err)
			}
			if _, err := fixture.service.DeleteAccount(context.Background(), fixture.licensed, id); err != nil {
				t.Fatal(err)
			}
			record.DisplayName = "Open source"
			if err := fixture.oss.SaveAccount(tenancy.DefaultOrgID, record); err != nil {
				t.Fatal(err)
			}
			relicensed := NewStore(fixture.provider, tenancy.NewOrgScope(fixture.licensed, tenancy.DefaultOrgID))
			if got, err := relicensed.GetAccount(fixture.licensed, id); err != nil || got.DisplayName != "Open source" {
				t.Fatalf("account after re-licensing = %+v, %v; want the open-source one", got, err)
			}
			if ids := listedIDs(t, relicensed, fixture.licensed); !slices.Contains(ids, id) {
				t.Fatalf("listing after re-licensing = %v; want %s", ids, id)
			}
		})
	}
}

// countingProvider counts the account listings the settings store makes:
// each starts with the write organization's account prefix.
type countingProvider struct {
	storage.Provider
	write    string
	mu       sync.Mutex
	listings int
}

func (provider *countingProvider) ListBlobs(prefix string) ([]storage.Blob, error) {
	if prefix == AccountPrefix(provider.write) {
		provider.mu.Lock()
		provider.listings++
		provider.mu.Unlock()
	}
	return provider.Provider.ListBlobs(prefix)
}

func (provider *countingProvider) take() int {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	count := provider.listings
	provider.listings = 0
	return count
}

// A page read over many accounts lists the connected accounts at most once,
// and not at all while no account changed; an account change lists again.
func TestConnectedListIsReadOncePerChange(t *testing.T) {
	for _, candidate := range revisionBackends() {
		t.Run(candidate.name, func(t *testing.T) {
			fixture := newLicenceFixture(t, candidate)
			at := time.Now().UTC().Add(-time.Hour)
			for i := range 10 {
				record := fixture.connectInOSS(t, "Account "+strconv.Itoa(i))
				fixture.seedInOSS(t, record.AccountID, at)
			}
			counting := &countingProvider{Provider: fixture.provider, write: fixture.licensed}
			scope := fixture.store.Scope()
			store := NewStore(counting, scope)
			service := NewService(store, WithConfig(collectingConfig(false)))
			var asked atomic.Int64
			snaps := snapshot.New(fixture.provider, scope, snapshot.WithConnectedAccounts(func(ctx context.Context) (map[string]bool, bool) {
				asked.Add(1)
				return service.ConnectedAccountSet(ctx)
			}))
			ctx := context.Background()
			read := func() int {
				counting.take()
				asked.Store(0)
				if result, err := snaps.Resources(ctx, snapshot.Query{}); err != nil || len(result.Resources) < 10 {
					t.Fatalf("resources = %d, %v", len(result.Resources), err)
				}
				if got := asked.Load(); got != 1 {
					t.Fatalf("one resources read asked for the connected accounts %d times, want once", got)
				}
				asked.Store(0)
				if _, err := snaps.Coverage(ctx, snapshot.CoverageScope{}); err != nil {
					t.Fatal(err)
				}
				if got := asked.Load(); got != 1 {
					t.Fatalf("one coverage read asked for the connected accounts %d times, want once", got)
				}
				return counting.take()
			}
			if got := read(); got > 1 {
				t.Fatalf("a resources and a coverage read listed the accounts %d times, want at most once", got)
			}
			if got := read(); got != 0 {
				t.Fatalf("an unchanged revision listed the accounts %d times, want none", got)
			}
			if err := store.SaveAccount(fixture.licensed, AccountRecord{AccountID: inheritedAccountID(), DisplayName: "New"}); err != nil {
				t.Fatal(err)
			}
			counting.take()
			if got := read(); got != 1 {
				t.Fatalf("after an account change the reads listed the accounts %d times, want once", got)
			}
		})
	}
}
