package mount

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
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

// Four connected accounts in four read states: one read with nothing in
// it, one added and never read, one whose only read failed, and one read
// in part.
const (
	countsEmpty   = "123400000201"
	countsNew     = "123400000202"
	countsFailed  = "123400000203"
	countsPartial = "123400000204"
)

// Every count says whether it is known, a lower bound or not read: an
// account read with nothing in it counts a known zero; an account added
// but not read, and one whose read failed, carry no count and a reason
// (the failed one says it failed, not that it was never tried); a partly
// read account's count is capped, and so is every sum over it; changes by
// people over a window nobody polled are a lower bound, over a settled
// window a known zero.
func TestEstateCountsAreKnownCappedOrNotRead(t *testing.T) {
	store := storage.NewMemory()
	app, mounted, _ := estateApp(t, store, tenancy.OrgScope{}, countsEmpty, countsNew, countsFailed, countsPartial)
	if _, err := mounted.Snapshots.Commit(model.Unit{Account: countsEmpty, Region: estateRegion, Service: "lambda"},
		awssnapshot.Candidate{}); err != nil {
		t.Fatal(err)
	}
	if err := mounted.Snapshots.RecordFailure(model.Unit{Account: countsFailed, Region: estateRegion, Service: "lambda"}, "AccessDenied"); err != nil {
		t.Fatal(err)
	}
	commitFunctions(t, mounted, countsPartial, "lambda", map[string]string{"only": "shop"}, nil)
	if err := mounted.Snapshots.RecordFailure(model.Unit{Account: countsPartial, Region: estateRegion, Service: "sqs"}, "AccessDenied"); err != nil {
		t.Fatal(err)
	}

	var overview overviewAnswer
	if status := getJSON(t, app, "/api/admin/aws/overview", &overview); status != 200 {
		t.Fatalf("overview = %d", status)
	}
	rows := map[string]int{}
	for index, row := range overview.Accounts {
		rows[row.ID] = index
	}
	if len(overview.Accounts) != 4 {
		t.Fatalf("account rows = %+v; want four", overview.Accounts)
	}
	empty := overview.Accounts[rows[countsEmpty]]
	if empty.Read != estate.ReadFull || empty.Resources == nil || *empty.Resources != 0 || empty.ResourcesCapped ||
		empty.OpenIssues == nil || *empty.OpenIssues != 0 {
		t.Errorf("account read with nothing in it = %+v; want read, a known 0 resources and 0 issues", empty)
	}
	fresh := overview.Accounts[rows[countsNew]]
	if fresh.Read != estate.ReadNone || fresh.Resources != nil || fresh.NotReadReason != awssnapshot.AccountNotReadYet {
		t.Errorf("account added, not read = %+v; want not read, no count, reason %q", fresh, awssnapshot.AccountNotReadYet)
	}
	failed := overview.Accounts[rows[countsFailed]]
	if failed.Read != estate.ReadNone || failed.Resources != nil || failed.ResourcesCapped {
		t.Errorf("account whose read failed = %+v; want not read and no count", failed)
	}
	if failed.NotReadReason == "" || failed.NotReadReason == awssnapshot.AccountNotReadYet {
		t.Errorf("account whose read failed gives reason %q; want a reason that says the read failed, not that it was never tried",
			failed.NotReadReason)
	}
	partial := overview.Accounts[rows[countsPartial]]
	if partial.Read != estate.ReadPartial || partial.Resources == nil || *partial.Resources != 1 || !partial.ResourcesCapped {
		t.Errorf("account read in part = %+v; want partial, 1 resource, capped", partial)
	}
	summary := overview.Summary
	if summary.AccountsTotal != 4 || summary.Resources == nil || *summary.Resources != 1 || !summary.ResourcesCapped {
		t.Errorf("summary = %+v; want 4 accounts and more than 1 resource (capped)", summary)
	}
	if summary.ActNow == nil || *summary.ActNow != 0 || summary.WorthALook == nil || *summary.WorthALook != 0 {
		t.Errorf("no open issue: act now %v, worth a look %v; want known zeros", deref(summary.ActNow), deref(summary.WorthALook))
	}
	if summary.PeopleChanges24h == nil || *summary.PeopleChanges24h != 0 || !summary.PeopleChangesCapped {
		t.Errorf("no change poll yet: people changes %v capped %v; want 0 as a lower bound", deref64(summary.PeopleChanges24h),
			summary.PeopleChangesCapped)
	}

	cards := panelNodes(t, app, "/api/admin/aws/topology?level=accounts")
	for _, account := range []string{countsNew, countsFailed} {
		card := cards["account:"+account]
		if card.Resources != nil {
			t.Errorf("topology card of unread account %s carries %d resources; want none", account, *card.Resources)
		}
	}
	if card := cards["account:"+countsEmpty]; card.Resources == nil || *card.Resources != 0 {
		t.Errorf("topology card of the empty account = %v; want a known 0", deref(card.Resources))
	}
	groups := topologyNodes(t, app, "/api/admin/aws/topology?level=groups&account="+countsFailed)
	if len(groups) != 1 || groups["account:"+countsFailed].Read != estate.ReadNone {
		t.Errorf("groups of the failed account = %+v; want one not-read card, never an empty account", groups)
	}

	// Over the empty account alone, with its change window settled, every
	// count is a known zero.
	settleChanges(t, store, mounted.Snapshots.Scope(), countsEmpty)
	var alone overviewAnswer
	getJSON(t, app, "/api/admin/aws/overview?account="+countsEmpty, &alone)
	if alone.Summary.Resources == nil || *alone.Summary.Resources != 0 || alone.Summary.ResourcesCapped {
		t.Errorf("empty account alone: resources %v capped %v; want a known 0", deref(alone.Summary.Resources), alone.Summary.ResourcesCapped)
	}
	if alone.Summary.PeopleChanges24h == nil || *alone.Summary.PeopleChanges24h != 0 || alone.Summary.PeopleChangesCapped {
		t.Errorf("settled window: people changes %v capped %v; want a known 0", deref64(alone.Summary.PeopleChanges24h),
			alone.Summary.PeopleChangesCapped)
	}
}

