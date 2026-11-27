package mount

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/VersusControl/versus-incident/pkg/aws/estate"
	"github.com/VersusControl/versus-incident/pkg/aws/model"
	awssettings "github.com/VersusControl/versus-incident/pkg/aws/settings"
	awssnapshot "github.com/VersusControl/versus-incident/pkg/aws/snapshot"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

func rawJSON(t *testing.T, app *fiber.App, path string) map[string]json.RawMessage {
	t.Helper()
	res, err := app.Test(httptest.NewRequest("GET", path, nil), -1)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)
	var out map[string]json.RawMessage
	if res.StatusCode != 200 || json.Unmarshal(data, &out) != nil {
		t.Fatalf("%s = %d %s", path, res.StatusCode, data)
	}
	return out
}

func field[T any](t *testing.T, raw json.RawMessage) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	return out
}

// While an account's first read runs (some units committed, the rest
// pending), it is partial, says the first read is in progress, its counts
// are lower bounds and the warning names it, on Overview, Topology and
// Coverage alike.
func TestFirstReadInProgressIsPartial(t *testing.T) {
	app, mounted, _ := estateApp(t, storage.NewMemory(), tenancy.OrgScope{}, estateRead)
	if err := mounted.Snapshots.RecordPending([]model.Unit{
		{Account: estateRead, Region: estateRegion, Service: "lambda"}, {Account: estateRead, Region: estateRegion, Service: "sqs"},
	}); err != nil {
		t.Fatal(err)
	}
	commitFunctions(t, mounted, estateRead, "lambda", map[string]string{"cart": "shop"}, nil)

	var overview overviewAnswer
	getJSON(t, app, "/api/admin/aws/overview", &overview)
	row := overview.Accounts[0]
	if row.Read != estate.ReadPartial || row.NotReadReason != awssnapshot.AccountFirstReadInProgress || row.Resources == nil ||
		*row.Resources != 1 || !row.ResourcesCapped || !overview.Summary.ResourcesCapped || overview.Warning == nil {
		t.Fatalf("overview row %+v, summary %+v, warning %+v; want partial, first read in progress, capped, warned", row, overview.Summary, overview.Warning)
	}
	var topology topologyAnswer
	getJSON(t, app, "/api/admin/aws/topology?level=accounts", &topology)
	for _, node := range topology.Nodes {
		if node.Account == estateRead && (node.Read != estate.ReadPartial || node.NotReadReason != awssnapshot.AccountFirstReadInProgress || !node.ResourcesCapped) {
			t.Fatalf("topology card = %+v; want partial, first read in progress, capped", node)
		}
	}
	coverage := rawJSON(t, app, "/api/admin/aws/coverage")
	accounts := field[[]awssnapshot.CoverageAccount](t, coverage["accounts"])
	if len(accounts) != 1 || accounts[0].Read != awssnapshot.AccountPartial || accounts[0].Reason != awssnapshot.AccountFirstReadInProgress {
		t.Fatalf("coverage accounts = %+v", accounts)
	}

	// Once every unit has read once, the account is read in full.
	if _, err := mounted.Snapshots.Commit(model.Unit{Account: estateRead, Region: estateRegion, Service: "sqs"}, awssnapshot.Candidate{}); err != nil {
		t.Fatal(err)
	}
	overview = overviewAnswer{}
	getJSON(t, app, "/api/admin/aws/overview", &overview)
	if row := overview.Accounts[0]; row.Read != estate.ReadFull || row.ResourcesCapped || overview.Warning != nil {
		t.Fatalf("after the first read: %+v, warning %+v", row, overview.Warning)
	}
}

// With one account in scope not read, Act now, Worth a look and the grouped
// share are lower bounds; with every account read in full they are exact.
func TestSummaryCountsCappedWhileAnAccountIsNotRead(t *testing.T) {
	app, mounted, _ := estateApp(t, storage.NewMemory(), tenancy.OrgScope{}, estateRead, estateFailed)
	commitFunctions(t, mounted, estateRead, "lambda", map[string]string{"cart": "shop", "pay": ""}, nil)
	read := func() map[string]json.RawMessage {
		return field[map[string]json.RawMessage](t, rawJSON(t, app, "/api/admin/aws/overview")["summary"])
	}
	summary := read()
	for _, flag := range []string{"act_now_capped", "worth_a_look_capped", "grouped_share_capped", "resources_capped"} {
		if string(summary[flag]) != "true" {
			t.Fatalf("one account not read: %s = %s; want true (%v)", flag, summary[flag], summary)
		}
	}
	if _, err := mounted.Snapshots.Commit(model.Unit{Account: estateFailed, Region: estateRegion, Service: "lambda"}, awssnapshot.Candidate{}); err != nil {
		t.Fatal(err)
	}
	summary = read()
	for _, flag := range []string{"act_now_capped", "worth_a_look_capped", "grouped_share_capped", "resources_capped"} {
		if string(summary[flag]) != "false" {
			t.Fatalf("every account read: %s = %s; want false", flag, summary[flag])
		}
	}
}

