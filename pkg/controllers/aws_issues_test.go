package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/VersusControl/versus-incident/pkg/aws/issues"
	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/aws/snapshot"
	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/middleware"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

const issuesAccount = "123400000007"

var issuesNow = time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)

var (
	issuesViewer = map[string]bool{string(core.PermissionInfrastructureView): true, string(core.PermissionInfrastructureIssuesWrite): false}
	issuesWriter = map[string]bool{string(core.PermissionInfrastructureView): true, string(core.PermissionInfrastructureIssuesWrite): true}
)

// seedIssues stores issues in the given states, one public bucket each,
// and returns their IDs in order.
func seedIssues(t *testing.T, provider storage.Provider, states ...issues.State) []string {
	t.Helper()
	store, _ := storage.AWSIssuesOf(provider)
	var ids []string
	for index, state := range states {
		owner := fmt.Sprintf("arn:aws:s3:::bucket-%d", index)
		issue := issues.Issue{ID: issues.IssueID("public-bucket", owner), Rule: "public-bucket", Kind: issues.KindSecurity,
			Severity: issues.SeverityHigh, Tier: issues.TierActNow, Account: issuesAccount, Region: "eu-west-1", Owner: owner,
			Title: fmt.Sprintf("Bucket %d is public", index), State: state, Confirm: issues.Confirmed,
			FirstSeen: issuesNow, OpenedAt: issuesNow, Version: 1, UpdatedAt: issuesNow}
		if state == issues.StateRejected {
			issue.Rejection = &issues.Rejection{Reason: "public on purpose", By: "bob", At: issuesNow, SeverityAtReject: issues.SeverityHigh}
		}
		record, err := issues.Record(issue)
		if err != nil {
			t.Fatal(err)
		}
		write := storage.AWSIssueWrite{Issue: record, Events: []storage.AWSIssueEvent{{At: issuesNow, Kind: storage.AWSIssueEventOpened, Actor: issues.EvaluatorActor}}}
		if err := store.WriteAWSIssues(context.Background(), tenancy.DefaultOrgScope(), storage.AWSIssueBatch{AccountID: issuesAccount, Writes: []storage.AWSIssueWrite{write}}); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, issue.ID)
	}
	return ids
}

func issuesApp(t *testing.T, permissions map[string]bool, provider storage.Provider) *fiber.App {
	t.Helper()
	actions, err := issues.NewActions(provider, tenancy.DefaultOrgScope(), func() time.Time { return issuesNow })
	if err != nil {
		t.Fatal(err)
	}
	app := fiber.New(fiber.Config{Immutable: true})
	app.Use(middleware.OrgInjector())
	app.Use(func(ctx *fiber.Ctx) error {
		middleware.MarkAuthorized(ctx)
		middleware.SetRequestActor(ctx, "alice@example.com")
		for permission, allowed := range permissions {
			middleware.SetRequestPermission(ctx, permission, allowed)
		}
		return ctx.Next()
	})
	coverage := func(context.Context, string) (issues.CoverageRead, bool, error) {
		return issues.CoverageRead{AccountsRead: 1, AccountsTotal: 2, NotRead: []snapshot.CoverageRow{
			{Source: "guardduty", Outcome: model.OutcomeNotRead, Reason: model.GapDenied, Accounts: []string{issuesAccount}}}}, true, nil
	}
	notAssessed := func(context.Context, []string) ([]issues.AccountNotAssessed, error) {
		return []issues.AccountNotAssessed{{Account: issuesAccount, NotAssessed: []issues.NotAssessed{{Rule: "detection-off", Missing: []string{"guardduty not read"}}}}}, nil
	}
	NewAWSIssuesController(actions, coverage, notAssessed, func() time.Time { return issuesNow }).Register(app.Group("/api"))
	return app
}

func decodeBody[T any](t *testing.T, body []byte) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("body %s: %v", body, err)
	}
	return out
}

