package mount

import (
	"fmt"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/issues"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

// issueReadAnswer is what the Issues page and its report say.
type issueReadAnswer struct {
	Summary       issues.Summary `json:"summary"`
	RejectedCount int            `json:"rejected_count"`
	Rows          int            `json:"rows"`
	Items         []struct {
		Account string `json:"account"`
	} `json:"items"`
}

// After an account is removed from settings, the Issues page, its report,
// Overview and the Topology account cards all leave its issues out and
// agree, on every store (Postgres reads the stored issues through its SQL
// account filter), with and without a licence (where the removed account's
// rows were stored under the default organization); with no account left
// connected every reader counts zero.
func TestIssueReadersAgreeAfterAnAccountIsRemoved(t *testing.T) {
	for name, provider := range estateProviders(t) {
		for _, licensed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/licensed=%v", name, licensed), func(t *testing.T) {
				store := provider(t)
				scope := tenancy.NewOrgScope(fmt.Sprintf("removal%d", time.Now().UnixNano()))
				if licensed {
					scope = tenancy.NewOrgScope(fmt.Sprintf("removal%d", time.Now().UnixNano()), tenancy.DefaultOrgID)
				}
				app, mounted, config := estateApp(t, store, scope, estateRead, estateFailed)
				commitFunctions(t, mounted, estateRead, "lambda", map[string]string{"cart": "shop"}, nil)
				commitFunctions(t, mounted, estateFailed, "lambda", map[string]string{"edge": ""}, nil)
				removedScope := mounted.Snapshots.Scope()
				if licensed {
					// Postgres keeps every test's rows in one database; under a
					// licence the removed account's rows sit in the shared
					// default organization, so its account ID is unique here.
					removedScope = tenancy.DefaultOrgScope()
				}
				now := time.Now().UTC()
				writeIssue(t, store, mounted.Snapshots.Scope(), estateRead, lambdaARN(estateRead, "cart"), issues.SeverityHigh, issues.TierActNow,
					issues.StateOpen, now)
				removed := estateFailed
				if licensed {
					removed = fmt.Sprintf("8%011d", time.Now().UnixNano()%100000000000)
					config.Connectors.AWS.Accounts[1].AccountID = removed
					commitFunctions(t, mounted, removed, "lambda", map[string]string{"edge": ""}, nil)
				}
				writeIssue(t, store, removedScope, removed, lambdaARN(removed, "edge"), issues.SeverityCritical, issues.TierActNow, issues.StateOpen, now)
				writeIssue(t, store, removedScope, removed, lambdaARN(removed, "edge2"), issues.SeverityLow, issues.TierWorthALook, issues.StateOpen, now)
				writeIssue(t, store, removedScope, removed, lambdaARN(removed, "edge3"), issues.SeverityLow, issues.TierActNow, issues.StateRejected, now)

				check := func(stage string, act, worth, rejected int) {
					t.Helper()
					var page, report issueReadAnswer
					if status := getJSON(t, app, "/api/admin/aws/issues", &page); status != 200 {
						t.Fatalf("%s: issues = %d", stage, status)
					}
					if status := getJSON(t, app, "/api/admin/aws/issues/report", &report); status != 200 {
						t.Fatalf("%s: report = %d", stage, status)
					}
					var overview overviewAnswer
					getJSON(t, app, "/api/admin/aws/overview", &overview)
					connected := map[string]bool{}
					for _, account := range config.Connectors.AWS.Accounts {
						connected[account.AccountID] = true
					}
					cardAct, cardWorth := 0, 0
					if len(connected) > 0 {
						cardAct, cardWorth = parityCards(t, app, "", connected)
					}
					got := fmt.Sprint(page.Summary.ActNow(), page.Summary.WorthALook, report.Summary.ActNow(), report.Summary.WorthALook,
						deref(overview.Summary.ActNow), deref(overview.Summary.WorthALook), cardAct, cardWorth)
					want := fmt.Sprint(act, worth, act, worth, act, worth, act, worth)
					if got != want {
						t.Errorf("%s: issues act/look, report act/look, overview act/look, cards act/look = %s; want %s", stage, got, want)
					}
					if page.RejectedCount != rejected || report.Summary.RejectedCount != rejected {
						t.Errorf("%s: rejected page %d report %d; want %d", stage, page.RejectedCount, report.Summary.RejectedCount, rejected)
					}
					if report.Rows != act+worth+rejected {
						t.Errorf("%s: report rows %d; want %d", stage, report.Rows, act+worth+rejected)
					}
					for _, item := range page.Items {
						if !connected[item.Account] {
							t.Errorf("%s: the Issues page lists an issue of %s, not connected", stage, item.Account)
						}
					}
				}
				check("both connected", 2, 1, 1)
				config.Connectors.AWS.Accounts = config.Connectors.AWS.Accounts[:1]
				check("after the removal", 1, 0, 0)
				config.Connectors.AWS.Accounts = nil
				check("no account connected", 0, 0, 0)
			})
		}
	}
}
