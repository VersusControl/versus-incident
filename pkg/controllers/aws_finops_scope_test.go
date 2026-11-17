package controllers

import (
	"encoding/json"
	"net/http"
	"sort"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/finops"
)

// TestFinOpsRoutesTakeCommaSeparatedScope: accounts, regions and teams are
// comma-separated lists; repeated singular keys are accepted too.
func TestFinOpsRoutesTakeCommaSeparatedScope(t *testing.T) {
	app, _ := finopsApp(t, finopsBackend(t), finopsViewer, false)
	keys := func(path string) []string {
		var v finops.BreakdownView
		_, body := awsTestRequest(t, app, http.MethodGet, "/api/admin/aws/finops/breakdown?period=2026-10&"+path, "")
		if err := json.Unmarshal(body, &v); err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, r := range v.Rows {
			out = append(out, r.Key)
		}
		sort.Strings(out)
		return out
	}
	both := keys("group_by=account&accounts=111111111111,222222222222")
	if len(both) != 2 || both[0] != "111111111111" || both[1] != "222222222222" {
		t.Fatalf("two accounts: %v", both)
	}
	if one := keys("group_by=account&accounts=222222222222"); len(one) != 1 || one[0] != "222222222222" {
		t.Fatalf("one account: %v", one)
	}
	if regions := keys("group_by=region&regions=us-east-1,global"); len(regions) != 2 {
		t.Fatalf("two regions: %v", regions)
	}
	if teams := keys("group_by=team&teams=checkout,platform"); len(teams) != 2 || teams[0] != "checkout" || teams[1] != "platform" {
		t.Fatalf("two teams: %v", teams)
	}
	var sum finops.SummaryView
	_, body := awsTestRequest(t, app, http.MethodGet, "/api/admin/aws/finops/summary?period=2026-10&accounts=222222222222", "")
	if err := json.Unmarshal(body, &sum); err != nil || len(sum.ScopeOptions.Accounts) != 3 {
		t.Fatalf("scope options keep every account under a filter: %+v", sum.ScopeOptions)
	}
}