// parityCards sums the Act now and Worth a look counts of the account
// cards of the scope's accounts, every connected one when it names none.
func parityCards(t *testing.T, app *fiber.App, query string, accounts map[string]bool) (int, int) {
	t.Helper()
	act, worth := 0, 0
	for _, card := range panelNodes(t, app, "/api/admin/aws/topology?level=accounts"+query) {
		if card.Kind != estate.NodeKindAccount || !accounts[card.Account] {
			continue
		}
		if card.ActNow == nil || card.WorthALook == nil {
			t.Fatalf("connected card %s carries no tier counts: %+v", card.Account, card)
		}
		act, worth = act+*card.ActNow, worth+*card.WorthALook
	}
	return act, worth
}

// Overview's Act now and Worth a look equal the Issues summary and the sum
// of the Topology account cards over the same scope: none, one account,
// both, a Region, and an account in a Region. A Region keeps its issues
// and the account-wide ones.
func TestOverviewTiersMatchIssuesAndAccountCards(t *testing.T) {
	for name, provider := range estateProviders(t) {
		t.Run(name, func(t *testing.T) {
			store := provider(t)
			app, mounted, _ := estateApp(t, store, tenancy.OrgScope{}, estateRead, estateFailed)
			scope := mounted.Snapshots.Scope()
			commitFunctions(t, mounted, estateRead, "lambda", map[string]string{"cart": "shop"}, nil)
			commitFunctions(t, mounted, estateFailed, "lambda", map[string]string{"edge": ""}, nil)
			now := time.Now().UTC()
			writeIssue(t, store, scope, estateRead, lambdaARN(estateRead, "cart"), issues.SeverityHigh, issues.TierActNow, issues.StateOpen, now)
			writeIssueIn(t, store, scope, estateRead, "us-east-1", "owner-far", issues.SeverityLow, issues.TierWorthALook, issues.StateOpen, now)
			writeIssueIn(t, store, scope, estateRead, model.GlobalRegion, "owner-global", issues.SeverityMedium, issues.TierActNow, issues.StateAcknowledged, now)
			writeIssue(t, store, scope, estateFailed, lambdaARN(estateFailed, "edge"), issues.SeverityLow, issues.TierWorthALook, issues.StateRegressed, now)
			writeIssueIn(t, store, scope, estateFailed, "us-east-1", "owner-east", issues.SeverityCritical, issues.TierActNow, issues.StateOpen, now)
			writeIssue(t, store, scope, estateFailed, "owner-rejected", issues.SeverityCritical, issues.TierActNow, issues.StateRejected, now)

			both := map[string]bool{estateRead: true, estateFailed: true}
			for _, tc := range []struct {
				query           string
				accounts        map[string]bool
				issuesQuery     string
				wantAct, wantWo int
			}{
				{"", both, "", 3, 2},
				{"&account=" + estateRead, map[string]bool{estateRead: true}, "?account=" + estateRead, 2, 1},
				{"&account=" + estateFailed, map[string]bool{estateFailed: true}, "?account=" + estateFailed, 1, 1},
				{"&account=" + estateRead + "," + estateFailed, both, "", 3, 2},
				{"&region=" + estateRegion, both, "-", 2, 1},
				{"&region=us-east-1", both, "-", 2, 1},
				{"&account=" + estateRead + "&region=us-east-1", map[string]bool{estateRead: true}, "-", 1, 1},
			} {
				var overview overviewAnswer
				if status := getJSON(t, app, "/api/admin/aws/overview?"+strings.TrimPrefix(tc.query, "&"), &overview); status != 200 {
					t.Fatalf("overview %q = %d", tc.query, status)
				}
				if overview.Summary.ActNow == nil || overview.Summary.WorthALook == nil {
					t.Fatalf("overview %q tiers not read", tc.query)
				}
				act, worth := *overview.Summary.ActNow, *overview.Summary.WorthALook
				if act != tc.wantAct || worth != tc.wantWo {
					t.Errorf("overview %q = act %d look %d; want %d and %d", tc.query, act, worth, tc.wantAct, tc.wantWo)
				}
				cardAct, cardWorth := parityCards(t, app, tc.query, tc.accounts)
				if cardAct != act || cardWorth != worth {
					t.Errorf("scope %q: overview act %d look %d, account cards act %d look %d", tc.query, act, worth, cardAct, cardWorth)
				}
				if tc.issuesQuery == "-" {
					continue // the Issues page has no Region filter
				}
				var page struct {
					Summary issues.Summary `json:"summary"`
				}
				if status := getJSON(t, app, "/api/admin/aws/issues"+tc.issuesQuery, &page); status != 200 {
					t.Fatalf("issues %q = %d", tc.issuesQuery, status)
				}
				if page.Summary.ActNow() != act || page.Summary.WorthALook != worth {
					t.Errorf("scope %q: overview act %d look %d, issues act %d look %d", tc.query, act, worth, page.Summary.ActNow(),
						page.Summary.WorthALook)
				}
			}
		})
	}
}