// With one account never read (its role could not be assumed from the
// start), Changes, Overview, Topology and Coverage agree on accounts read
// of total, the Regions read and each account's state; Changes names the
// account's failure, never "disconnected".
func TestAccountsRegionsAndStatesAgreeAcrossPages(t *testing.T) {
	store := storage.NewMemory()
	app, mounted, config := estateApp(t, store, tenancy.OrgScope{}, estateRead, estateFailed)
	config.Connectors.AWS.Accounts[0].Regions = []string{estateRegion, "eu-west-1", "us-east-1"}
	config.Connectors.AWS.Accounts[1].Regions = []string{estateRegion}
	scope := mounted.Snapshots.Scope()
	if err := awssettings.NewStore(store, scope).SaveHealth(scope.Write, awssettings.HealthRecord{AccountID: estateFailed,
		Status: awssettings.StatusDisconnected, ErrorCode: "trust_denied", FailureSince: time.Now().UTC().Add(-time.Hour),
		DisconnectedSince: time.Now().UTC().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	commitFunctions(t, mounted, estateRead, "lambda", map[string]string{"cart": "shop"}, nil)

	type numbers struct {
		read, total int
		regions     []string
		states      map[string]string
	}
	var got = map[string]numbers{}

	overview := rawJSON(t, app, "/api/admin/aws/overview")
	summary := field[struct {
		AccountsRead  int `json:"accounts_read"`
		AccountsTotal int `json:"accounts_total"`
	}](t, overview["summary"])
	rows := field[[]struct {
		ID   string `json:"id"`
		Read string `json:"read"`
	}](t, overview["accounts"])
	states := map[string]string{}
	for _, row := range rows {
		states[row.ID] = row.Read
	}
	got["overview"] = numbers{summary.AccountsRead, summary.AccountsTotal, field[[]string](t, overview["regions"]), states}

	topology := rawJSON(t, app, "/api/admin/aws/topology?level=accounts")
	nodes := field[[]struct {
		Account string `json:"account"`
		Read    string `json:"read"`
	}](t, topology["nodes"])
	states, read := map[string]string{}, 0
	for _, node := range nodes {
		if node.Read != "" {
			states[node.Account] = node.Read
			if node.Read != estate.ReadNone {
				read++
			}
		}
	}
	got["topology"] = numbers{read, len(states), field[[]string](t, topology["regions"]), states}

	coverage := rawJSON(t, app, "/api/admin/aws/coverage")
	coverageSummary := field[struct {
		AccountsRead  int `json:"accounts_read"`
		AccountsTotal int `json:"accounts_total"`
	}](t, coverage["summary"])
	states = map[string]string{}
	for _, account := range field[[]awssnapshot.CoverageAccount](t, coverage["accounts"]) {
		states[account.Account] = account.Read
	}
	got["coverage"] = numbers{coverageSummary.AccountsRead, coverageSummary.AccountsTotal, field[[]string](t, coverage["regions"]), states}

	changes := rawJSON(t, app, "/api/admin/aws/changes")
	block := field[*awssnapshot.FeedCoverage](t, changes["coverage"])
	if block == nil {
		t.Fatal("the Changes answer has no coverage block")
	}
	states = map[string]string{}
	for _, account := range block.Accounts {
		states[account.Account] = account.Read
	}
	got["changes"] = numbers{block.AccountsRead, block.AccountsTotal, block.Regions, states}

	want := asJSON(map[string]any{"read": 1, "total": 2, "regions": []string{"eu-south-2", "eu-west-1", "us-east-1"},
		"states": map[string]string{estateRead: estate.ReadFull, estateFailed: estate.ReadNone}})
	for page, numbers := range got {
		if have := asJSON(map[string]any{"read": numbers.read, "total": numbers.total, "regions": numbers.regions, "states": numbers.states}); have != want {
			t.Errorf("%s = %s; want %s", page, have, want)
		}
	}

	feedAccounts := field[[]awssnapshot.FeedAccount](t, changes["accounts"])
	for _, account := range feedAccounts {
		if account.Account == estateFailed && (account.Reason == nil || *account.Reason == string(awssnapshot.GapAccountDisconnected) || account.DisconnectedAt != nil) {
			t.Fatalf("never-read account on Changes = %+v (reason %v); want its failure, not disconnected", account, account.Reason)
		}
	}
}
