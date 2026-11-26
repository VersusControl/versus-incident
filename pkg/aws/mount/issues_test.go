package mount

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/VersusControl/versus-incident/pkg/aws/issues"
	"github.com/VersusControl/versus-incident/pkg/aws/model"
	awssettings "github.com/VersusControl/versus-incident/pkg/aws/settings"
	awssnapshot "github.com/VersusControl/versus-incident/pkg/aws/snapshot"
	c "github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

// The mount builds the issue evaluator over the provider's issue tables,
// and a committed or failed read marks it due; the change poller's writes
// do not.
func TestMountedIssuesFollowCollectorWrites(t *testing.T) {
	const account = "123400000005"
	store := storage.NewMemory()
	mounted := mountForTest(t, fiber.New(), Deps{Store: store})
	if mounted.Issues == nil || mounted.IssueActions == nil {
		t.Fatal("no issue evaluator was mounted over a provider with the issue tables")
	}
	if mounted.Issues.Due() {
		t.Fatal("due before any write")
	}
	unit := model.Unit{Account: account, Region: "eu-west-1", Service: "s3"}
	if _, err := mounted.Snapshots.Commit(unit, awssnapshot.Candidate{}); err != nil {
		t.Fatal(err)
	}
	if !mounted.Issues.Due() {
		t.Fatal("a committed read did not mark the evaluator due")
	}
	report, err := mounted.Issues.Evaluate(context.Background())
	if err != nil || len(report.Accounts) != 1 || report.Accounts[0].Account != account {
		t.Fatalf("evaluation = %+v, %v", report, err)
	}
	if mounted.Issues.Due() {
		t.Fatal("still due after the evaluation")
	}
	if err := mounted.Snapshots.RecordFailure(unit, "denied"); err != nil {
		t.Fatal(err)
	}
	if !mounted.Issues.Due() {
		t.Fatal("a failed read did not mark the evaluator due")
	}
}

// The evaluator's connected accounts come from the connector: none known
// while it is off, and each configured account with the Regions it is read
// in once it is on.
func TestMountedIssuesReadConnectedAccounts(t *testing.T) {
	const account = "123400000009"
	store := storage.NewMemory()
	scope := tenancy.DefaultOrgScope()
	config := c.AWSConnectorConfig{Accounts: []c.AWSAccountConfig{{AccountID: account, Regions: []string{"eu-west-1", "us-east-1"}}}}
	read := connectedAccounts(awssettings.NewStore(store, scope), scope, func() c.AWSConnectorConfig { return config }, time.Now)
	if _, err := read(context.Background()); !errors.Is(err, issues.ErrConnectedUnknown) {
		t.Fatalf("connector off = %v", err)
	}
	config.Enable = true
	got, err := read(context.Background())
	if err != nil || len(got) != 1 || got[0].Account != account || len(got[0].Regions) != 2 || got[0].Regions[1] != "us-east-1" {
		t.Fatalf("connected = %+v, %v", got, err)
	}
}