// An account removed from settings drops out of every count: its open
// issues are in neither Overview's tiers nor the account cards, with or
// without a licence (where its rows were stored under the default
// organization before the licence).
func TestRemovedAccountIssuesStayOutOfOverviewCounts(t *testing.T) {
	for _, licensed := range []bool{false, true} {
		t.Run(fmt.Sprintf("licensed=%v", licensed), func(t *testing.T) {
			store := storage.NewMemory()
			scope := tenancy.OrgScope{}
			if licensed {
				scope = tenancy.NewOrgScope("acme", tenancy.DefaultOrgID)
			}
			app, mounted, _ := estateApp(t, store, scope, estateRead)
			commitFunctions(t, mounted, estateRead, "lambda", map[string]string{"cart": "shop"}, nil)
			now := time.Now().UTC()
			writeIssue(t, store, mounted.Snapshots.Scope(), estateRead, lambdaARN(estateRead, "cart"), issues.SeverityHigh, issues.TierActNow,
				issues.StateOpen, now)
			removedScope := mounted.Snapshots.Scope()
			if licensed {
				removedScope = tenancy.DefaultOrgScope()
			}
			writeIssue(t, store, removedScope, estateOutsider, lambdaARN(estateOutsider, "gone"), issues.SeverityCritical, issues.TierActNow,
				issues.StateOpen, now)
			writeIssue(t, store, removedScope, estateOutsider, lambdaARN(estateOutsider, "gone-too"), issues.SeverityLow, issues.TierWorthALook,
				issues.StateOpen, now)

			var overview overviewAnswer
			getJSON(t, app, "/api/admin/aws/overview", &overview)
			act, worth := parityCards(t, app, "", map[string]bool{estateRead: true})
			if deref(overview.Summary.ActNow) != 1 || deref(overview.Summary.WorthALook) != 0 || act != 1 || worth != 0 {
				t.Errorf("overview act %v look %v, cards act %d look %d; want 1 and 0 everywhere (the removed account's issues left out)",
					deref(overview.Summary.ActNow), deref(overview.Summary.WorthALook), act, worth)
			}
			var listed struct {
				NeedsAttention []struct {
					ID      string `json:"id"`
					Account string `json:"account"`
				} `json:"needs_attention"`
			}
			getJSON(t, app, "/api/admin/aws/overview", &listed)
			for _, row := range listed.NeedsAttention {
				if row.Account == estateOutsider {
					t.Errorf("needs attention lists the removed account's issue %s", row.ID)
				}
			}
		})
	}
}