// Reads and the report need the infrastructure view; acknowledge,
// reject, bulk reject and reopen need the issues write permission too.
// Each refusal is the 403 permission_required, and a refused write
// changes nothing.
func TestIssueRoutesNeedTheirPermissions(t *testing.T) {
	provider := storage.NewMemory()
	ids := seedIssues(t, provider, issues.StateOpen, issues.StateRejected)
	noView := issuesApp(t, map[string]bool{string(core.PermissionInfrastructureView): false, string(core.PermissionInfrastructureIssuesWrite): true}, provider)
	for _, path := range []string{"/api/admin/aws/issues", "/api/admin/aws/issues/report", "/api/admin/aws/issues/" + ids[0]} {
		res, body := awsTestRequest(t, noView, http.MethodGet, path, "")
		refusedFor(t, res, body, core.PermissionInfrastructureView)
	}
	viewer := issuesApp(t, issuesViewer, provider)
	for _, request := range []struct{ path, body string }{
		{"/api/admin/aws/issues/" + ids[0] + "/acknowledge", ""},
		{"/api/admin/aws/issues/" + ids[0] + "/reject", `{"reason":"public on purpose"}`},
		{"/api/admin/aws/issues/" + ids[1] + "/reopen", ""},
		{"/api/admin/aws/issues/reject", `{"ids":["` + ids[0] + `"],"reason":"public on purpose"}`},
	} {
		res, body := awsTestRequest(t, viewer, http.MethodPost, request.path, request.body)
		refusedFor(t, res, body, core.PermissionInfrastructureIssuesWrite)
	}
	res, body := awsTestRequest(t, viewer, http.MethodGet, "/api/admin/aws/issues/"+ids[0], "")
	if detail := decodeBody[awsIssueDetail](t, body); res.StatusCode != http.StatusOK || detail.Issue.State != issues.StateOpen || detail.Issue.Version != 1 {
		t.Fatalf("after refused writes = %d %s", res.StatusCode, body)
	}
}

// The open-source gateway caller holds the issues write permission.
func TestGatewayCallerMayWriteIssues(t *testing.T) {
	middleware.SetGatewayAuthEnabled(true)
	t.Cleanup(func() { middleware.SetGatewayAuthEnabled(true) })
	const secret = "aws-issues-gateway-secret"
	loadGatewayConfig(t, secret)
	previous := config.GetConfig().GatewaySecret
	config.GetConfig().GatewaySecret = secret
	t.Cleanup(func() { config.GetConfig().GatewaySecret = previous })
	provider := storage.NewMemory()
	ids := seedIssues(t, provider, issues.StateOpen)
	actions, _ := issues.NewActions(provider, tenancy.DefaultOrgScope(), func() time.Time { return issuesNow })
	app := fiber.New()
	app.Use(middleware.OrgInjector())
	NewAWSIssuesController(actions, nil, nil, nil).Register(app.Group("/api"))
	request := httptest.NewRequest(http.MethodPost, "/api/admin/aws/issues/"+ids[0]+"/acknowledge", nil)
	request.Header.Set("X-Gateway-Secret", secret)
	response, err := app.Test(request, -1)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("gateway acknowledge = %d", response.StatusCode)
	}
}

// A reason or review date that is not valid is a 400 that names it; an
// unknown issue is a 404; an issue in the wrong state is a 409.
func TestIssueWriteErrors(t *testing.T) {
	provider := storage.NewMemory()
	ids := seedIssues(t, provider, issues.StateOpen, issues.StateRejected, issues.StateResolved)
	app := issuesApp(t, issuesWriter, provider)
	unknown := strings.Repeat("0", 64)
	for _, tc := range []struct {
		name, path, body string
		status           int
		code             string
	}{
		{"short reason", "/" + ids[0] + "/reject", `{"reason":"ok"}`, 400, "invalid_reason"},
		{"long reason", "/" + ids[0] + "/reject", `{"reason":"` + strings.Repeat("x", 501) + `"}`, 400, "invalid_reason"},
		{"control character", "/" + ids[0] + "/reject", `{"reason":"public\u0007 bucket"}`, 400, "invalid_reason"},
		{"review date in the past", "/" + ids[0] + "/reject", `{"reason":"public on purpose","review_on":"2026-01-01"}`, 400, "invalid_review_date"},
		{"review date not a day", "/" + ids[0] + "/reject", `{"reason":"public on purpose","review_on":"next week"}`, 400, "invalid_review_date"},
		{"unknown field", "/" + ids[0] + "/reject", `{"reason":"public on purpose","expires":"2027-01-01"}`, 400, "invalid_body"},
		{"bulk reason", "/reject", `{"ids":["` + ids[0] + `"],"reason":"no"}`, 400, "invalid_reason"},
		{"bulk without ids", "/reject", `{"ids":[],"reason":"public on purpose"}`, 400, "invalid_body"},
		{"bulk over 200", "/reject", `{"ids":[` + strings.TrimSuffix(strings.Repeat(`"`+unknown+`",`, 201), ",") + `],"reason":"public on purpose"}`, 400, "too_many"},
		{"unknown issue", "/" + unknown + "/acknowledge", "", 404, "not_found"},
		{"malformed issue", "/not-an-id/acknowledge", "", 404, "not_found"},
		{"already rejected", "/" + ids[1] + "/reject", `{"reason":"public on purpose"}`, 409, "already_rejected"},
		{"resolved", "/" + ids[2] + "/acknowledge", "", 409, "not_open"},
		{"reopen an open issue", "/" + ids[0] + "/reopen", "", 409, "not_rejected"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, body := awsTestRequest(t, app, http.MethodPost, "/api/admin/aws/issues"+tc.path, tc.body)
			if got := decodeBody[map[string]string](t, body); res.StatusCode != tc.status || got["code"] != tc.code {
				t.Fatalf("= %d %s; want %d %s", res.StatusCode, body, tc.status, tc.code)
			}
		})
	}
	for _, state := range []string{"resolved", "deleted"} {
		res, body := awsTestRequest(t, app, http.MethodGet, "/api/admin/aws/issues/report?state="+state, "")
		if got := decodeBody[map[string]string](t, body); res.StatusCode != 400 || got["code"] != "invalid_filter" {
			t.Fatalf("report with state %s = %d %s; want 400 invalid_filter", state, res.StatusCode, body)
		}
	}
	res, body := awsTestRequest(t, app, http.MethodGet, "/api/admin/aws/issues?state=deleted", "")
	if got := decodeBody[map[string]string](t, body); res.StatusCode != 400 || got["code"] != "invalid_filter" {
		t.Fatalf("unknown state = %d %s", res.StatusCode, body)
	}
}