// An account connected through the setup wizard before a licence is stored
// under the default organization. After the licence it is still connected:
// the list holds the accounts of every organization in the read scope, so
// its issues are not resolved as "account no longer connected".
func TestConnectedAccountsSpanTheReadScope(t *testing.T) {
	const wizard, licensed = "123400000011", "123400000012"
	store := storage.NewMemory()
	community := tenancy.DefaultOrgScope()
	if err := awssettings.NewStore(store, community).SaveAccount(community.Write, awssettings.AccountRecord{AccountID: wizard}); err != nil {
		t.Fatal(err)
	}
	scope := tenancy.NewOrgScope("acme", tenancy.DefaultOrgID)
	if err := awssettings.NewStore(store, scope).SaveAccount("acme", awssettings.AccountRecord{AccountID: licensed}); err != nil {
		t.Fatal(err)
	}
	config := c.AWSConnectorConfig{Enable: true}
	got, err := connectedAccounts(awssettings.NewStore(store, scope), scope, func() c.AWSConnectorConfig { return config }, time.Now)(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, account := range got {
		seen[account.Account] = true
	}
	if len(got) != 2 || !seen[wizard] || !seen[licensed] {
		t.Fatalf("connected = %+v; want the wizard account and the licensed one", got)
	}
}

// Under a licence the evaluator reads an inherited account's Regions from
// the health the checks keep for the licensed organization, not the
// default organization's last record, and an inherited account removed
// under the licence is no longer connected.
func TestConnectedAccountsUseLicensedHealthAndRemovals(t *testing.T) {
	const kept, removed = "123400000021", "123400000022"
	store := storage.NewMemory()
	community := awssettings.NewStore(store, tenancy.DefaultOrgScope())
	for _, id := range []string{kept, removed} {
		if err := community.SaveAccount(tenancy.DefaultOrgID, awssettings.AccountRecord{AccountID: id}); err != nil {
			t.Fatal(err)
		}
		if err := community.SaveHealth(tenancy.DefaultOrgID, awssettings.HealthRecord{AccountID: id, Status: awssettings.StatusConnected,
			Regions: []awssettings.RegionStatus{{Region: "us-east-1", Enabled: true}}}); err != nil {
			t.Fatal(err)
		}
	}
	scope := tenancy.NewOrgScope("acme", tenancy.DefaultOrgID)
	licensed := awssettings.NewStore(store, scope)
	if err := licensed.SaveHealth("acme", awssettings.HealthRecord{AccountID: kept, Status: awssettings.StatusConnected,
		Regions: []awssettings.RegionStatus{{Region: "eu-west-1", Enabled: true}}}); err != nil {
		t.Fatal(err)
	}
	if err := licensed.RemoveAccount("acme", removed, nil); err != nil {
		t.Fatal(err)
	}
	config := c.AWSConnectorConfig{Enable: true}
	got, err := connectedAccounts(licensed, scope, func() c.AWSConnectorConfig { return config }, time.Now)(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Account != kept || len(got[0].Regions) != 1 || got[0].Regions[0] != "eu-west-1" {
		t.Fatalf("connected = %+v; want only %s, read in eu-west-1", got, kept)
	}
}

// Removing the last account marks the evaluator due, so its open issues
// are resolved as no longer connected by the next evaluation, without a
// collector write or a restart.
func TestRemovingTheLastAccountResolvesItsIssues(t *testing.T) {
	const account = "123400000031"
	store := storage.NewMemory()
	scope := tenancy.DefaultOrgScope()
	cfg := &c.Config{}
	cfg.Connectors.AWS.Enable = true
	mounted := mountForTest(t, fiber.New(), Deps{Store: store, Config: cfg, Scope: scope})
	if mounted.Issues == nil {
		t.Fatal("no issue evaluator was mounted")
	}
	if err := awssettings.NewStore(store, scope).SaveAccount(scope.Write, awssettings.AccountRecord{AccountID: account, Source: awssettings.SourceWizard}); err != nil {
		t.Fatal(err)
	}
	issueStore, _ := storage.AWSIssuesOf(store)
	at := time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC)
	owner := "arn:aws:s3:::public-bucket"
	open := storage.AWSIssueRecord{
		ID: issues.IssueID("s3-public", owner), Rule: "s3-public", Kind: "security", Severity: "high", Tier: "act_now", State: "open",
		Confirm: "confirmed", AccountID: account, Region: "eu-west-1", Owner: owner, Title: "bucket is public",
		FirstSeen: at, OpenedAt: at, Version: 1, UpdatedAt: at,
	}
	if err := issueStore.WriteAWSIssues(context.Background(), scope, storage.AWSIssueBatch{AccountID: account, Writes: []storage.AWSIssueWrite{{
		Issue: open, Events: []storage.AWSIssueEvent{{At: at, Kind: storage.AWSIssueEventOpened, Actor: "evaluator", Detail: "existed when first read"}},
	}}}); err != nil {
		t.Fatal(err)
	}
	if mounted.Issues.Due() {
		t.Fatal("due before the account was removed")
	}
	if _, err := mounted.Settings.DeleteAccount(context.Background(), scope.Write, account); err != nil {
		t.Fatal(err)
	}
	if !mounted.Issues.Due() {
		t.Fatal("removing the account did not mark the evaluator due")
	}
	report, err := mounted.Issues.Evaluate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Accounts) != 1 || !report.Accounts[0].Disconnected {
		t.Fatalf("evaluation = %+v; want the removed account resolved", report)
	}
	stored, err := issueStore.AWSIssues(context.Background(), scope, storage.AWSIssueQuery{AccountID: account})
	if err != nil || len(stored) != 1 || stored[0].State != "resolved" {
		t.Fatalf("issues after the removal = %+v, %v; want the one issue resolved", stored, err)
	}
}