// Under a licence, the default organization's stored resources of an
// account removed after the licence never appear in a search or a Focus
// view, while those of an account still connected do.
func TestLicensedSearchAndFocusLeaveOutRemovedAccount(t *testing.T) {
	store := storage.NewMemory()
	app, _, _ := estateApp(t, store, tenancy.NewOrgScope("acme", tenancy.DefaultOrgID), estateRead)
	before := awssnapshot.New(store, tenancy.DefaultOrgScope())
	for account, name := range map[string]string{estateRead: "checkout-kept", estateFailed: "checkout-removed"} {
		if _, err := before.Commit(model.Unit{Account: account, Region: estateRegion, Service: "lambda"}, awssnapshot.Candidate{
			Resources: []model.Resource{{ARN: lambdaARN(account, name), Service: "lambda", Type: "function", Account: account,
				Region: estateRegion, Name: name}}}); err != nil {
			t.Fatal(err)
		}
	}
	var found struct {
		Items []estate.SearchItem `json:"items"`
	}
	if status := getJSON(t, app, "/api/admin/aws/topology/search?q=checkout", &found); status != 200 {
		t.Fatalf("search = %d", status)
	}
	if len(found.Items) != 1 || found.Items[0].Account != estateRead {
		t.Fatalf("search = %+v; want only the connected account's inherited resource", found.Items)
	}
	if status := getJSON(t, app, "/api/admin/aws/topology?level=focus&ref="+url.QueryEscape(lambdaARN(estateFailed, "checkout-removed")), nil); status != 404 {
		t.Fatalf("focus on the removed account's resource = %d; want 404", status)
	}
	if status := getJSON(t, app, "/api/admin/aws/topology/search?q=checkout&account="+estateFailed, nil); status != 404 {
		t.Fatalf("search scoped to the removed account = %d; want 404", status)
	}
}

