package mount

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/VersusControl/versus-incident/pkg/aws/estate"
	"github.com/VersusControl/versus-incident/pkg/aws/issues"
	"github.com/VersusControl/versus-incident/pkg/aws/model"
	awssettings "github.com/VersusControl/versus-incident/pkg/aws/settings"
	awssnapshot "github.com/VersusControl/versus-incident/pkg/aws/snapshot"
	c "github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/middleware"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

const (
	estateRegion   = "eu-south-2"
	estateRead     = "123400000101"
	estateFailed   = "123400000102"
	estateOutsider = "555500009999"
)

// estateProviders are the stores the estate routes are checked over: the
// memory store always, and Postgres when TEST_POSTGRES_DSN names an
// isolated database.
func estateProviders(t *testing.T) map[string]func(t *testing.T) storage.Provider {
	out := map[string]func(t *testing.T) storage.Provider{"memory": func(*testing.T) storage.Provider { return storage.NewMemory() }}
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Log("TEST_POSTGRES_DSN not set: the Postgres backend is skipped")
		return out
	}
	out["postgres"] = func(t *testing.T) storage.Provider {
		provider, err := storage.NewPostgres(storage.PostgresOptions{DSN: dsn})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = provider.Close() })
		return provider
	}
	return out
}

// estateApp mounts the AWS surface for a caller who may view the
// infrastructure pages, with the connector on for accounts.
func estateApp(t *testing.T, store storage.Provider, scope tenancy.OrgScope, accounts ...string) (*fiber.App, *Mounted, *c.Config) {
	t.Helper()
	app := fiber.New(fiber.Config{Immutable: true})
	app.Use(func(ctx *fiber.Ctx) error {
		middleware.MarkAuthorized(ctx)
		middleware.SetRequestPermission(ctx, string(core.PermissionInfrastructureView), true)
		return ctx.Next()
	})
	config := &c.Config{}
	config.Connectors.AWS.Enable = true
	for _, account := range accounts {
		config.Connectors.AWS.Accounts = append(config.Connectors.AWS.Accounts, c.AWSAccountConfig{AccountID: account, Regions: []string{estateRegion}})
	}
	if scope.Write == "" {
		scope = tenancy.NewOrgScope(fmt.Sprintf("estate%d", time.Now().UnixNano()))
	}
	mounted := mountForTest(t, app, Deps{Store: store, Config: config, Scope: scope})
	return app, mounted, config
}

func getJSON(t *testing.T, app *fiber.App, path string, into any) int {
	t.Helper()
	res, err := app.Test(httptest.NewRequest("GET", path, nil), -1)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)
	if into != nil && res.StatusCode == fiber.StatusOK {
		if err := json.Unmarshal(data, into); err != nil {
			t.Fatalf("%s: %v: %s", path, err, data)
		}
	}
	return res.StatusCode
}

func lambdaARN(account, name string) string {
	return "arn:aws:lambda:" + estateRegion + ":" + account + ":function:" + name
}

// commitFunctions commits functions in account: in a stack when stack is
// set, each with the relations given.
func commitFunctions(t *testing.T, mounted *Mounted, account, service string, functions map[string]string, relations map[string][]model.Relation) {
	t.Helper()
	var resources []model.Resource
	for name, stack := range functions {
		resource := model.Resource{ARN: lambdaARN(account, name), Service: "lambda", Type: "function", Account: account, Region: estateRegion,
			Name: name, Attributes: map[string]string{}, Relations: relations[name]}
		if stack != "" {
			resource.Attributes[model.AttrStackName] = stack
		}
		resources = append(resources, resource)
	}
	if _, err := mounted.Snapshots.Commit(model.Unit{Account: account, Region: estateRegion, Service: service},
		awssnapshot.Candidate{Resources: resources}); err != nil {
		t.Fatal(err)
	}
}

// writeIssue stores one open issue as the evaluator would.
func writeIssue(t *testing.T, store storage.Provider, scope tenancy.OrgScope, account, owner string, severity issues.Severity, tier issues.Tier, state issues.State, opened time.Time) {
	t.Helper()
	writeIssueIn(t, store, scope, account, estateRegion, owner, severity, tier, state, opened)
}

// writeIssueIn stores one open issue in a Region.
func writeIssueIn(t *testing.T, store storage.Provider, scope tenancy.OrgScope, account, region, owner string, severity issues.Severity, tier issues.Tier, state issues.State, opened time.Time) {
	t.Helper()
	issueStore, ok := storage.AWSIssuesOf(store)
	if !ok {
		t.Fatal("no issue store")
	}
	issue := issues.Issue{ID: issues.IssueID("rule."+string(severity)+string(tier), owner), Rule: "rule." + string(severity), Kind: issues.KindSecurity,
		Severity: severity, Tier: tier, Account: account, Region: region, Owner: owner, Title: "issue on " + owner, State: state,
		Confirm: "confirmed", FirstSeen: opened, OpenedAt: opened, Version: 1, UpdatedAt: opened}
	if state == issues.StateRejected {
		issue.Rejection = &issues.Rejection{Reason: "how we work", By: "admin", At: opened, Digest: "d", SeverityAtReject: severity}
	}
	record, err := issues.Record(issue)
	if err != nil {
		t.Fatal(err)
	}
	if err := issueStore.WriteAWSIssues(context.Background(), scope, storage.AWSIssueBatch{AccountID: account,
		Writes: []storage.AWSIssueWrite{{Issue: record}}}); err != nil {
		t.Fatal(err)
	}
}

