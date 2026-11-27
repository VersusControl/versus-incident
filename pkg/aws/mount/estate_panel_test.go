package mount

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/VersusControl/versus-incident/pkg/aws/estate"
	"github.com/VersusControl/versus-incident/pkg/aws/issues"
	"github.com/VersusControl/versus-incident/pkg/aws/model"
	awssnapshot "github.com/VersusControl/versus-incident/pkg/aws/snapshot"
	c "github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/middleware"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

// postJSON sends a body as a caller who may also write settings.
func postJSON(t *testing.T, app *fiber.App, path, body string) (int, string) {
	t.Helper()
	request := httptest.NewRequest("POST", path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	res, err := app.Test(request, -1)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(data)
}

// A correction saved with the created_at its writer read replaces it; one
// saved with a stale created_at, or for a correction that is gone, is a
// conflict that writes nothing; one saved without it keeps the old
// unconditional behaviour.
func TestGroupCorrectionCompareAndSwap(t *testing.T) {
	for name, provider := range estateProviders(t) {
		t.Run(name, func(t *testing.T) {
			app := fiber.New(fiber.Config{Immutable: true})
			app.Use(func(ctx *fiber.Ctx) error {
				middleware.MarkAuthorized(ctx)
				middleware.SetRequestPermission(ctx, string(core.PermissionInfrastructureView), true)
				middleware.SetRequestPermission(ctx, string(core.PermissionInfrastructureSettingsWrite), true)
				return ctx.Next()
			})
			config := &c.Config{}
			config.Connectors.AWS.Enable = true
			mountForTest(t, app, Deps{Store: provider(t), Config: config, Scope: tenancy.NewOrgScope(fmt.Sprintf("cas%d", time.Now().UnixNano()))})
			path := "/api/admin/aws/groups/corrections"
			rename := func(name, expected string) (int, string) {
				body := `{"account_id":"` + estateRead + `","kind":"rename","group_id":"stack:shop","name":"` + name + `"`
				if expected != "" {
					body += `,"expected_created_at":"` + expected + `"`
				}
				return postJSON(t, app, path, body+"}")
			}
			status, text := rename("Shop", "")
			var saved struct {
				Correction struct {
					CreatedAt time.Time `json:"created_at"`
				} `json:"correction"`
			}
			if status != 200 || json.Unmarshal([]byte(text), &saved) != nil {
				t.Fatalf("first save = %d %s", status, text)
			}
			first := saved.Correction.CreatedAt.Format(time.RFC3339Nano)
			status, text = rename("Shop two", first)
			if status != 200 || json.Unmarshal([]byte(text), &saved) != nil {
				t.Fatalf("save with the current created_at = %d %s", status, text)
			}
			if status, text = rename("Shop three", first); status != fiber.StatusConflict || !strings.Contains(text, `"correction_conflict"`) {
				t.Fatalf("save with a stale created_at = %d %s; want 409 correction_conflict", status, text)
			}
			body := `{"account_id":"` + estateRead + `","kind":"rename","group_id":"stack:gone","name":"Gone","expected_created_at":"` + first + `"}`
			if status, text = postJSON(t, app, path, body); status != fiber.StatusConflict {
				t.Fatalf("save expecting a correction that is gone = %d %s", status, text)
			}
			if status, text = rename("Shop four", ""); status != 200 {
				t.Fatalf("save without created_at = %d %s; want the unconditional save", status, text)
			}
		})
	}
}

type panelNode struct {
	ID               string             `json:"id"`
	Kind             string             `json:"kind"`
	Account          string             `json:"account"`
	OpenIssues       *int               `json:"open_issues"`
	ActNow           *int               `json:"act_now"`
	WorthALook       *int               `json:"worth_a_look"`
	Resources        *int               `json:"resources"`
	Health           *estate.NodeHealth `json:"health"`
	TopIssue         *estate.TopIssue   `json:"top_issue"`
	RoleDetail       string             `json:"role_detail"`
	ActNowCapped     bool               `json:"act_now_capped"`
	WorthALookCapped bool               `json:"worth_a_look_capped"`
}

func panelNodes(t *testing.T, app *fiber.App, path string) map[string]panelNode {
	t.Helper()
	var answer struct {
		Nodes []panelNode `json:"nodes"`
	}
	if status := getJSON(t, app, path, &answer); status != 200 {
		t.Fatalf("%s = %d", path, status)
	}
	out := map[string]panelNode{}
	for _, node := range answer.Nodes {
		out[node.ID] = node
	}
	return out
}

// Account and group cards carry Act now and Worth a look beside the open
// issues; a Focus view puts on each resource its latest health (or none,
// or not read) and its leading open issue, Act now first, with the likely
// cause; Overview names where each issue sits.
func TestTopologyPanelFacts(t *testing.T) {
	store := storage.NewMemory()
	app, mounted, _ := estateApp(t, store, tenancy.OrgScope{}, estateRead, estateFailed)
	scope := mounted.Snapshots.Scope()
	localRole, remoteRole := "arn:aws:iam::"+estateRead+":role/local", "arn:aws:iam::"+estateFailed+":role/remote"
	commitFunctions(t, mounted, estateRead, "lambda", map[string]string{"cart": "shop", "pay": "shop"},
		map[string][]model.Relation{"cart": {{Type: model.RelationRoutesTo, Target: lambdaARN(estateRead, "pay")},
			{Type: model.RelationRoutesTo, Target: lambdaARN(estateFailed, "remote")},
			{Type: model.RelationRoutesTo, Target: localRole}, {Type: model.RelationRoutesTo, Target: remoteRole}}})
	commitFunctions(t, mounted, estateFailed, "lambda", map[string]string{"remote": ""}, nil)
	for account, role := range map[string]string{estateRead: localRole, estateFailed: remoteRole} {
		if _, err := mounted.Snapshots.Commit(model.Unit{Account: account, Region: model.GlobalRegion, Service: "iam"}, awssnapshot.Candidate{
			Resources: []model.Resource{{ARN: role, Service: "iam", Type: "role", Account: account, Region: model.GlobalRegion, Name: "role"}}}); err != nil {
			t.Fatal(err)
		}
	}
	// The second account's health is read in us-east-1 only: its
	// eu-south-2 function has no health read, its global role has.
	if _, err := mounted.Snapshots.Commit(model.Unit{Account: estateFailed, Region: "us-east-1", Service: "cloudwatch"}, awssnapshot.Candidate{}); err != nil {
		t.Fatal(err)
	}
	since := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	if _, err := mounted.Snapshots.Commit(model.Unit{Account: estateRead, Region: estateRegion, Service: "cloudwatch"}, awssnapshot.Candidate{
		Health: []model.Health{{Resource: lambdaARN(estateRead, "cart"), Account: estateRead, Region: estateRegion, Source: "cloudwatch",
			Status: model.HealthCritical, Summary: "errors above 5%", Since: since}}}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	writeIssue(t, store, scope, estateRead, lambdaARN(estateRead, "cart"), issues.SeverityCritical, issues.TierWorthALook, issues.StateOpen, now)
	writeIssue(t, store, scope, estateRead, lambdaARN(estateRead, "cart"), issues.SeverityMedium, issues.TierActNow, issues.StateOpen, now.Add(-time.Hour))
	writeIssue(t, store, scope, estateRead, lambdaARN(estateRead, "pay"), issues.SeverityLow, issues.TierWorthALook, issues.StateOpen, now)
	writeIssue(t, store, scope, estateRead, "account-wide", issues.SeverityHigh, issues.TierActNow, issues.StateOpen, now)

	groups := panelNodes(t, app, "/api/admin/aws/topology?level=groups&account="+estateRead)
	shop := groups["group:"+estateRead+"/stack:shop"]
	if shop.ActNow == nil || *shop.ActNow != 1 || shop.WorthALook == nil || *shop.WorthALook != 2 || shop.OpenIssues == nil || *shop.OpenIssues != 3 {
		t.Fatalf("group tiers = act %v look %v open %v; want 1, 2, 3", deref(shop.ActNow), deref(shop.WorthALook), deref(shop.OpenIssues))
	}
	accounts := panelNodes(t, app, "/api/admin/aws/topology?level=accounts")
	card := accounts["account:"+estateRead]
	if card.ActNow == nil || *card.ActNow != 2 || card.WorthALook == nil || *card.WorthALook != 2 {
		t.Fatalf("account tiers = act %v look %v; want 2 and 2", deref(card.ActNow), deref(card.WorthALook))
	}

	focus := panelNodes(t, app, "/api/admin/aws/topology?level=focus&ref="+lambdaARN(estateRead, "cart"))
	cart, pay, remote := focus[lambdaARN(estateRead, "cart")], focus[lambdaARN(estateRead, "pay")], focus[lambdaARN(estateFailed, "remote")]
	if cart.Health == nil || cart.Health.State != string(model.HealthCritical) || cart.Health.Summary != "errors above 5%" {
		t.Fatalf("cart health = %+v; want the critical signal", cart.Health)
	}
	if pay.Health == nil || pay.Health.State != estate.HealthNone {
		t.Fatalf("pay health = %+v; want none: health read, no signal", pay.Health)
	}
	if remote.Health == nil || remote.Health.State != estate.HealthNotRead {
		t.Fatalf("remote health = %+v; want not read: its account's health was read in us-east-1, not in its Region", remote.Health)
	}
	// A global resource is answered by its account's us-east-1 health read.
	if role := focus[remoteRole]; role.Health == nil || role.Health.State != estate.HealthNone {
		t.Fatalf("global role with a us-east-1 health read = %+v; want none", role.Health)
	}
	if role := focus[localRole]; role.Health == nil || role.Health.State != estate.HealthNotRead {
		t.Fatalf("global role with no us-east-1 health read = %+v; want not read", role.Health)
	}
	if cart.TopIssue == nil || cart.TopIssue.Tier != issues.TierActNow || cart.TopIssue.Severity != issues.SeverityMedium {
		t.Fatalf("cart top issue = %+v; want the Act now one before the critical Worth a look", cart.TopIssue)
	}
	if pay.TopIssue == nil || pay.TopIssue.Tier != issues.TierWorthALook || remote.TopIssue != nil {
		t.Fatalf("top issues: pay %+v, remote %+v", pay.TopIssue, remote.TopIssue)
	}

	var overview struct {
		NeedsAttention []struct {
			Owner string `json:"owner"`
			Where string `json:"where"`
		} `json:"needs_attention"`
	}
	getJSON(t, app, "/api/admin/aws/overview", &overview)
	places := map[string]string{}
	for _, row := range overview.NeedsAttention {
		places[row.Owner] = row.Where
	}
	if places[lambdaARN(estateRead, "cart")] != "function cart" || places["account-wide"] != "the account" {
		t.Fatalf("where = %v; want \"function cart\" and \"the account\"", places)
	}
}

// The scope bar narrows Overview and Topology, over the memory store and
// Postgres (whose SQL Region and Tier filters it exercises): one account named gives
// exactly that account's unfiltered row and counts; a Region keeps its
// resources and the global ones; bad values are a 400 and an account not
// connected a 404; repeated and comma-separated accounts read alike.
func TestEstateScopeFilters(t *testing.T) {
	for name, provider := range estateProviders(t) {
		t.Run(name, func(t *testing.T) {
			estateScopeFilters(t, provider(t))
		})
	}
}

func estateScopeFilters(t *testing.T, store storage.Provider) {
	app, mounted, _ := estateApp(t, store, tenancy.OrgScope{}, estateRead, estateFailed)
	scope := mounted.Snapshots.Scope()
	commitFunctions(t, mounted, estateRead, "lambda", map[string]string{"cart": "shop", "pay": ""},
		map[string][]model.Relation{"cart": {{Type: model.RelationRoutesTo, Target: lambdaARN(estateFailed, "remote")}}})
	commitFunctions(t, mounted, estateFailed, "lambda", map[string]string{"remote": "", "edge": ""}, nil)
	elsewhere := model.Resource{ARN: "arn:aws:lambda:us-east-1:" + estateRead + ":function:far", Service: "lambda", Type: "function",
		Account: estateRead, Region: "us-east-1", Name: "far"}
	if _, err := mounted.Snapshots.Commit(model.Unit{Account: estateRead, Region: "us-east-1", Service: "lambda"},
		awssnapshot.Candidate{Resources: []model.Resource{elsewhere}}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	writeIssue(t, store, scope, estateRead, lambdaARN(estateRead, "cart"), issues.SeverityHigh, issues.TierActNow, issues.StateOpen, now)
	writeIssue(t, store, scope, estateFailed, lambdaARN(estateFailed, "edge"), issues.SeverityLow, issues.TierWorthALook, issues.StateOpen, now)
	writeIssueIn(t, store, scope, estateRead, "us-east-1", elsewhere.ARN, issues.SeverityCritical, issues.TierActNow, issues.StateOpen, now)
	writeIssueIn(t, store, scope, estateRead, model.GlobalRegion, "account-wide", issues.SeverityLow, issues.TierWorthALook, issues.StateOpen, now)

	var all, one overviewAnswer
	getJSON(t, app, "/api/admin/aws/overview", &all)
	if status := getJSON(t, app, "/api/admin/aws/overview?account="+estateRead, &one); status != 200 {
		t.Fatalf("scoped overview = %d", status)
	}
	var row any
	for _, candidate := range all.Accounts {
		if candidate.ID == estateRead {
			row = candidate
		}
	}
	if len(one.Accounts) != 1 || asJSON(one.Accounts[0]) != asJSON(row) {
		t.Fatalf("scoped rows %+v; want exactly the unfiltered row %+v", one.Accounts, row)
	}
	if one.Summary.AccountsTotal != 1 || deref(one.Summary.Resources) != deref(one.Accounts[0].Resources) || deref(one.Summary.ActNow) != 2 ||
		deref(one.Summary.WorthALook) != 1 || deref(all.Summary.WorthALook) != 2 {
		t.Fatalf("scoped summary %+v, unfiltered %+v", one.Summary, all.Summary)
	}
	var both, comma overviewAnswer
	getJSON(t, app, "/api/admin/aws/overview?account="+estateRead+"&account="+estateFailed, &both)
	getJSON(t, app, "/api/admin/aws/overview?account="+estateRead+","+estateFailed, &comma)
	if asJSON(both.Summary) != asJSON(all.Summary) || asJSON(comma.Summary) != asJSON(all.Summary) {
		t.Fatalf("both accounts named: %+v / %+v; want the unfiltered %+v", both.Summary, comma.Summary, all.Summary)
	}
	var regional overviewAnswer
	getJSON(t, app, "/api/admin/aws/overview?account="+estateRead+"&region="+estateRegion, &regional)
	// The Region keeps its issues and the account-wide ones.
	if deref(regional.Summary.ActNow) != 1 || deref(regional.Summary.WorthALook) != 1 {
		t.Fatalf("region %s issues = act %v look %v; want 1 and 1", estateRegion, deref(regional.Summary.ActNow), deref(regional.Summary.WorthALook))
	}
	if deref(regional.Summary.Resources) != 2 || deref(one.Summary.Resources) != 3 {
		t.Fatalf("region %s resources = %v (unfiltered %v); want 2 of 3", estateRegion, deref(regional.Summary.Resources), deref(one.Summary.Resources))
	}

	accounts := panelNodes(t, app, "/api/admin/aws/topology?level=accounts&account="+estateRead)
	if _, ok := accounts["account:"+estateRead]; !ok || len(accounts) != 2 {
		t.Fatalf("scoped accounts view = %v; want the account and the one it links to", accounts)
	}
	for path, want := range map[string]int{
		"/api/admin/aws/overview?account=12":            400,
		"/api/admin/aws/overview?region=mars":           400,
		"/api/admin/aws/topology?region=mars":           400,
		"/api/admin/aws/overview?account=999999999999":  404,
		"/api/admin/aws/topology?account=999999999999":  404,
		"/api/admin/aws/topology/search?q=a":            400,
		"/api/admin/aws/topology/search?q=ab&region=xx": 400,
	} {
		if status := getJSON(t, app, path, nil); status != want {
			t.Errorf("%s = %d; want %d", path, status, want)
		}
	}
}

// Search finds resources by name, ID or ARN, ignoring case, names that
// start with the text first; at most 20, with capped set past that; within
// the scope.
func TestTopologySearch(t *testing.T) {
	app, mounted, _ := estateApp(t, storage.NewMemory(), tenancy.OrgScope{}, estateRead, estateFailed)
	functions := map[string]string{"Checkout-Web": "", "a-checkout": ""}
	for index := range 25 {
		functions[fmt.Sprintf("batch-%02d", index)] = ""
	}
	commitFunctions(t, mounted, estateRead, "lambda", functions, nil)
	commitFunctions(t, mounted, estateFailed, "lambda", map[string]string{"checkout-api": ""}, nil)
	type result struct {
		Items  []estate.SearchItem `json:"items"`
		Capped bool                `json:"capped"`
	}
	var found result
	if status := getJSON(t, app, "/api/admin/aws/topology/search?q=CHECKOUT", &found); status != 200 {
		t.Fatalf("search = %d", status)
	}
	if len(found.Items) != 3 || found.Capped || !strings.HasPrefix(strings.ToLower(found.Items[0].Name), "checkout") ||
		found.Items[2].Name != "a-checkout" || found.Items[0].Ref == "" {
		t.Fatalf("search CHECKOUT = %+v; want three, names starting with it first", found.Items)
	}
	getJSON(t, app, "/api/admin/aws/topology/search?q=checkout&account="+estateRead, &found)
	if len(found.Items) != 2 {
		t.Fatalf("scoped search = %+v; want the account's two", found.Items)
	}
	getJSON(t, app, "/api/admin/aws/topology/search?q=function:batch", &found)
	if len(found.Items) != estate.MaxSearchResults || !found.Capped {
		t.Fatalf("search by ARN = %d items, capped %v; want 20, capped", len(found.Items), found.Capped)
	}
}

// The coverage answer says the settings AWS answered as not set are not
// counted yet: null, never zero.
func TestCoverageNotSetIsNotCounted(t *testing.T) {
	app, _, _ := estateApp(t, storage.NewMemory(), tenancy.OrgScope{}, estateRead)
	res, err := app.Test(httptest.NewRequest("GET", "/api/admin/aws/coverage", nil), -1)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var coverage struct {
		NotGaps map[string]json.RawMessage `json:"not_gaps"`
	}
	data, _ := io.ReadAll(res.Body)
	if err := json.Unmarshal(data, &coverage); err != nil {
		t.Fatal(err)
	}
	if value, ok := coverage.NotGaps["not_set"]; !ok || string(value) != "null" {
		t.Fatalf("not_gaps = %s; want not_set null", data)
	}
}

func asJSON(value any) string {
	data, _ := json.Marshal(value)
	return string(data)
}

// A Focus view names, on a resource's leading issue, the node its likely
// cause changed (matched by resource key, so a function version finds the
// function), and on each resource its latest change of the last day from
// the page of changes already read; a resource with none has none.
func TestFocusCauseRefAndLastChange(t *testing.T) {
	store := storage.NewMemory()
	app, mounted, _ := estateApp(t, store, tenancy.OrgScope{}, estateRead)
	scope := mounted.Snapshots.Scope()
	commitFunctions(t, mounted, estateRead, "lambda", map[string]string{"cart": "shop", "pay": "shop", "idle": "shop"},
		map[string][]model.Relation{"cart": {{Type: model.RelationRoutesTo, Target: lambdaARN(estateRead, "pay")},
			{Type: model.RelationRoutesTo, Target: lambdaARN(estateRead, "idle")}}})
	now := time.Now().UTC()
	issueStore, _ := storage.AWSIssuesOf(store)
	issue := issues.Issue{ID: issues.IssueID("rule.cause", lambdaARN(estateRead, "cart")), Rule: "rule.cause", Kind: issues.KindReliability,
		Severity: issues.SeverityHigh, Tier: issues.TierActNow, Account: estateRead, Region: estateRegion, Owner: lambdaARN(estateRead, "cart"),
		Title: "cart errors", State: issues.StateOpen, Confirm: "confirmed", FirstSeen: now, OpenedAt: now, Version: 1, UpdatedAt: now,
		Cause: &issues.Cause{Status: "found", Text: "pay changed at 09:00", Change: "c9", Resource: lambdaARN(estateRead, "pay") + ":7"}}
	record, err := issues.Record(issue)
	if err != nil {
		t.Fatal(err)
	}
	if err := issueStore.WriteAWSIssues(t.Context(), scope, storage.AWSIssueBatch{AccountID: estateRead,
		Writes: []storage.AWSIssueWrite{{Issue: record}}}); err != nil {
		t.Fatal(err)
	}
	appendChanges(t, store, scope, estateRead, []storage.AWSChange{
		{EventID: "old", Account: estateRead, Region: estateRegion, Service: "lambda", EventTime: now.Add(-3 * time.Hour), EventName: "UpdateFunctionConfiguration",
			ActorClass: "person", Actor: "alice", ResourceARN: lambdaARN(estateRead, "pay")},
		{EventID: "new", Account: estateRead, Region: estateRegion, Service: "lambda", EventTime: now.Add(-time.Hour), EventName: "UpdateFunctionCode",
			ActorClass: "pipeline", Actor: "deploy", ResourceARN: lambdaARN(estateRead, "pay")},
	})
	var answer struct {
		Nodes []struct {
			ID              string             `json:"id"`
			TopIssue        *estate.TopIssue   `json:"top_issue"`
			LastChange      *estate.LastChange `json:"last_change"`
			LastChangeKnown *bool              `json:"last_change_known"`
			Changes24h      *int               `json:"changes_24h"`
		} `json:"nodes"`
	}
	if status := getJSON(t, app, "/api/admin/aws/topology?level=focus&ref="+lambdaARN(estateRead, "cart"), &answer); status != 200 {
		t.Fatalf("focus = %d", status)
	}
	byID := map[string]int{}
	for index, node := range answer.Nodes {
		byID[node.ID] = index
	}
	cart, pay, idle := answer.Nodes[byID[lambdaARN(estateRead, "cart")]], answer.Nodes[byID[lambdaARN(estateRead, "pay")]], answer.Nodes[byID[lambdaARN(estateRead, "idle")]]
	if cart.TopIssue == nil || cart.TopIssue.CauseRef != lambdaARN(estateRead, "pay") {
		t.Fatalf("cart top issue = %+v; want the cause pointing at the pay node", cart.TopIssue)
	}
	if pay.LastChange == nil || pay.LastChange.Actor == "" || !strings.HasSuffix(pay.LastChange.EventName, "UpdateFunctionCode") ||
		pay.LastChangeKnown != nil || pay.Changes24h != nil {
		t.Fatalf("pay last change = %+v known %v, changes %v; want the newest, no known flag, no lens count",
			pay.LastChange, pay.LastChangeKnown, pay.Changes24h)
	}
	// No change poll settled this window: a resource with no change read
	// may still have had one.
	if idle.LastChange != nil || idle.LastChangeKnown == nil || *idle.LastChangeKnown {
		t.Fatalf("idle last change = %+v known %v; want none, not known (window not settled)", idle.LastChange, idle.LastChangeKnown)
	}
}

// settleChanges records the account's change polls as covering the last
// two days in its Region and the global pair, so the window is settled.
func settleChanges(t *testing.T, store storage.Provider, scope tenancy.OrgScope, account string) {
	t.Helper()
	inventory := store.(storage.AWSInventoryStore)
	now := time.Now().UTC()
	for _, region := range []string{estateRegion, model.GlobalRegion} {
		if err := inventory.AppendAWSChanges(t.Context(), scope, account, region, nil,
			[]storage.AWSInterval{{From: now.Add(-48 * time.Hour), To: now.Add(time.Hour)}}, nil); err != nil {
			t.Fatal(err)
		}
	}
}

type lastChangeNode struct {
	ID              string             `json:"id"`
	LastChange      *estate.LastChange `json:"last_change"`
	LastChangeKnown *bool              `json:"last_change_known"`
}

func lastChanges(t *testing.T, app *fiber.App, ref string) map[string]lastChangeNode {
	t.Helper()
	var answer struct {
		Nodes []lastChangeNode `json:"nodes"`
	}
	if status := getJSON(t, app, "/api/admin/aws/topology?level=focus&ref="+ref, &answer); status != 200 {
		t.Fatalf("focus = %d", status)
	}
	out := map[string]lastChangeNode{}
	for _, node := range answer.Nodes {
		out[node.ID] = node
	}
	return out
}

// The feed page is newest first, so a cut page loses older changes: a
// resource whose only change fell off the page has no last change and is
// not known, while the resource with the newest changes has its exact
// last change. On a complete, settled page a resource with no change is
// known to have none.
func TestFocusLastChangeOnCutAndCompletePages(t *testing.T) {
	store := storage.NewMemory()
	app, mounted, _ := estateApp(t, store, tenancy.OrgScope{}, estateRead)
	scope := mounted.Snapshots.Scope()
	commitFunctions(t, mounted, estateRead, "lambda", map[string]string{"cart": "shop", "pay": "shop", "idle": "shop"},
		map[string][]model.Relation{"cart": {{Type: model.RelationRoutesTo, Target: lambdaARN(estateRead, "pay")},
			{Type: model.RelationRoutesTo, Target: lambdaARN(estateRead, "idle")}}})
	settleChanges(t, store, scope, estateRead)
	now := time.Now().UTC()

	appendChanges(t, store, scope, estateRead, []storage.AWSChange{{EventID: "pay-only", Account: estateRead, Region: estateRegion,
		Service: "lambda", EventTime: now.Add(-2 * time.Hour), EventName: "UpdateFunctionCode", ActorClass: "person", Actor: "alice",
		ResourceARN: lambdaARN(estateRead, "pay")}})
	complete := lastChanges(t, app, lambdaARN(estateRead, "cart"))
	if idle := complete[lambdaARN(estateRead, "idle")]; idle.LastChange != nil || idle.LastChangeKnown == nil || !*idle.LastChangeKnown {
		t.Fatalf("complete settled page: idle = %+v; want no change, known", idle)
	}

	rows := []storage.AWSChange{{EventID: "cart-old", Account: estateRead, Region: estateRegion, Service: "lambda",
		EventTime: now.Add(-20 * time.Hour), EventName: "UpdateFunctionConfiguration", ActorClass: "person", Actor: "bob",
		ResourceARN: lambdaARN(estateRead, "cart")}}
	for index := range 260 {
		rows = append(rows, storage.AWSChange{EventID: fmt.Sprintf("pay-%04d", index), Account: estateRead, Region: estateRegion,
			Service: "lambda", EventTime: now.Add(-time.Duration(index+1) * time.Minute), EventName: "UpdateFunctionCode", ActorClass: "person",
			Actor: fmt.Sprintf("person-%04d", index), ResourceARN: lambdaARN(estateRead, "pay")})
	}
	appendChanges(t, store, scope, estateRead, rows)
	settleChanges(t, store, scope, estateRead)
	cut := lastChanges(t, app, lambdaARN(estateRead, "cart"))
	cart, pay := cut[lambdaARN(estateRead, "cart")], cut[lambdaARN(estateRead, "pay")]
	if cart.LastChange != nil || cart.LastChangeKnown == nil || *cart.LastChangeKnown {
		t.Fatalf("cut page: cart = %+v; want no last change, not known", cart)
	}
	if pay.LastChange == nil || pay.LastChange.Actor == "" || pay.LastChangeKnown != nil ||
		now.Add(-time.Minute).Sub(pay.LastChange.Time).Abs() > time.Second {
		t.Fatalf("cut page: pay = %+v; want its exact newest change, a minute ago", pay)
	}
}

// A summed link between groups breaks its count down by the AWS read that
// found the links under it, so a group-level grant can read "KMS key 2".
func TestGroupLinksCarrySources(t *testing.T) {
	app, mounted, _ := estateApp(t, storage.NewMemory(), tenancy.OrgScope{}, estateRead)
	grant := func(source string) model.Relation {
		return model.Relation{Type: model.RelationMayAccess, Target: "arn:aws:iam::" + estateOutsider + ":role/ci", Kind: model.RelationKindMayAccess,
			Source: source}
	}
	var resources []model.Resource
	for name, source := range map[string]string{"key-a": "kms:GetKeyPolicy", "key-b": "kms:GetKeyPolicy", "repo": "ecr:GetRepositoryPolicy"} {
		resources = append(resources, model.Resource{ARN: "arn:aws:kms:" + estateRegion + ":" + estateRead + ":key/" + name, Service: "kms", Type: "key",
			Account: estateRead, Region: estateRegion, Name: name, Attributes: map[string]string{model.AttrStackName: "vault"},
			Relations: []model.Relation{grant(source)}})
	}
	if _, err := mounted.Snapshots.Commit(model.Unit{Account: estateRead, Region: estateRegion, Service: "kms"},
		awssnapshot.Candidate{Resources: resources}); err != nil {
		t.Fatal(err)
	}
	var groups struct {
		Links []struct {
			From    string         `json:"from"`
			To      string         `json:"to"`
			Count   int            `json:"count"`
			Sources map[string]int `json:"sources"`
		} `json:"links"`
	}
	if status := getJSON(t, app, "/api/admin/aws/topology?level=groups&account="+estateRead, &groups); status != 200 {
		t.Fatalf("groups = %d", status)
	}
	found := false
	for _, link := range groups.Links {
		if link.From == "group:"+estateRead+"/stack:vault" && link.To == "account:"+estateOutsider {
			found = link.Count == 3 && link.Sources["kms:GetKeyPolicy"] == 2 && link.Sources["ecr:GetRepositoryPolicy"] == 1 && len(link.Sources) == 2
			if !found {
				t.Fatalf("summed link = %+v; want count 3 from KMS key 2 and ECR 1", link)
			}
		}
	}
	if !found {
		t.Fatalf("no summed link from the group to the outside account: %+v", groups.Links)
	}
	var accounts struct {
		Links []struct {
			Count   int            `json:"count"`
			Source  string         `json:"source"`
			Sources map[string]int `json:"sources"`
		} `json:"links"`
	}
	getJSON(t, app, "/api/admin/aws/topology?level=accounts", &accounts)
	for _, link := range accounts.Links {
		if link.Source != "" && link.Sources[link.Source] != link.Count {
			t.Fatalf("accounts-level link = %+v; want its count under its source", link)
		}
	}
}