// Every bad parameter is a 400 and every well-formed value the stored
// reads do not hold a 404, on all three routes; a scope names at most 50
// accounts.
func TestEstateRoutesRefuseBadAndUnknownValues(t *testing.T) {
	app, mounted, _ := estateApp(t, storage.NewMemory(), tenancy.OrgScope{}, estateRead, estateFailed)
	commitFunctions(t, mounted, estateRead, "lambda", map[string]string{"cart": "shop"}, nil)
	accounts := func(count int) string {
		list := make([]string, 0, count)
		for index := range count {
			list = append(list, fmt.Sprintf("9999%08d", index))
		}
		return strings.Join(list, ",")
	}
	cart := url.QueryEscape(lambdaARN(estateRead, "cart"))
	for path, want := range map[string]int{
		// The scope bar, on every route.
		"/api/admin/aws/overview?account=" + accounts(51):                  400,
		"/api/admin/aws/overview?account=" + accounts(50):                  404,
		"/api/admin/aws/topology?account=" + accounts(51):                  400,
		"/api/admin/aws/topology?level=accounts&account=" + accounts(50):   404,
		"/api/admin/aws/topology/search?q=cart&account=" + accounts(51):    400,
		"/api/admin/aws/topology/search?q=cart&account=" + accounts(50):    404,
		"/api/admin/aws/overview?account=12345678901a":                     400,
		"/api/admin/aws/overview?account=1234000001011":                    400,
		"/api/admin/aws/overview?region=EU-SOUTH-2":                        400,
		"/api/admin/aws/overview?region=eu-south-2":                        200,
		"/api/admin/aws/overview?region=us-gov-west-1":                     200,
		"/api/admin/aws/overview?account=" + estateOutsider:                404,
		"/api/admin/aws/topology/search?q=cart&account=" + estateOutsider:  404,
		"/api/admin/aws/topology/search?q=cart&region=mars":                400,
		"/api/admin/aws/topology?level=accounts&account=" + estateOutsider: 404,
		// Topology values.
		"/api/admin/aws/topology?level=focus&ref=" + cart + "&hops=two":                                                               400,
		"/api/admin/aws/topology?level=focus&ref=" + cart + "&hops=-1":                                                                400,
		"/api/admin/aws/topology?level=focus&ref=" + cart + "&hops=1.5":                                                               400,
		"/api/admin/aws/topology?level=focus&ref=" + cart + "&hops=3":                                                                 200,
		"/api/admin/aws/topology?level=focus&ref=" + strings.Repeat("a", 2049):                                                        400,
		"/api/admin/aws/topology?level=groups&account=" + estateRead + "&group=nonsense":                                              400,
		"/api/admin/aws/topology?level=groups&account=" + estateRead + "&group=stack:":                                                400,
		"/api/admin/aws/topology?level=groups&account=" + estateRead + "&group=" + url.QueryEscape("stack:"+strings.Repeat("x", 600)): 400,
		"/api/admin/aws/topology?level=groups&account=" + estateRead + "&group=stack:shop":                                            200,
		"/api/admin/aws/topology?level=groups&account=" + estateRead + "&group=stack:other":                                           404,
		"/api/admin/aws/topology?lens=network,identity,data,changes,network":                                                          400,
		"/api/admin/aws/topology?lens=Network":                                                                                        400,
		"/api/admin/aws/topology?level=Accounts":                                                                                      400,
		// Two accounts connected: the groups of which one is a question.
		"/api/admin/aws/topology?level=groups": 400,
		// Search text.
		"/api/admin/aws/topology/search":                                                  400,
		"/api/admin/aws/topology/search?q=" + url.QueryEscape("  c  "):                    400,
		"/api/admin/aws/topology/search?q=" + strings.Repeat("c", estate.MaxSearchText+1): 400,
		"/api/admin/aws/topology/search?q=" + strings.Repeat("c", estate.MaxSearchText):   200,
		"/api/admin/aws/topology/search?q=ca":                                             200,
	} {
		if status := getJSON(t, app, path, nil); status != want {
			t.Errorf("%s = %d; want %d", shorten(path), status, want)
		}
	}
}

func shorten(path string) string {
	if len(path) > 160 {
		return path[:160] + "..."
	}
	return path
}

// countingTransport answers every AWS call with an error and counts it.
type countingTransport struct{ calls atomic.Int64 }

func (transport *countingTransport) Do(request *http.Request) (*http.Response, error) {
	transport.calls.Add(1)
	return nil, errors.New("no AWS call is expected during a page read")
}