// appendChanges stores changes in account over the last day.
func appendChanges(t *testing.T, store storage.Provider, scope tenancy.OrgScope, account string, rows []storage.AWSChange) {
	t.Helper()
	inventory, ok := store.(storage.AWSInventoryStore)
	if !ok {
		t.Fatal("no inventory store")
	}
	now := time.Now().UTC()
	if err := inventory.AppendAWSChanges(context.Background(), scope, account, estateRegion, rows,
		[]storage.AWSInterval{{From: now.Add(-25 * time.Hour), To: now}}, nil); err != nil {
		t.Fatal(err)
	}
}

type overviewAnswer struct {
	Enabled bool `json:"enabled"`
	Summary struct {
		AccountsRead        int    `json:"accounts_read"`
		AccountsTotal       int    `json:"accounts_total"`
		ActNow              *int   `json:"act_now"`
		WorthALook          *int   `json:"worth_a_look"`
		Resources           *int   `json:"resources"`
		ResourcesCapped     bool   `json:"resources_capped"`
		GroupedShare        *int   `json:"grouped_share"`
		PeopleChanges24h    *int64 `json:"people_changes_24h"`
		PeopleChangesCapped bool   `json:"people_changes_capped"`
	} `json:"summary"`
	Warning *struct {
		NotRead int `json:"not_read"`
	} `json:"warning"`
	NeedsAttention []struct {
		ID   string `json:"id"`
		Tier string `json:"tier"`
	} `json:"needs_attention"`
	Accounts []struct {
		ID              string `json:"id"`
		Role            string `json:"role"`
		Read            string `json:"read"`
		NotReadReason   string `json:"not_read_reason"`
		Resources       *int   `json:"resources"`
		ResourcesCapped bool   `json:"resources_capped"`
		OpenIssues      *int   `json:"open_issues"`
	} `json:"accounts"`
	OtherAccounts struct {
		Count   int `json:"count"`
		Unknown int `json:"unknown"`
	} `json:"other_accounts"`
	PeopleChanges []struct {
		Concern   string `json:"concern"`
		EventName string `json:"event_name"`
	} `json:"people_changes"`
}

type topologyAnswer struct {
	Enabled bool   `json:"enabled"`
	Level   string `json:"level"`
	TooMany bool   `json:"too_many"`
	Hint    *struct {
		Level string `json:"level"`
		Hops  int    `json:"hops"`
	} `json:"hint"`
	NodeCount int `json:"node_count"`
	MaxNodes  int `json:"max_nodes"`
	Nodes     []struct {
		ID              string `json:"id"`
		Kind            string `json:"kind"`
		Account         string `json:"account"`
		Label           string `json:"label"`
		Read            string `json:"read"`
		NotReadReason   string `json:"not_read_reason"`
		Resources       *int   `json:"resources"`
		ResourcesCapped bool   `json:"resources_capped"`
		OpenIssues      *int   `json:"open_issues"`
	} `json:"nodes"`
	Links []struct {
		From        string `json:"from"`
		To          string `json:"to"`
		Type        string `json:"type"`
		Kind        string `json:"kind"`
		SeenFrom    string `json:"seen_from"`
		Count       int    `json:"count"`
		CountCapped bool   `json:"count_capped"`
	} `json:"links"`
}

// With the connector off both routes answer enabled=false; with it on and
// nothing read, every count is not read (null), never zero, and the account
// is one grey card that says why.
func TestEstateRoutesEmptyAndNotRead(t *testing.T) {
	app, _, config := estateApp(t, storage.NewMemory(), tenancy.OrgScope{}, estateRead)
	var overview overviewAnswer
	var topology topologyAnswer
	if status := getJSON(t, app, "/api/admin/aws/overview", &overview); status != 200 || !overview.Enabled {
		t.Fatalf("overview = %d %+v", status, overview)
	}
	if overview.Summary.Resources != nil || overview.Summary.GroupedShare != nil || overview.Summary.AccountsRead != 0 ||
		overview.Summary.AccountsTotal != 1 || overview.Warning == nil {
		t.Fatalf("nothing read but the summary reads %+v, warning %+v", overview.Summary, overview.Warning)
	}
	if len(overview.Accounts) != 1 || overview.Accounts[0].Read != estate.ReadNone || overview.Accounts[0].Resources != nil ||
		overview.Accounts[0].NotReadReason == "" {
		t.Fatalf("account rows = %+v; want one not-read account with a reason and no count", overview.Accounts)
	}
	if overview.NeedsAttention == nil || overview.PeopleChanges == nil {
		t.Fatal("lists must be empty arrays, never null")
	}
	if status := getJSON(t, app, "/api/admin/aws/topology", &topology); status != 200 || topology.Level != estate.LevelGroups ||
		len(topology.Nodes) != 1 || topology.Nodes[0].Read != estate.ReadNone || topology.Nodes[0].Resources != nil {
		t.Fatalf("topology of an unread account = %d %+v", status, topology)
	}
	config.Connectors.AWS.Enable = false
	overview, topology = overviewAnswer{}, topologyAnswer{}
	getJSON(t, app, "/api/admin/aws/overview", &overview)
	getJSON(t, app, "/api/admin/aws/topology", &topology)
	if overview.Enabled || topology.Enabled {
		t.Fatal("connector off but a route answered enabled")
	}
}