// Acknowledge, reject (reason stored as plain text, redacted) and reopen
// answer the issue as written, and the detail lists their events.
func TestIssueWritesAndDetail(t *testing.T) {
	provider := storage.NewMemory()
	ids := seedIssues(t, provider, issues.StateOpen)
	app := issuesApp(t, issuesWriter, provider)
	path := "/api/admin/aws/issues/" + ids[0]
	if res, body := awsTestRequest(t, app, http.MethodPost, path+"/acknowledge", ""); res.StatusCode != 200 ||
		decodeBody[map[string]issues.Issue](t, body)["issue"].State != issues.StateAcknowledged {
		t.Fatalf("acknowledge = %d %s", res.StatusCode, body)
	}
	res, body := awsTestRequest(t, app, http.MethodPost, path+"/reject",
		`{"reason":"<b>docs</b> key AKIAIOSFODNN7EXAMPLE is public on purpose","review_on":"2027-03-01"}`)
	rejected := decodeBody[map[string]issues.Issue](t, body)["issue"]
	if res.StatusCode != 200 || rejected.State != issues.StateRejected || rejected.Rejection.ReviewOn != "2027-03-01" ||
		rejected.Rejection.By != "alice@example.com" || strings.Contains(rejected.Rejection.Reason, "AKIAIOSFODNN7EXAMPLE") ||
		!strings.Contains(rejected.Rejection.Reason, "<b>docs</b>") {
		t.Fatalf("reject = %d %s", res.StatusCode, body)
	}
	if res, body := awsTestRequest(t, app, http.MethodPost, path+"/reopen", `{"reason":"no longer on purpose"}`); res.StatusCode != 200 ||
		decodeBody[map[string]issues.Issue](t, body)["issue"].State != issues.StateOpen {
		t.Fatalf("reopen = %d %s", res.StatusCode, body)
	}
	res, body = awsTestRequest(t, app, http.MethodGet, path, "")
	detail := decodeBody[awsIssueDetail](t, body)
	var kinds []string
	for _, event := range detail.Events {
		kinds = append(kinds, string(event.Kind))
	}
	if res.StatusCode != 200 || strings.Join(kinds, ",") != "opened,acknowledged,rejected,reopened" ||
		detail.Impact.Owner != detail.Issue.Owner || detail.Impact.Account != issuesAccount {
		t.Fatalf("detail = %d %s", res.StatusCode, body)
	}
}