// The mounted page routes read the stores only: Overview, every Topology
// level and lens, scoped or not, and search make no AWS call through the
// transport every AWS caller shares.
func TestEstateRoutesMakeNoAWSCall(t *testing.T) {
	transport := &countingTransport{}
	app := fiber.New(fiber.Config{Immutable: true})
	app.Use(func(ctx *fiber.Ctx) error {
		middleware.MarkAuthorized(ctx)
		middleware.SetRequestPermission(ctx, string(core.PermissionInfrastructureView), true)
		return ctx.Next()
	})
	config := &c.Config{}
	config.Connectors.AWS.Enable = true
	for _, account := range []string{estateRead, estateFailed} {
		config.Connectors.AWS.Accounts = append(config.Connectors.AWS.Accounts, c.AWSAccountConfig{AccountID: account, Regions: []string{estateRegion}})
	}
	store := storage.NewMemory()
	mounted := mountForTest(t, app, Deps{Store: store, Config: config, HTTPClient: transport,
		Scope: tenancy.NewOrgScope(fmt.Sprintf("nocall%d", time.Now().UnixNano()))})
	commitFunctions(t, mounted, estateRead, "lambda", map[string]string{"cart": "shop", "pay": "shop"},
		map[string][]model.Relation{"cart": {{Type: model.RelationRoutesTo, Target: lambdaARN(estateFailed, "edge")}}})
	commitFunctions(t, mounted, estateFailed, "lambda", map[string]string{"edge": ""}, nil)
	writeIssue(t, store, mounted.Snapshots.Scope(), estateRead, lambdaARN(estateRead, "cart"), issues.SeverityHigh, issues.TierActNow,
		issues.StateOpen, time.Now().UTC())
	before := transport.calls.Load()
	cart := url.QueryEscape(lambdaARN(estateRead, "cart"))
	for _, path := range []string{
		"/api/admin/aws/overview",
		"/api/admin/aws/overview?account=" + estateRead + "&region=" + estateRegion,
		"/api/admin/aws/topology",
		"/api/admin/aws/topology?level=accounts&lens=changes",
		"/api/admin/aws/topology?level=groups&account=" + estateRead + "&lens=network,identity,data,changes",
		"/api/admin/aws/topology?level=groups&account=" + estateRead + "&group=stack:shop",
		"/api/admin/aws/topology?level=focus&ref=" + cart + "&hops=1",
		"/api/admin/aws/topology?level=focus&ref=" + cart + "&hops=3&lens=changes",
		"/api/admin/aws/topology?level=accounts&account=" + estateFailed + "&region=" + estateRegion,
		"/api/admin/aws/topology/search?q=cart",
		"/api/admin/aws/topology/search?q=edge&account=" + estateFailed + "&region=" + estateRegion,
	} {
		if status := getJSON(t, app, path, nil); status != 200 {
			t.Fatalf("%s = %d", path, status)
		}
	}
	if calls := transport.calls.Load() - before; calls != 0 {
		t.Fatalf("page reads made %d AWS calls; want none", calls)
	}
}

// Writers racing to change the same correction, each holding the
// created_at it read, are serialized: exactly one saves and every other
// one gets 409 correction_conflict, on every store.
func TestGroupCorrectionRacingWritersOneWins(t *testing.T) {
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
			mountForTest(t, app, Deps{Store: provider(t), Config: config, Scope: tenancy.NewOrgScope(fmt.Sprintf("race%d", time.Now().UnixNano()))})
			path := "/api/admin/aws/groups/corrections"
			body := func(name, expected string) string {
				out := `{"account_id":"` + estateRead + `","kind":"rename","group_id":"stack:shop","name":"` + name + `"`
				if expected != "" {
					out += `,"expected_created_at":"` + expected + `"`
				}
				return out + "}"
			}
			status, text := postJSON(t, app, path, body("Shop", ""))
			if status != 200 {
				t.Fatalf("first save = %d %s", status, text)
			}
			var first struct {
				Correction struct {
					CreatedAt time.Time `json:"created_at"`
				} `json:"correction"`
			}
			if err := json.Unmarshal([]byte(text), &first); err != nil || first.Correction.CreatedAt.IsZero() {
				t.Fatalf("first save answered %s (%v)", text, err)
			}
			created := first.Correction.CreatedAt.Format(time.RFC3339Nano)

			const writers = 8
			statuses := make([]int, writers)
			texts := make([]string, writers)
			start := make(chan struct{})
			var wait sync.WaitGroup
			for index := range writers {
				wait.Add(1)
				go func() {
					defer wait.Done()
					<-start
					request := httptest.NewRequest("POST", path, strings.NewReader(body(fmt.Sprintf("Shop %d", index), created)))
					request.Header.Set("Content-Type", "application/json")
					res, err := app.Test(request, -1)
					if err != nil {
						texts[index] = err.Error()
						return
					}
					defer res.Body.Close()
					data, _ := io.ReadAll(res.Body)
					statuses[index], texts[index] = res.StatusCode, string(data)
				}()
			}
			close(start)
			wait.Wait()
			saved, conflicts := 0, 0
			for index, status := range statuses {
				switch {
				case status == 200:
					saved++
				case status == fiber.StatusConflict && strings.Contains(texts[index], `"correction_conflict"`):
					conflicts++
				default:
					t.Errorf("writer %d = %d %s", index, status, texts[index])
				}
			}
			if saved != 1 || conflicts != writers-1 {
				t.Fatalf("racing writers: %d saved, %d conflicts; want exactly 1 and %d", saved, conflicts, writers-1)
			}
		})
	}
}