// The Overview answers every field: an account read and one whose read
// failed (not read, with its reason, no count), the issue tiers with Act
// now first, the grouped share, other accounts named by links, and only
// the changes by people that touched exposure or identity.
func TestOverviewAnswersEveryField(t *testing.T) {
	for name, provider := range estateProviders(t) {
		t.Run(name, func(t *testing.T) {
			store := provider(t)
			app, mounted, _ := estateApp(t, store, tenancy.OrgScope{}, estateRead, estateFailed)
			scope := mounted.Snapshots.Scope()
			commitFunctions(t, mounted, estateRead, "lambda", map[string]string{"cart": "shop", "pay": "shop", "loose": ""},
				map[string][]model.Relation{"cart": {{Type: model.RelationTrusts, Target: "arn:aws:iam::" + estateOutsider + ":role/ci", Kind: model.RelationKindMayAccess}}})
			if err := mounted.Snapshots.RecordFailure(model.Unit{Account: estateFailed, Region: estateRegion, Service: "lambda"}, "denied"); err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			for index := range 3 {
				writeIssue(t, store, scope, estateRead, fmt.Sprintf("owner-low-%d", index), issues.SeverityLow, issues.TierWorthALook, issues.StateOpen, now.Add(-time.Minute))
			}
			// A rule may list a critical finding as worth a look: it still
			// comes after every Act now issue.
			writeIssue(t, store, scope, estateRead, "owner-critical-look", issues.SeverityCritical, issues.TierWorthALook, issues.StateOpen, now)
			writeIssue(t, store, scope, estateRead, lambdaARN(estateRead, "cart"), issues.SeverityHigh, issues.TierActNow, issues.StateOpen, now.Add(-time.Hour))
			writeIssue(t, store, scope, estateRead, lambdaARN(estateRead, "pay"), issues.SeverityMedium, issues.TierActNow, issues.StateOpen, now.Add(-time.Hour))
			writeIssue(t, store, scope, estateRead, "owner-rejected", issues.SeverityCritical, issues.TierActNow, issues.StateRejected, now)
			person := string(model.ActorPerson)
			appendChanges(t, store, scope, estateRead, []storage.AWSChange{
				{EventID: "e1", Account: estateRead, Region: estateRegion, Service: "iam", EventTime: now.Add(-3 * time.Hour), EventName: "CreateUser", ActorClass: person, Actor: "alice"},
				{EventID: "e2", Account: estateRead, Region: estateRegion, Service: "ec2", EventTime: now.Add(-2 * time.Hour), EventName: "AuthorizeSecurityGroupIngress", ActorClass: person, Actor: "bob"},
				{EventID: "e3", Account: estateRead, Region: estateRegion, Service: "lambda", EventTime: now.Add(-time.Hour), EventName: "UpdateFunctionCode", ActorClass: person, Actor: "bob"},
				{EventID: "e4", Account: estateRead, Region: estateRegion, Service: "iam", EventTime: now.Add(-time.Hour), EventName: "AttachRolePolicy", ActorClass: string(model.ActorPipeline), Actor: "deploy"},
			})
			var overview overviewAnswer
			if status := getJSON(t, app, "/api/admin/aws/overview", &overview); status != 200 {
				t.Fatalf("overview = %d", status)
			}
			summary := overview.Summary
			if summary.AccountsRead != 1 || summary.AccountsTotal != 2 || summary.ActNow == nil || *summary.ActNow != 2 ||
				summary.WorthALook == nil || *summary.WorthALook != 4 {
				t.Fatalf("summary = %+v", summary)
			}
			if summary.Resources == nil || *summary.Resources != 3 || !summary.ResourcesCapped || summary.GroupedShare == nil || *summary.GroupedShare != 67 {
				t.Fatalf("resources = %v capped %v share %v; want 3, capped (one account not read), 67", deref(summary.Resources), summary.ResourcesCapped, deref(summary.GroupedShare))
			}
			if overview.Warning == nil || overview.Warning.NotRead == 0 {
				t.Fatalf("one account not read but no warning: %+v", overview.Warning)
			}
			if len(overview.NeedsAttention) != estate.MaxNeedsAttention || overview.NeedsAttention[0].Tier != string(issues.TierActNow) ||
				overview.NeedsAttention[1].Tier != string(issues.TierActNow) || overview.NeedsAttention[2].Tier != string(issues.TierWorthALook) {
				t.Fatalf("needs attention = %+v; want five, Act now first", overview.NeedsAttention)
			}
			rows := map[string]int{}
			for index, row := range overview.Accounts {
				rows[row.ID] = index
			}
			read, failed := overview.Accounts[rows[estateRead]], overview.Accounts[rows[estateFailed]]
			if read.Read != estate.ReadFull || read.Resources == nil || *read.Resources != 3 || read.ResourcesCapped || read.OpenIssues == nil || *read.OpenIssues != 6 {
				t.Fatalf("read account = %+v", read)
			}
			if failed.Read != estate.ReadNone || failed.Resources != nil || failed.NotReadReason == "" || failed.OpenIssues == nil || *failed.OpenIssues != 0 {
				t.Fatalf("failed account = %+v; want not read, no count, a reason", failed)
			}
			if overview.OtherAccounts.Count != 1 || overview.OtherAccounts.Unknown != 1 {
				t.Fatalf("other accounts = %+v; want the trusted outsider, unknown", overview.OtherAccounts)
			}
			if summary.PeopleChanges24h == nil || *summary.PeopleChanges24h != 3 {
				t.Fatalf("people changes = %v; want 3", deref64(summary.PeopleChanges24h))
			}
			concerns := map[string]string{}
			for _, change := range overview.PeopleChanges {
				concerns[change.EventName[strings.LastIndex(change.EventName, ":")+1:]] = change.Concern
			}
			if len(overview.PeopleChanges) != 2 || concerns["CreateUser"] != "identity" || concerns["AuthorizeSecurityGroupIngress"] != "exposure" {
				t.Fatalf("people changes = %+v; want the identity and the exposure change only", overview.PeopleChanges)
			}
		})
	}
}