// A bulk reject answers 200 with a result per ID: rejected,
// already_rejected, not_found or conflict. One bad ID never undoes the
// others, and a repeat changes nothing.
func TestBulkRejectAnswersPerID(t *testing.T) {
	provider := storage.NewMemory()
	ids := seedIssues(t, provider, issues.StateOpen, issues.StateRegressed, issues.StateRejected, issues.StateResolved)
	app := issuesApp(t, issuesWriter, provider)
	unknown := strings.Repeat("a", 64)
	request := `{"ids":["` + strings.Join(append(append([]string{}, ids...), unknown), `","`) + `"],"reason":"accepted risk for the demo account"}`
	want := []string{issues.BulkRejected, issues.BulkRejected, issues.BulkAlreadyRejected, issues.BulkConflict, issues.BulkNotFound}
	for round, expect := range [][]string{want, {issues.BulkAlreadyRejected, issues.BulkAlreadyRejected, issues.BulkAlreadyRejected, issues.BulkConflict, issues.BulkNotFound}} {
		res, body := awsTestRequest(t, app, http.MethodPost, "/api/admin/aws/issues/reject", request)
		results := decodeBody[awsIssueBulkRejectResponse](t, body).Results
		if res.StatusCode != 200 || len(results) != len(expect) {
			t.Fatalf("round %d = %d %s", round, res.StatusCode, body)
		}
		for index, result := range results {
			if result.Result != expect[index] {
				t.Fatalf("round %d: %s = %s; want %s", round, result.ID, result.Result, expect[index])
			}
		}
	}
	store, _ := storage.AWSIssuesOf(provider)
	events, err := store.AWSIssueEvents(context.Background(), tenancy.DefaultOrgScope(), ids[0])
	if err != nil || len(events) != 2 {
		t.Fatalf("events after a repeated bulk reject = %d, %v; want opened and one rejected", len(events), err)
	}
}

// The list answers the summary, the warning line, the listed issues, the
// rejected count and where rules are not assessed; state=rejected lists
// the Rejected tab with who, why and when. The report carries the same
// counts.
func TestIssueListAndReport(t *testing.T) {
	provider := storage.NewMemory()
	seedIssues(t, provider, issues.StateOpen, issues.StateAcknowledged, issues.StateRejected, issues.StateResolved)
	app := issuesApp(t, issuesViewer, provider)
	res, body := awsTestRequest(t, app, http.MethodGet, "/api/admin/aws/issues?kind=security&account="+issuesAccount, "")
	page := decodeBody[awsIssuesPage](t, body)
	if res.StatusCode != 200 || !page.Enabled || page.Summary.Open != 2 || len(page.Items) != 2 || page.RejectedCount != 1 ||
		page.Summary.AccountsRead != 1 || page.Summary.AccountsTotal != 2 || page.Warning == nil || page.Warning.NotRead != 1 ||
		len(page.NotAssessed) != 1 {
		t.Fatalf("list = %d %s", res.StatusCode, body)
	}
	res, body = awsTestRequest(t, app, http.MethodGet, "/api/admin/aws/issues?state=rejected", "")
	tab := decodeBody[awsIssuesPage](t, body)
	if res.StatusCode != 200 || len(tab.Items) != 1 || tab.Items[0].Rejection == nil || tab.Items[0].Rejection.Reason != "public on purpose" ||
		tab.Items[0].Rejection.By != "bob" || tab.Summary != page.Summary {
		t.Fatalf("rejected tab = %d %s", res.StatusCode, body)
	}
	res, body = awsTestRequest(t, app, http.MethodGet, "/api/admin/aws/issues/report?kind=security", "")
	report := decodeBody[issues.IssueReport](t, body)
	if res.StatusCode != 200 || len(report.Sections.ActNow) != 2 || len(report.Sections.Rejected) != 1 || report.Summary != page.Summary ||
		report.Warning == nil || len(report.NotRead) != 1 || report.Filters.Kind != "security" || !report.AsOf.Equal(issuesNow) || report.Capped {
		t.Fatalf("report = %d %s", res.StatusCode, body)
	}
}

// Without the issue tables the reads answer enabled=false and the writes
// are not ready.
func TestIssueRoutesWithoutTheStore(t *testing.T) {
	app := fiber.New()
	app.Use(func(ctx *fiber.Ctx) error {
		middleware.MarkAuthorized(ctx)
		for permission, allowed := range issuesWriter {
			middleware.SetRequestPermission(ctx, permission, allowed)
		}
		return ctx.Next()
	})
	NewAWSIssuesController(nil, nil, nil, nil).Register(app.Group("/api"))
	if res, body := awsTestRequest(t, app, http.MethodGet, "/api/admin/aws/issues", ""); res.StatusCode != 200 || !strings.Contains(string(body), `"enabled":false`) {
		t.Fatalf("list = %d %s", res.StatusCode, body)
	}
	if res, _ := awsTestRequest(t, app, http.MethodPost, "/api/admin/aws/issues/reject", `{"ids":["x"],"reason":"public on purpose"}`); res.StatusCode != 503 {
		t.Fatalf("bulk reject = %d", res.StatusCode)
	}
}