// An expected_created_at that is not a time is a bad request, never an
// unconditional save.
func TestGroupCorrectionRefusesMalformedExpectedTime(t *testing.T) {
	app := fiber.New(fiber.Config{Immutable: true})
	app.Use(func(ctx *fiber.Ctx) error {
		middleware.MarkAuthorized(ctx)
		middleware.SetRequestPermission(ctx, string(core.PermissionInfrastructureView), true)
		middleware.SetRequestPermission(ctx, string(core.PermissionInfrastructureSettingsWrite), true)
		return ctx.Next()
	})
	config := &c.Config{}
	config.Connectors.AWS.Enable = true
	mountForTest(t, app, Deps{Store: storage.NewMemory(), Config: config, Scope: tenancy.NewOrgScope(fmt.Sprintf("badtime%d", time.Now().UnixNano()))})
	status, text := postJSON(t, app, "/api/admin/aws/groups/corrections",
		`{"account_id":"`+estateRead+`","kind":"rename","group_id":"stack:shop","name":"Shop","expected_created_at":"yesterday"}`)
	if status != fiber.StatusBadRequest {
		t.Fatalf("malformed expected_created_at = %d %s; want 400", status, text)
	}
}

// The groups view draws exactly 300 cards whole (299 groups and the
// Ungrouped card, drawn even when empty); one more is too many.
func TestTopologyGroupsDrawExactlyThreeHundred(t *testing.T) {
	app, mounted, _ := estateApp(t, storage.NewMemory(), tenancy.OrgScope{}, estateRead)
	functions := map[string]string{}
	for index := range 299 {
		functions[fmt.Sprintf("fn%03d", index)] = fmt.Sprintf("stack%03d", index)
	}
	commitFunctions(t, mounted, estateRead, "lambda", functions, nil)
	var topology topologyAnswer
	if status := getJSON(t, app, "/api/admin/aws/topology?level=groups", &topology); status != 200 {
		t.Fatalf("groups = %d", status)
	}
	if topology.TooMany || len(topology.Nodes) != 300 || topology.NodeCount != 300 || topology.Hint != nil {
		t.Fatalf("300 groups: too many %v, %d nodes, count %d, hint %+v; want all 300 drawn", topology.TooMany, len(topology.Nodes),
			topology.NodeCount, topology.Hint)
	}
	commitFunctions(t, mounted, estateRead, "lambda-more", map[string]string{"fn299": "stack299"}, nil)
	topology = topologyAnswer{}
	getJSON(t, app, "/api/admin/aws/topology?level=groups", &topology)
	if !topology.TooMany || len(topology.Nodes) != 0 || topology.NodeCount != 301 {
		t.Fatalf("301 cards: too many %v, %d nodes, count %d; want too many, empty, 301", topology.TooMany, len(topology.Nodes), topology.NodeCount)
	}
}