func deref(value *int) any {
	if value == nil {
		return nil
	}
	return *value
}

func deref64(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

// Overview, the Issues page and the Changes page agree: the same Act now,
// Worth a look and accounts read, the same warning, the same count of
// changes by people; and the Topology account cards carry the Overview's
// resource and issue counts.
func TestOverviewIssuesChangesAndTopologyAgree(t *testing.T) {
	store := storage.NewMemory()
	app, mounted, _ := estateApp(t, store, tenancy.OrgScope{}, estateRead, estateFailed)
	scope := mounted.Snapshots.Scope()
	commitFunctions(t, mounted, estateRead, "lambda", map[string]string{"cart": "shop", "pay": ""}, nil)
	if err := mounted.Snapshots.RecordFailure(model.Unit{Account: estateFailed, Region: estateRegion, Service: "lambda"}, "denied"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	writeIssue(t, store, scope, estateRead, lambdaARN(estateRead, "cart"), issues.SeverityCritical, issues.TierActNow, issues.StateOpen, now)
	writeIssue(t, store, scope, estateRead, lambdaARN(estateRead, "pay"), issues.SeverityLow, issues.TierWorthALook, issues.StateAcknowledged, now)
	writeIssue(t, store, scope, estateRead, "owner-x", issues.SeverityHigh, issues.TierActNow, issues.StateRejected, now)
	appendChanges(t, store, scope, estateRead, []storage.AWSChange{
		{EventID: "p1", Account: estateRead, Region: estateRegion, Service: "iam", EventTime: now.Add(-time.Hour), EventName: "CreateRole", ActorClass: "person", Actor: "alice"},
		{EventID: "p2", Account: estateRead, Region: estateRegion, Service: "lambda", EventTime: now.Add(-2 * time.Hour), EventName: "UpdateFunctionCode", ActorClass: "person", Actor: "alice"},
	})
	var overview overviewAnswer
	getJSON(t, app, "/api/admin/aws/overview", &overview)
	var page struct {
		Summary issues.Summary `json:"summary"`
		Warning *struct {
			NotRead int `json:"not_read"`
		} `json:"warning"`
	}
	if status := getJSON(t, app, "/api/admin/aws/issues", &page); status != 200 {
		t.Fatalf("issues = %d", status)
	}
	var changes struct {
		Summary struct {
			ByActorClass map[string]int64 `json:"by_actor_class"`
		} `json:"summary"`
	}
	if status := getJSON(t, app, "/api/admin/aws/changes", &changes); status != 200 {
		t.Fatalf("changes = %d", status)
	}
	if overview.Summary.ActNow == nil || *overview.Summary.ActNow != page.Summary.ActNow() || *overview.Summary.ActNow != 1 ||
		*overview.Summary.WorthALook != page.Summary.WorthALook || overview.Summary.AccountsRead != page.Summary.AccountsRead ||
		overview.Summary.AccountsTotal != page.Summary.AccountsTotal {
		t.Fatalf("overview %+v and issues %+v disagree", overview.Summary, page.Summary)
	}
	if (overview.Warning == nil) != (page.Warning == nil) || overview.Warning == nil || overview.Warning.NotRead != page.Warning.NotRead {
		t.Fatalf("warning: overview %+v, issues %+v", overview.Warning, page.Warning)
	}
	if overview.Summary.PeopleChanges24h == nil || *overview.Summary.PeopleChanges24h != changes.Summary.ByActorClass["person"] ||
		changes.Summary.ByActorClass["person"] != 2 {
		t.Fatalf("people changes: overview %v, changes %v", deref64(overview.Summary.PeopleChanges24h), changes.Summary.ByActorClass)
	}
	var topology topologyAnswer
	if status := getJSON(t, app, "/api/admin/aws/topology?level=accounts", &topology); status != 200 {
		t.Fatalf("topology = %d", status)
	}
	cards := map[string]int{}
	for index, node := range topology.Nodes {
		cards[node.Account] = index
	}
	for _, row := range overview.Accounts {
		card := topology.Nodes[cards[row.ID]]
		if fmt.Sprint(deref(card.Resources)) != fmt.Sprint(deref(row.Resources)) || card.Read != row.Read ||
			fmt.Sprint(deref(card.OpenIssues)) != fmt.Sprint(deref(row.OpenIssues)) {
			t.Fatalf("account %s: topology %+v, overview %+v", row.ID, card, row)
		}
	}
}

// Topology query checks: a bad level, hops or lens is a 400; an unknown
// account, group or resource a 404.
func TestTopologyRefusesBadQueries(t *testing.T) {
	app, mounted, _ := estateApp(t, storage.NewMemory(), tenancy.OrgScope{}, estateRead)
	commitFunctions(t, mounted, estateRead, "lambda", map[string]string{"cart": "shop"}, nil)
	for path, want := range map[string]int{
		"/api/admin/aws/topology?level=map":                         400,
		"/api/admin/aws/topology?level=focus":                       400,
		"/api/admin/aws/topology?level=focus&ref=x&hops=4":          400,
		"/api/admin/aws/topology?level=focus&ref=x&hops=0":          400,
		"/api/admin/aws/topology?lens=cost":                         400,
		"/api/admin/aws/topology?account=12":                        400,
		"/api/admin/aws/topology?level=groups&account=999999999999": 404,
		"/api/admin/aws/topology?level=groups&group=stack:gone":     404,
		"/api/admin/aws/topology?level=focus&ref=arn:aws:s3:::gone": 404,
		"/api/admin/aws/topology?level=groups&group=stack:shop":     200,
		"/api/admin/aws/topology?level=focus&ref=" + lambdaARN(estateRead, "cart") + "&hops=3&lens=network,identity,data,changes": 200,
	} {
		if status := getJSON(t, app, path, nil); status != want {
			t.Errorf("%s = %d; want %d", path, status, want)
		}
	}
}

// More groups than a view holds answer too_many with a hint to focus,
// and no node: never a silently cut graph. A partly read account's link
// counts are capped.
func TestTopologyTooManyAndCapped(t *testing.T) {
	app, mounted, _ := estateApp(t, storage.NewMemory(), tenancy.OrgScope{}, estateRead)
	functions := map[string]string{}
	for index := range 301 {
		functions[fmt.Sprintf("fn%03d", index)] = fmt.Sprintf("stack%03d", index)
	}
	commitFunctions(t, mounted, estateRead, "lambda", functions, nil)
	var topology topologyAnswer
	if status := getJSON(t, app, "/api/admin/aws/topology?level=groups", &topology); status != 200 {
		t.Fatalf("groups = %d", status)
	}
	if !topology.TooMany || len(topology.Nodes) != 0 || len(topology.Links) != 0 || topology.Hint == nil ||
		topology.Hint.Level != estate.LevelFocus || topology.NodeCount <= topology.MaxNodes || topology.MaxNodes != 300 {
		t.Fatalf("301 groups = %+v; want too_many, no node, a focus hint", topology)
	}

	app, mounted, _ = estateApp(t, storage.NewMemory(), tenancy.OrgScope{}, estateRead)
	commitFunctions(t, mounted, estateRead, "lambda", map[string]string{"cart": "shop"},
		map[string][]model.Relation{"cart": {{Type: model.RelationTrusts, Target: "arn:aws:iam::" + estateOutsider + ":role/ci", Kind: model.RelationKindMayAccess}}})
	if err := mounted.Snapshots.RecordFailure(model.Unit{Account: estateRead, Region: estateRegion, Service: "iam"}, "denied"); err != nil {
		t.Fatal(err)
	}
	topology = topologyAnswer{}
	getJSON(t, app, "/api/admin/aws/topology?level=groups", &topology)
	capped := false
	for _, link := range topology.Links {
		capped = capped || link.CountCapped && link.SeenFrom == estate.SeenOne
	}
	var card *int
	resourcesCapped := false
	for _, node := range topology.Nodes {
		if node.ID == "group:"+estateRead+"/stack:shop" {
			card, resourcesCapped = node.Resources, node.ResourcesCapped
		}
	}
	if !capped || card == nil || *card != 1 || !resourcesCapped {
		t.Fatalf("partly read account: links %+v, nodes %+v; want capped counts", topology.Links, topology.Nodes)
	}
}

// A removed account: the links only it read disappear, and the links the
// other account read stay, seen from one side, with no error. Under a
// licence the default organization's rows of the removed account stay
// hidden too.
func TestTopologyAfterAnAccountIsRemoved(t *testing.T) {
	for _, licensed := range []bool{false, true} {
		t.Run(fmt.Sprintf("licensed=%v", licensed), func(t *testing.T) {
			store := storage.NewMemory()
			scope := tenancy.OrgScope{}
			if licensed {
				scope = tenancy.NewOrgScope("acme", tenancy.DefaultOrgID)
			}
			app, mounted, config := estateApp(t, store, scope, estateRead, estateFailed)
			remoteRole := "arn:aws:iam::" + estateFailed + ":role/remote"
			removedWriter := mounted.Snapshots
			if licensed {
				// The removed account's reads were stored before the
				// licence, under the default organization.
				removedWriter = awssnapshot.New(store, tenancy.DefaultOrgScope())
			}
			commitFunctions(t, mounted, estateRead, "lambda", map[string]string{"cart": "shop"},
				map[string][]model.Relation{"cart": {{Type: model.RelationTrusts, Target: remoteRole, Kind: model.RelationKindMayAccess}}})
			remote := model.Resource{ARN: remoteRole, Service: "iam", Type: "role", Account: estateFailed,
				Region: model.GlobalRegion, Name: "remote", Relations: []model.Relation{{Type: model.RelationRoutesTo, Target: lambdaARN(estateRead, "cart")}}}
			if _, err := removedWriter.Commit(model.Unit{Account: estateFailed, Region: model.GlobalRegion, Service: "iam"},
				awssnapshot.Candidate{Resources: []model.Resource{remote}}); err != nil {
				t.Fatal(err)
			}
			links := func() map[string]string {
				var topology topologyAnswer
				if status := getJSON(t, app, "/api/admin/aws/topology?level=accounts", &topology); status != 200 {
					t.Fatalf("accounts = %d", status)
				}
				out := map[string]string{}
				for _, link := range topology.Links {
					out[link.From+">"+link.To+" "+link.Type] = link.SeenFrom
				}
				return out
			}
			trust := "account:" + estateRead + ">account:" + estateFailed + " trusts"
			routes := "account:" + estateFailed + ">account:" + estateRead + " routes_to"
			if got := links(); got[routes] == "" || got[trust] != estate.SeenBoth {
				t.Fatalf("both connected: links %v", got)
			}
			config.Connectors.AWS.Accounts = config.Connectors.AWS.Accounts[:1]
			got := links()
			if _, ok := got[routes]; ok {
				t.Fatalf("a link only the removed account read is still drawn: %v", got)
			}
			if got[trust] != estate.SeenOne {
				t.Fatalf("the link the connected account read = %q; want seen from one side (%v)", got[trust], got)
			}
		})
	}
}

type countedNode struct {
	ID            string `json:"id"`
	Account       string `json:"account"`
	Read          string `json:"read"`
	OpenIssues    *int   `json:"open_issues"`
	Changes24h    *int   `json:"changes_24h"`
	ChangesCapped bool   `json:"changes_capped"`
}

func topologyNodes(t *testing.T, app *fiber.App, path string) map[string]countedNode {
	t.Helper()
	var answer struct {
		Nodes []countedNode `json:"nodes"`
	}
	if status := getJSON(t, app, path, &answer); status != 200 {
		t.Fatalf("%s = %d", path, status)
	}
	out := map[string]countedNode{}
	for _, node := range answer.Nodes {
		out[node.ID] = node
	}
	return out
}

// Every account a view shows carries its own counts, read from its own
// issues and changes: the groups view of one account shows the other
// connected account's open issues as Overview does, never a known zero,
// and an account that is not connected carries no count at all. An
// account's changes count every change it holds, also those with no
// resource or with an ARN that names no account.
func TestTopologyCountsEveryShownAccount(t *testing.T) {
	store := storage.NewMemory()
	app, mounted, _ := estateApp(t, store, tenancy.OrgScope{}, estateRead, estateFailed)
	scope := mounted.Snapshots.Scope()
	commitFunctions(t, mounted, estateRead, "lambda", map[string]string{"cart": "shop"},
		map[string][]model.Relation{"cart": {
			{Type: model.RelationRoutesTo, Target: lambdaARN(estateFailed, "remote")},
			{Type: model.RelationTrusts, Target: "arn:aws:iam::" + estateOutsider + ":role/ci", Kind: model.RelationKindMayAccess},
		}})
	commitFunctions(t, mounted, estateFailed, "lambda", map[string]string{"remote": "edge"}, nil)
	now := time.Now().UTC()
	for index := range 3 {
		writeIssue(t, store, scope, estateFailed, fmt.Sprintf("owner-remote-%d", index), issues.SeverityHigh, issues.TierActNow, issues.StateOpen, now)
	}
	appendChanges(t, store, scope, estateRead, []storage.AWSChange{
		{EventID: "c1", Account: estateRead, Region: estateRegion, Service: "lambda", EventTime: now.Add(-time.Hour), EventName: "UpdateFunctionCode",
			ActorClass: "person", Actor: "alice", ResourceARN: lambdaARN(estateRead, "cart")},
		{EventID: "c2", Account: estateRead, Region: estateRegion, Service: "s3", EventTime: now.Add(-2 * time.Hour), EventName: "PutBucketPolicy",
			ActorClass: "person", Actor: "alice", ResourceARN: "arn:aws:s3:::shop-assets"},
		{EventID: "c3", Account: estateRead, Region: estateRegion, Service: "iam", EventTime: now.Add(-3 * time.Hour), EventName: "CreateUser",
			ActorClass: "person", Actor: "alice"},
	})
	var overview overviewAnswer
	getJSON(t, app, "/api/admin/aws/overview", &overview)
	open := map[string]any{}
	for _, row := range overview.Accounts {
		open[row.ID] = deref(row.OpenIssues)
	}
	groups := topologyNodes(t, app, "/api/admin/aws/topology?level=groups&account="+estateRead)
	remote := groups["account:"+estateFailed]
	if remote.OpenIssues == nil || *remote.OpenIssues != 3 || open[estateFailed] != 3 {
		t.Fatalf("other account in the groups view: open issues %v, overview %v; want 3 in both", deref(remote.OpenIssues), open[estateFailed])
	}
	if outsider, ok := groups["account:"+estateOutsider]; !ok || outsider.OpenIssues != nil {
		t.Fatalf("an account not connected carries a count: %+v (drawn %v)", outsider, ok)
	}
	focus := topologyNodes(t, app, "/api/admin/aws/topology?level=focus&ref="+lambdaARN(estateRead, "cart")+"&lens=changes")
	if node := focus[lambdaARN(estateFailed, "remote")]; node.OpenIssues == nil || node.Changes24h == nil || *node.Changes24h != 0 {
		t.Fatalf("other account's resource in focus: %+v; want its own counts, read", node)
	}
	if node := focus[lambdaARN(estateRead, "cart")]; node.Changes24h == nil || *node.Changes24h != 1 {
		t.Fatalf("changed resource: %+v; want one change", node)
	}
	accounts := topologyNodes(t, app, "/api/admin/aws/topology?level=accounts&lens=changes")
	if card := accounts["account:"+estateRead]; card.Changes24h == nil || *card.Changes24h != 3 {
		t.Fatalf("account card changes = %v; want all 3, with and without a resource", deref(card.Changes24h))
	}
	if card := accounts["account:"+estateFailed]; card.Changes24h == nil || *card.Changes24h != 0 || card.OpenIssues == nil || *card.OpenIssues != 3 {
		t.Fatalf("second account card = %+v", card)
	}
}

// Needs attention keeps every Act now issue first even when hundreds of
// Worth a look issues of higher severity rank above them in the queue.
func TestNeedsAttentionKeepsActNowFirst(t *testing.T) {
	for name, provider := range estateProviders(t) {
		t.Run(name, func(t *testing.T) {
			store := provider(t)
			app, mounted, _ := estateApp(t, store, tenancy.OrgScope{}, estateRead)
			scope := mounted.Snapshots.Scope()
			now := time.Now().UTC()
			for index := range 250 {
				writeIssue(t, store, scope, estateRead, fmt.Sprintf("owner-look-%03d", index), issues.SeverityCritical, issues.TierWorthALook, issues.StateOpen, now)
			}
			writeIssue(t, store, scope, estateRead, "owner-act", issues.SeverityMedium, issues.TierActNow, issues.StateOpen, now.Add(-time.Hour))
			var overview overviewAnswer
			getJSON(t, app, "/api/admin/aws/overview", &overview)
			if overview.Summary.ActNow == nil || *overview.Summary.ActNow != 1 || len(overview.NeedsAttention) != estate.MaxNeedsAttention ||
				overview.NeedsAttention[0].Tier != string(issues.TierActNow) {
				t.Fatalf("act now %v, needs attention %+v; want the Act now issue first", deref(overview.Summary.ActNow), overview.NeedsAttention)
			}
		})
	}
}

// Under a licence, the open-source organization's change rows of an account
// that is not connected stay out of the changes by people, their count and
// the changes lens.
func TestLicensedPeopleChangesExcludeRemovedAccount(t *testing.T) {
	store := storage.NewMemory()
	scope := tenancy.NewOrgScope("acme", tenancy.DefaultOrgID)
	app, mounted, _ := estateApp(t, store, scope, estateRead)
	commitFunctions(t, mounted, estateRead, "lambda", map[string]string{"cart": "shop"}, nil)
	now := time.Now().UTC()
	appendChanges(t, store, mounted.Snapshots.Scope(), estateRead, []storage.AWSChange{
		{EventID: "own", Account: estateRead, Region: estateRegion, Service: "iam", EventTime: now.Add(-time.Hour), EventName: "CreateRole",
			ActorClass: "person", Actor: "alice", ResourceARN: lambdaARN(estateRead, "cart")},
	})
	// The removed account's rows were stored before the licence.
	appendChanges(t, store, tenancy.DefaultOrgScope(), estateFailed, []storage.AWSChange{
		{EventID: "gone", Account: estateFailed, Region: estateRegion, Service: "iam", EventTime: now.Add(-time.Hour), EventName: "CreateUser",
			ActorClass: "person", Actor: "mallory", ResourceARN: lambdaARN(estateRead, "cart")},
	})
	var overview overviewAnswer
	getJSON(t, app, "/api/admin/aws/overview", &overview)
	if overview.Summary.PeopleChanges24h == nil || *overview.Summary.PeopleChanges24h != 1 || len(overview.PeopleChanges) != 1 {
		t.Fatalf("people changes %v, rows %+v; want only the connected account's one", deref64(overview.Summary.PeopleChanges24h), overview.PeopleChanges)
	}
	focus := topologyNodes(t, app, "/api/admin/aws/topology?level=focus&ref="+lambdaARN(estateRead, "cart")+"&lens=changes")
	accounts := topologyNodes(t, app, "/api/admin/aws/topology?level=accounts&lens=changes")
	if node := focus[lambdaARN(estateRead, "cart")]; node.Changes24h == nil || *node.Changes24h != 1 {
		t.Fatalf("changes lens on the resource = %v; want 1", deref(node.Changes24h))
	}
	if card := accounts["account:"+estateRead]; card.Changes24h == nil || *card.Changes24h != 1 {
		t.Fatalf("changes lens on the account = %v; want 1", deref(card.Changes24h))
	}
	if _, drawn := accounts["account:"+estateFailed]; drawn {
		t.Fatal("the removed account is drawn")
	}
}

// The page reads share the stored account listing until the account
// revision changes: with ten accounts, an account written without raising
// the revision is not seen by Overview, Topology, Coverage or Issues, so no
// request listed the stored accounts again; a save raises the revision
// and every page sees it.
func TestEstateReadsReuseTheAccountListing(t *testing.T) {
	store := storage.NewMemory()
	scope := tenancy.NewOrgScope(fmt.Sprintf("listing%d", time.Now().UnixNano()))
	app, mounted, _ := estateApp(t, store, scope)
	settingsStore := awssettings.NewStore(store, scope)
	for index := range 10 {
		if err := settingsStore.SaveAccount(scope.Write, awssettings.AccountRecord{AccountID: fmt.Sprintf("1234000002%02d", index),
			Regions: []string{estateRegion}}); err != nil {
			t.Fatal(err)
		}
	}
	totals := func() []int {
		var overview overviewAnswer
		getJSON(t, app, "/api/admin/aws/overview", &overview)
		var coverage struct {
			Summary struct {
				AccountsTotal int `json:"accounts_total"`
			} `json:"summary"`
		}
		getJSON(t, app, "/api/admin/aws/coverage", &coverage)
		var page struct {
			Summary issues.Summary `json:"summary"`
		}
		getJSON(t, app, "/api/admin/aws/issues", &page)
		var topology topologyAnswer
		getJSON(t, app, "/api/admin/aws/topology?level=accounts", &topology)
		return []int{overview.Summary.AccountsTotal, coverage.Summary.AccountsTotal, page.Summary.AccountsTotal, len(topology.Nodes)}
	}
	if got := totals(); fmt.Sprint(got) != "[10 10 10 10]" {
		t.Fatalf("totals = %v", got)
	}
	record, _ := json.Marshal(awssettings.AccountRecord{AccountID: "123400000299", Regions: []string{estateRegion}})
	if err := store.WriteBlob(awssettings.AccountPrefix(scope.Write)+"123400000299", record); err != nil {
		t.Fatal(err)
	}
	if got := totals(); fmt.Sprint(got) != "[10 10 10 10]" {
		t.Fatalf("a request listed the stored accounts again at the same revision: totals %v", got)
	}
	if err := settingsStore.SaveAccount(scope.Write, awssettings.AccountRecord{AccountID: "123400000298", Regions: []string{estateRegion}}); err != nil {
		t.Fatal(err)
	}
	if got := totals(); fmt.Sprint(got) != "[12 12 12 12]" {
		t.Fatalf("after a save the pages read %v; want 12 everywhere", got)
	}
	_ = mounted
}

// The Issues page, its report and Overview count and list only issues of
// connected accounts: a removed account's open issues stay out of every
// summary and list before the evaluator resolves them.
func TestIssueReadsLeaveOutRemovedAccounts(t *testing.T) {
	store := storage.NewMemory()
	app, mounted, _ := estateApp(t, store, tenancy.OrgScope{}, estateRead)
	scope := mounted.Snapshots.Scope()
	now := time.Now().UTC()
	writeIssue(t, store, scope, estateRead, "owner-kept", issues.SeverityHigh, issues.TierActNow, issues.StateOpen, now)
	writeIssue(t, store, scope, estateFailed, "owner-gone", issues.SeverityCritical, issues.TierActNow, issues.StateOpen, now)
	var page struct {
		Summary issues.Summary `json:"summary"`
		Items   []issues.Issue `json:"items"`
	}
	if status := getJSON(t, app, "/api/admin/aws/issues", &page); status != 200 {
		t.Fatalf("issues = %d", status)
	}
	if page.Summary.Open != 1 || len(page.Items) != 1 || page.Items[0].Account != estateRead {
		t.Fatalf("issues page = %+v, %d items; want only the connected account's issue", page.Summary, len(page.Items))
	}
	var report struct {
		Rows int `json:"rows"`
	}
	getJSON(t, app, "/api/admin/aws/issues/report", &report)
	var removed struct {
		Summary issues.Summary `json:"summary"`
	}
	getJSON(t, app, "/api/admin/aws/issues?account="+estateFailed, &removed)
	if report.Rows != 1 || removed.Summary.Open != 0 {
		t.Fatalf("report rows %d, removed account's own page %+v; want 1 and none", report.Rows, removed.Summary)
	}
	var overview overviewAnswer
	getJSON(t, app, "/api/admin/aws/overview", &overview)
	if deref(overview.Summary.ActNow) != 1 || len(overview.NeedsAttention) != 1 {
		t.Fatalf("overview act now %v, needs attention %+v; want only the connected account's issue", deref(overview.Summary.ActNow), overview.NeedsAttention)
	}
}

// An account whose only read failed says why: the failure's cause class,
// never "not read yet". The grouped share of no resources is null.
func TestNotReadReasonAndEmptyGroupedShare(t *testing.T) {
	app, mounted, _ := estateApp(t, storage.NewMemory(), tenancy.OrgScope{}, estateRead, estateFailed)
	if _, err := mounted.Snapshots.Commit(model.Unit{Account: estateRead, Region: estateRegion, Service: "lambda"}, awssnapshot.Candidate{}); err != nil {
		t.Fatal(err)
	}
	// The collectors record a denied read under the code "denied".
	if err := mounted.Snapshots.RecordFailure(model.Unit{Account: estateFailed, Region: estateRegion, Service: "lambda"}, "denied"); err != nil {
		t.Fatal(err)
	}
	var overview overviewAnswer
	getJSON(t, app, "/api/admin/aws/overview", &overview)
	reasons := map[string]string{}
	for _, row := range overview.Accounts {
		reasons[row.ID] = row.NotReadReason
	}
	if reasons[estateFailed] != string(model.GapDenied) {
		t.Fatalf("failed account reason = %q; want %q", reasons[estateFailed], model.GapDenied)
	}
	if overview.Summary.Resources == nil || *overview.Summary.Resources != 0 || overview.Summary.GroupedShare != nil {
		t.Fatalf("resources %v, grouped share %v; want a known 0 and a null share", deref(overview.Summary.Resources), deref(overview.Summary.GroupedShare))
	}
}
