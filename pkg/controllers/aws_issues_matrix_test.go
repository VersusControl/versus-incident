package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/VersusControl/versus-incident/pkg/aws/issues"
	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/middleware"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

const otherIssuesAccount = "123400000008"

// issueSeed is one stored issue for the matrix tests.
type issueSeed struct {
	owner    string
	account  string
	kind     issues.Kind
	severity issues.Severity
	state    issues.State
	group    string
	reason   string
}

// seedIssueSet stores the issues and returns their IDs in order.
func seedIssueSet(t *testing.T, provider storage.Provider, seeds []issueSeed) []string {
	t.Helper()
	store, _ := storage.AWSIssuesOf(provider)
	ids := make([]string, 0, len(seeds))
	for _, seed := range seeds {
		account := seed.account
		if account == "" {
			account = issuesAccount
		}
		issue := issues.Issue{ID: issues.IssueID("public-bucket", seed.owner), Rule: "public-bucket", Kind: seed.kind,
			Severity: seed.severity, Tier: issues.TierFor(seed.severity), Account: account, Region: "eu-west-1", Owner: seed.owner,
			GroupID: seed.group, Title: "Bucket " + seed.owner + " is public", State: seed.state, Confirm: issues.Confirmed,
			FirstSeen: issuesNow, OpenedAt: issuesNow, Version: 1, UpdatedAt: issuesNow}
		if seed.state == issues.StateResolved {
			issue.ResolvedAt = issuesNow
		}
		if seed.state == issues.StateRejected {
			reason := seed.reason
			if reason == "" {
				reason = "public on purpose"
			}
			issue.Rejection = &issues.Rejection{Reason: reason, By: "bob", At: issuesNow, SeverityAtReject: seed.severity}
		}
		record, err := issues.Record(issue)
		if err != nil {
			t.Fatal(err)
		}
		write := storage.AWSIssueWrite{Issue: record, Events: []storage.AWSIssueEvent{{At: issuesNow, Kind: storage.AWSIssueEventOpened, Actor: issues.EvaluatorActor}}}
		if err := store.WriteAWSIssues(context.Background(), tenancy.DefaultOrgScope(), storage.AWSIssueBatch{AccountID: account, Writes: []storage.AWSIssueWrite{write}}); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, issue.ID)
	}
	return ids
}

// issueRoutes are every issue route, with a body a writer would send.
func issueRoutes(open, rejected string) []struct{ method, path, body string } {
	return []struct{ method, path, body string }{
		{http.MethodGet, "/api/admin/aws/issues", ""},
		{http.MethodGet, "/api/admin/aws/issues?state=rejected", ""},
		{http.MethodGet, "/api/admin/aws/issues/report", ""},
		{http.MethodGet, "/api/admin/aws/issues/" + open, ""},
		{http.MethodPost, "/api/admin/aws/issues/" + open + "/acknowledge", ""},
		{http.MethodPost, "/api/admin/aws/issues/" + open + "/reject", `{"reason":"public on purpose"}`},
		{http.MethodPost, "/api/admin/aws/issues/" + rejected + "/reopen", ""},
		{http.MethodPost, "/api/admin/aws/issues/reject", `{"ids":["` + open + `"],"reason":"public on purpose"}`},
	}
}

// issueEventCount is how many events the store holds for the issues.
func issueEventCount(t *testing.T, provider storage.Provider, ids ...string) int {
	t.Helper()
	store, _ := storage.AWSIssuesOf(provider)
	total := 0
	for _, id := range ids {
		events, err := store.AWSIssueEvents(context.Background(), tenancy.DefaultOrgScope(), id)
		if err != nil {
			t.Fatal(err)
		}
		total += len(events)
	}
	return total
}

// A caller with no credential gets 401 on every issue route, read or
// write, and nothing is written.
func TestIssueRoutesWithoutAuthAnswer401(t *testing.T) {
	middleware.SetGatewayAuthEnabled(true)
	t.Cleanup(func() { middleware.SetGatewayAuthEnabled(true) })
	const secret = "aws-issues-matrix-secret"
	loadGatewayConfig(t, secret)
	previous := config.GetConfig().GatewaySecret
	config.GetConfig().GatewaySecret = secret
	t.Cleanup(func() { config.GetConfig().GatewaySecret = previous })

	provider := storage.NewMemory()
	ids := seedIssueSet(t, provider, []issueSeed{
		{owner: "arn:aws:s3:::open", kind: issues.KindSecurity, severity: issues.SeverityHigh, state: issues.StateOpen},
		{owner: "arn:aws:s3:::rejected", kind: issues.KindSecurity, severity: issues.SeverityHigh, state: issues.StateRejected},
	})
	actions, _ := issues.NewActions(provider, tenancy.DefaultOrgScope(), func() time.Time { return issuesNow })
	app := fiber.New()
	app.Use(middleware.OrgInjector())
	NewAWSIssuesController(actions, nil, nil, nil).Register(app.Group("/api"))
	for _, route := range issueRoutes(ids[0], ids[1]) {
		res, body := awsTestRequest(t, app, route.method, route.path, route.body)
		if res.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s without a credential = %d %s; want 401", route.method, route.path, res.StatusCode, body)
		}
	}
	// A wrong gateway secret is no credential either.
	for _, route := range issueRoutes(ids[0], ids[1]) {
		res, body := awsTestRequestWithHeader(t, app, route.method, route.path, route.body, "X-Gateway-Secret", "not-the-secret")
		if res.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s with a wrong secret = %d %s; want 401", route.method, route.path, res.StatusCode, body)
		}
	}
	if events := issueEventCount(t, provider, ids...); events != 2 {
		t.Fatalf("unauthenticated calls wrote events: %d; want the 2 seeded", events)
	}
}

// awsTestRequestWithHeader is awsTestRequest with one extra header.
func awsTestRequestWithHeader(t *testing.T, app *fiber.App, method, path, body, header, value string) (*http.Response, []byte) {
	t.Helper()
	wrapped := fiber.New()
	wrapped.Use(func(ctx *fiber.Ctx) error {
		ctx.Request().Header.Set(header, value)
		return ctx.Next()
	})
	wrapped.Mount("/", app)
	return awsTestRequest(t, wrapped, method, path, body)
}

// A view-only caller reads the list, the Rejected tab, the report and the
// detail, and gets 403 permission_required on every write, which writes
// nothing. A caller with the write permission may use every write.
func TestIssueViewOnlyAndWriterMatrix(t *testing.T) {
	provider := storage.NewMemory()
	ids := seedIssueSet(t, provider, []issueSeed{
		{owner: "arn:aws:s3:::open", kind: issues.KindSecurity, severity: issues.SeverityHigh, state: issues.StateOpen},
		{owner: "arn:aws:s3:::rejected", kind: issues.KindSecurity, severity: issues.SeverityHigh, state: issues.StateRejected},
	})
	viewer := issuesApp(t, issuesViewer, provider)
	for _, route := range issueRoutes(ids[0], ids[1]) {
		res, body := awsTestRequest(t, viewer, route.method, route.path, route.body)
		if route.method == http.MethodGet {
			if res.StatusCode != http.StatusOK {
				t.Errorf("view-only GET %s = %d %s", route.path, res.StatusCode, body)
			}
			continue
		}
		refusedFor(t, res, body, core.PermissionInfrastructureIssuesWrite)
	}
	if events := issueEventCount(t, provider, ids...); events != 2 {
		t.Fatalf("refused writes wrote events: %d; want the 2 seeded", events)
	}

	// No explicit decision on the write permission is a refusal too.
	viewOnly := issuesApp(t, map[string]bool{string(core.PermissionInfrastructureView): true}, provider)
	for _, route := range issueRoutes(ids[0], ids[1]) {
		if route.method == http.MethodPost {
			res, body := awsTestRequest(t, viewOnly, route.method, route.path, route.body)
			refusedFor(t, res, body, core.PermissionInfrastructureIssuesWrite)
		}
	}

	writer := issuesApp(t, issuesWriter, provider)
	for _, route := range []struct{ path, body, state string }{
		{"/api/admin/aws/issues/" + ids[0] + "/acknowledge", "", "acknowledged"},
		{"/api/admin/aws/issues/" + ids[0] + "/reject", `{"reason":"public on purpose"}`, "rejected"},
		{"/api/admin/aws/issues/" + ids[0] + "/reopen", "", "open"},
		{"/api/admin/aws/issues/" + ids[1] + "/reopen", "", "open"},
	} {
		res, body := awsTestRequest(t, writer, http.MethodPost, route.path, route.body)
		if res.StatusCode != http.StatusOK || decodeBody[map[string]issues.Issue](t, body)["issue"].State != issues.State(route.state) {
			t.Fatalf("writer POST %s = %d %s", route.path, res.StatusCode, body)
		}
	}
	res, body := awsTestRequest(t, writer, http.MethodPost, "/api/admin/aws/issues/reject", `{"ids":["`+ids[0]+`","`+ids[1]+`"],"reason":"public on purpose"}`)
	results := decodeBody[awsIssueBulkRejectResponse](t, body).Results
	if res.StatusCode != http.StatusOK || len(results) != 2 || results[0].Result != issues.BulkRejected || results[1].Result != issues.BulkRejected {
		t.Fatalf("writer bulk reject = %d %s", res.StatusCode, body)
	}
}

// noActorApp authorizes a caller with every permission but no user name.
func noActorApp(t *testing.T, provider storage.Provider) *fiber.App {
	t.Helper()
	actions, err := issues.NewActions(provider, tenancy.DefaultOrgScope(), func() time.Time { return issuesNow })
	if err != nil {
		t.Fatal(err)
	}
	app := fiber.New(fiber.Config{Immutable: true})
	app.Use(middleware.OrgInjector())
	app.Use(func(ctx *fiber.Ctx) error {
		middleware.MarkAuthorized(ctx)
		for permission, allowed := range issuesWriter {
			middleware.SetRequestPermission(ctx, permission, allowed)
		}
		return ctx.Next()
	})
	NewAWSIssuesController(actions, nil, nil, func() time.Time { return issuesNow }).Register(app.Group("/api"))
	return app
}

// conflictingIssues is an issue store whose every write finds the issue
// changed, as when an evaluation keeps writing it first.
type conflictingIssues struct {
	storage.Provider
}

func (store conflictingIssues) inner() storage.AWSIssueStore {
	inner, _ := storage.AWSIssuesOf(store.Provider)
	return inner
}

func (store conflictingIssues) AWSIssues(ctx context.Context, scope tenancy.OrgScope, query storage.AWSIssueQuery) ([]storage.AWSIssueRecord, error) {
	return store.inner().AWSIssues(ctx, scope, query)
}

func (store conflictingIssues) AWSIssueCounts(ctx context.Context, scope tenancy.OrgScope, query storage.AWSIssueQuery) ([]storage.AWSIssueCount, error) {
	return store.inner().AWSIssueCounts(ctx, scope, query)
}

func (store conflictingIssues) AWSIssueEvents(ctx context.Context, scope tenancy.OrgScope, id string) ([]storage.AWSIssueEvent, error) {
	return store.inner().AWSIssueEvents(ctx, scope, id)
}

func (store conflictingIssues) AWSIssueBaselines(ctx context.Context, scope tenancy.OrgScope, account string) ([]storage.AWSIssueBaseline, error) {
	return store.inner().AWSIssueBaselines(ctx, scope, account)
}

func (store conflictingIssues) WriteAWSIssues(context.Context, tenancy.OrgScope, storage.AWSIssueBatch) error {
	return storage.ErrAWSIssueConflict
}

func (store conflictingIssues) PruneAWSIssues(ctx context.Context, scope tenancy.OrgScope, before time.Time) (int64, error) {
	return store.inner().PruneAWSIssues(ctx, scope, before)
}

// Each 400 and 409 code the API documents, from the request that causes
// it, and the bulk reject edges: none, 200, 201 and duplicate IDs.
func TestIssueAPIValidationCodes(t *testing.T) {
	provider := storage.NewMemory()
	ids := seedIssueSet(t, provider, []issueSeed{
		{owner: "arn:aws:s3:::open", kind: issues.KindSecurity, severity: issues.SeverityHigh, state: issues.StateOpen},
		{owner: "arn:aws:s3:::rejected", kind: issues.KindSecurity, severity: issues.SeverityHigh, state: issues.StateRejected},
		{owner: "arn:aws:s3:::resolved", kind: issues.KindSecurity, severity: issues.SeverityHigh, state: issues.StateResolved},
		{owner: "arn:aws:s3:::acked", kind: issues.KindSecurity, severity: issues.SeverityHigh, state: issues.StateAcknowledged},
	})
	writer := issuesApp(t, issuesWriter, provider)
	type call struct {
		name, method, path, body string
		status                   int
		code                     string
	}
	base := "/api/admin/aws/issues"
	for _, tc := range []call{
		{"reason blank", http.MethodPost, base + "/" + ids[0] + "/reject", `{"reason":"   "}`, 400, "invalid_reason"},
		{"reason missing", http.MethodPost, base + "/" + ids[0] + "/reject", `{}`, 400, "invalid_reason"},
		{"reason 2 characters", http.MethodPost, base + "/" + ids[0] + "/reject", `{"reason":"ab"}`, 400, "invalid_reason"},
		{"review date today", http.MethodPost, base + "/" + ids[0] + "/reject", `{"reason":"public on purpose","review_on":"2026-11-25"}`, 400, "invalid_review_date"},
		{"review date with a time", http.MethodPost, base + "/" + ids[0] + "/reject", `{"reason":"public on purpose","review_on":"2027-01-01T00:00:00Z"}`, 400, "invalid_review_date"},
		{"body not JSON", http.MethodPost, base + "/" + ids[0] + "/reject", `reason=public`, 400, "invalid_body"},
		{"body empty on reject", http.MethodPost, base + "/" + ids[0] + "/reject", ``, 400, "invalid_body"},
		{"reopen body not JSON", http.MethodPost, base + "/" + ids[1] + "/reopen", `[1]`, 400, "invalid_body"},
		{"bulk body not JSON", http.MethodPost, base + "/reject", `{"ids":"` + ids[0] + `"}`, 400, "invalid_body"},
		{"bulk ids missing", http.MethodPost, base + "/reject", `{"reason":"public on purpose"}`, 400, "invalid_body"},
		{"bulk review date in the past", http.MethodPost, base + "/reject", `{"ids":["` + ids[0] + `"],"reason":"public on purpose","review_on":"2020-01-01"}`, 400, "invalid_review_date"},
		{"invalid kind", http.MethodGet, base + "?kind=cost", "", 400, "invalid_filter"},
		{"invalid severity", http.MethodGet, base + "?severity=urgent", "", 400, "invalid_filter"},
		{"invalid state on the report", http.MethodGet, base + "/report?state=deleted", "", 400, "invalid_filter"},
		{"invalid severity with state=rejected", http.MethodGet, base + "?state=rejected&severity=urgent", "", 400, "invalid_filter"},
		{"reject a resolved issue", http.MethodPost, base + "/" + ids[2] + "/reject", `{"reason":"public on purpose"}`, 409, "not_open"},
		{"acknowledge a rejected issue", http.MethodPost, base + "/" + ids[1] + "/acknowledge", "", 409, "not_open"},
		{"reopen a resolved issue", http.MethodPost, base + "/" + ids[2] + "/reopen", "", 409, "not_rejected"},
		{"reopen an acknowledged issue", http.MethodPost, base + "/" + ids[3] + "/reopen", "", 409, "not_rejected"},
		{"reject a rejected issue", http.MethodPost, base + "/" + ids[1] + "/reject", `{"reason":"public on purpose"}`, 409, "already_rejected"},
		{"detail of an unknown issue", http.MethodGet, base + "/" + strings.Repeat("b", 64), "", 404, "not_found"},
		{"reject an unknown issue", http.MethodPost, base + "/" + strings.Repeat("b", 64) + "/reject", `{"reason":"public on purpose"}`, 404, "not_found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, body := awsTestRequest(t, writer, tc.method, tc.path, tc.body)
			if got := decodeBody[map[string]string](t, body); res.StatusCode != tc.status || got["code"] != tc.code {
				t.Fatalf("= %d %s; want %d %s", res.StatusCode, body, tc.status, tc.code)
			}
		})
	}

	t.Run("no actor", func(t *testing.T) {
		app := noActorApp(t, provider)
		for _, route := range issueRoutes(ids[0], ids[1]) {
			if route.method != http.MethodPost {
				continue
			}
			res, body := awsTestRequest(t, app, route.method, route.path, route.body)
			if got := decodeBody[map[string]string](t, body); res.StatusCode != 400 || got["code"] != "no_actor" {
				t.Errorf("%s without a user = %d %s; want 400 no_actor", route.path, res.StatusCode, body)
			}
		}
	})

	t.Run("conflict", func(t *testing.T) {
		actions, _ := issues.NewActions(conflictingIssues{provider}, tenancy.DefaultOrgScope(), func() time.Time { return issuesNow })
		app := fiber.New(fiber.Config{Immutable: true})
		app.Use(middleware.OrgInjector())
		app.Use(func(ctx *fiber.Ctx) error {
			middleware.MarkAuthorized(ctx)
			middleware.SetRequestActor(ctx, "alice@example.com")
			for permission, allowed := range issuesWriter {
				middleware.SetRequestPermission(ctx, permission, allowed)
			}
			return ctx.Next()
		})
		NewAWSIssuesController(actions, nil, nil, nil).Register(app.Group("/api"))
		for _, route := range []struct{ path, body string }{
			{base + "/" + ids[0] + "/acknowledge", ""},
			{base + "/" + ids[0] + "/reject", `{"reason":"public on purpose"}`},
			{base + "/" + ids[1] + "/reopen", ""},
		} {
			res, body := awsTestRequest(t, app, http.MethodPost, route.path, route.body)
			if got := decodeBody[map[string]string](t, body); res.StatusCode != 409 || got["code"] != "conflict" {
				t.Errorf("%s while the issue keeps changing = %d %s; want 409 conflict", route.path, res.StatusCode, body)
			}
		}
		res, body := awsTestRequest(t, app, http.MethodPost, base+"/reject", `{"ids":["`+ids[0]+`"],"reason":"public on purpose"}`)
		results := decodeBody[awsIssueBulkRejectResponse](t, body).Results
		if res.StatusCode != 200 || len(results) != 1 || results[0].Result != issues.BulkConflict {
			t.Errorf("bulk reject while the issue keeps changing = %d %s; want 200 with conflict", res.StatusCode, body)
		}
	})
}

// The bulk reject edges through the API: 200 IDs are served with one
// result each, 201 are refused before any write, and a repeated ID is
// rejected once and answered once.
func TestIssueBulkRejectEdges(t *testing.T) {
	provider := storage.NewMemory()
	var seeds []issueSeed
	for index := range 3 {
		seeds = append(seeds, issueSeed{owner: fmt.Sprintf("arn:aws:s3:::bulk-%d", index), kind: issues.KindSecurity,
			severity: issues.SeverityHigh, state: issues.StateOpen})
	}
	ids := seedIssueSet(t, provider, seeds)
	writer := issuesApp(t, issuesWriter, provider)
	quoted := func(list []string) string { return `"` + strings.Join(list, `","`) + `"` }

	// 201 with a real ID among them: refused, nothing written.
	many := append([]string{}, ids[0])
	for index := 0; len(many) < 201; index++ {
		many = append(many, fmt.Sprintf("%064x", index+1))
	}
	res, body := awsTestRequest(t, writer, http.MethodPost, "/api/admin/aws/issues/reject", `{"ids":[`+quoted(many)+`],"reason":"public on purpose"}`)
	if got := decodeBody[map[string]string](t, body); res.StatusCode != 400 || got["code"] != "too_many" {
		t.Fatalf("201 IDs = %d %s", res.StatusCode, body)
	}
	if events := issueEventCount(t, provider, ids...); events != 3 {
		t.Fatalf("a refused bulk reject wrote: %d events", events)
	}

	// Exactly 200: one result per ID, in order.
	res, body = awsTestRequest(t, writer, http.MethodPost, "/api/admin/aws/issues/reject", `{"ids":[`+quoted(many[:200])+`],"reason":"public on purpose"}`)
	results := decodeBody[awsIssueBulkRejectResponse](t, body).Results
	if res.StatusCode != 200 || len(results) != 200 || results[0].ID != ids[0] || results[0].Result != issues.BulkRejected {
		t.Fatalf("200 IDs = %d, %d results", res.StatusCode, len(results))
	}
	for _, result := range results[1:] {
		if result.Result != issues.BulkNotFound {
			t.Fatalf("an unknown ID = %+v", result)
		}
	}

	// Duplicates: each issue is rejected once and answered once.
	res, body = awsTestRequest(t, writer, http.MethodPost, "/api/admin/aws/issues/reject",
		`{"ids":[`+quoted([]string{ids[1], ids[1], ids[2], ids[1], ids[2]})+`],"reason":"public on purpose"}`)
	results = decodeBody[awsIssueBulkRejectResponse](t, body).Results
	if res.StatusCode != 200 || len(results) != 2 || results[0] != (issues.BulkResult{ID: ids[1], Result: issues.BulkRejected}) ||
		results[1] != (issues.BulkResult{ID: ids[2], Result: issues.BulkRejected}) {
		t.Fatalf("duplicate IDs = %d %s", res.StatusCode, body)
	}
	if events := issueEventCount(t, provider, ids[1], ids[2]); events != 4 {
		t.Fatalf("duplicate IDs wrote %d events; want opened and one rejected each", events)
	}
}

// The filters combine with state=rejected: the Rejected tab lists only the
// rejected issues the other filters select, and its summary is the summary
// of the same filters without the state.
func TestIssueFiltersCombineWithRejected(t *testing.T) {
	provider := storage.NewMemory()
	seeds := []issueSeed{
		{owner: "arn:aws:s3:::match", account: issuesAccount, kind: issues.KindSecurity, severity: issues.SeverityHigh, state: issues.StateRejected, group: "web"},
		{owner: "arn:aws:s3:::other-account", account: otherIssuesAccount, kind: issues.KindSecurity, severity: issues.SeverityHigh, state: issues.StateRejected, group: "web"},
		{owner: "arn:aws:s3:::other-kind", account: issuesAccount, kind: issues.KindReliability, severity: issues.SeverityHigh, state: issues.StateRejected, group: "web"},
		{owner: "arn:aws:s3:::other-severity", account: issuesAccount, kind: issues.KindSecurity, severity: issues.SeverityLow, state: issues.StateRejected, group: "web"},
		{owner: "arn:aws:s3:::other-group", account: issuesAccount, kind: issues.KindSecurity, severity: issues.SeverityHigh, state: issues.StateRejected, group: "batch"},
		{owner: "arn:aws:s3:::open-match", account: issuesAccount, kind: issues.KindSecurity, severity: issues.SeverityHigh, state: issues.StateOpen, group: "web"},
		{owner: "arn:aws:s3:::resolved-match", account: issuesAccount, kind: issues.KindSecurity, severity: issues.SeverityHigh, state: issues.StateResolved, group: "web"},
	}
	ids := seedIssueSet(t, provider, seeds)
	app := issuesApp(t, issuesViewer, provider)
	filters := "kind=security&severity=high&account=" + issuesAccount + "&group=web"
	res, body := awsTestRequest(t, app, http.MethodGet, "/api/admin/aws/issues?state=rejected&"+filters, "")
	tab := decodeBody[awsIssuesPage](t, body)
	if res.StatusCode != 200 || len(tab.Items) != 1 || tab.Items[0].ID != ids[0] || tab.RejectedCount != 1 {
		t.Fatalf("rejected tab with every filter = %d %s", res.StatusCode, body)
	}
	res, body = awsTestRequest(t, app, http.MethodGet, "/api/admin/aws/issues?"+filters, "")
	page := decodeBody[awsIssuesPage](t, body)
	if res.StatusCode != 200 || len(page.Items) != 1 || page.Items[0].ID != ids[5] || page.Summary != tab.Summary ||
		page.Summary.Open != 1 || page.Summary.RejectedCount != 1 {
		t.Fatalf("open list with the same filters = %d %s", res.StatusCode, body)
	}
	for query, want := range map[string]int{
		"state=rejected&account=" + issuesAccount:                        4,
		"state=rejected&kind=reliability":                                1,
		"state=rejected&severity=low":                                    1,
		"state=rejected&group=batch":                                     1,
		"state=rejected":                                                 5,
		"state=rejected&account=" + otherIssuesAccount + "&severity=low": 0,
	} {
		res, body := awsTestRequest(t, app, http.MethodGet, "/api/admin/aws/issues?"+query, "")
		got := decodeBody[awsIssuesPage](t, body)
		if res.StatusCode != 200 || len(got.Items) != want || got.RejectedCount != want {
			t.Errorf("%s = %d: %d items, rejected count %d; want %d", query, res.StatusCode, len(got.Items), got.RejectedCount, want)
		}
		for _, item := range got.Items {
			if item.State != issues.StateRejected || item.Rejection == nil {
				t.Errorf("%s lists %s in state %s", query, item.ID, item.State)
			}
		}
	}
	res, body = awsTestRequest(t, app, http.MethodGet, "/api/admin/aws/issues/report?state=rejected&"+filters, "")
	report := decodeBody[issues.IssueReport](t, body)
	if res.StatusCode != 200 || len(report.Sections.Rejected) != 1 || len(report.Sections.ActNow)+len(report.Sections.WorthALook) != 0 ||
		report.Filters.State != "rejected" || report.Filters.Group != "web" || report.Summary != tab.Summary {
		t.Fatalf("report with state=rejected and every filter = %d %s", res.StatusCode, body)
	}
}

// The report through the API: capped at 1,000 rows with the exact words,
// the warning line and every Not read source, and counts that match the
// list and the Rejected tab with and without rejected issues. A reason
// comes back as the plain text the user wrote.
func TestIssueReportThroughTheAPI(t *testing.T) {
	t.Run("capped", func(t *testing.T) {
		provider := storage.NewMemory()
		var seeds []issueSeed
		for index := range 1001 {
			seeds = append(seeds, issueSeed{owner: fmt.Sprintf("arn:aws:s3:::cap-%04d", index), kind: issues.KindSecurity,
				severity: issues.SeverityHigh, state: issues.StateOpen})
		}
		seedIssueSet(t, provider, seeds)
		app := issuesApp(t, issuesViewer, provider)
		res, body := awsTestRequest(t, app, http.MethodGet, "/api/admin/aws/issues/report", "")
		report := decodeBody[issues.IssueReport](t, body)
		if res.StatusCode != 200 || report.Rows != 1001 || report.Included != 1000 || len(report.Sections.ActNow) != 1000 ||
			!report.Capped || report.CappedText != "more than 1,000, not all included" {
			t.Fatalf("1,001 rows = %d: rows %d included %d capped %v %q", res.StatusCode, report.Rows, report.Included, report.Capped, report.CappedText)
		}
		if report.Warning == nil || report.Warning.NotRead != 1 || len(report.NotRead) != 1 || report.NotRead[0].Source != "guardduty" {
			t.Fatalf("report header = %+v %+v", report.Warning, report.NotRead)
		}
		if !strings.Contains(string(body), `"capped_text":"more than 1,000, not all included"`) {
			t.Fatalf("the wire carries other words: %.300s", body)
		}
	})
	t.Run("exactly 1,000 is not capped", func(t *testing.T) {
		provider := storage.NewMemory()
		var seeds []issueSeed
		for index := range 1000 {
			state := issues.StateOpen
			if index%10 == 0 {
				state = issues.StateRejected
			}
			seeds = append(seeds, issueSeed{owner: fmt.Sprintf("arn:aws:s3:::full-%04d", index), kind: issues.KindSecurity,
				severity: issues.SeverityHigh, state: state})
		}
		seedIssueSet(t, provider, seeds)
		app := issuesApp(t, issuesViewer, provider)
		res, body := awsTestRequest(t, app, http.MethodGet, "/api/admin/aws/issues/report", "")
		report := decodeBody[issues.IssueReport](t, body)
		if res.StatusCode != 200 || report.Rows != 1000 || report.Included != 1000 || report.Capped || report.CappedText != "" ||
			strings.Contains(string(body), "capped_text") {
			t.Fatalf("1,000 rows = %d: rows %d included %d capped %v %q", res.StatusCode, report.Rows, report.Included, report.Capped, report.CappedText)
		}
	})
	t.Run("counts match with and without rejected", func(t *testing.T) {
		for _, withRejected := range []bool{false, true} {
			provider := storage.NewMemory()
			states := []issues.State{issues.StateOpen, issues.StateAcknowledged, issues.StateRegressed, issues.StateResolved, issues.StateOpen}
			severities := []issues.Severity{issues.SeverityCritical, issues.SeverityHigh, issues.SeverityMedium, issues.SeverityLow, issues.SeverityHigh}
			var seeds []issueSeed
			for index, state := range states {
				seeds = append(seeds, issueSeed{owner: fmt.Sprintf("arn:aws:s3:::count-%d", index), kind: issues.KindSecurity, severity: severities[index], state: state})
			}
			if withRejected {
				for index, severity := range []issues.Severity{issues.SeverityCritical, issues.SeverityHigh, issues.SeverityLow} {
					seeds = append(seeds, issueSeed{owner: fmt.Sprintf("arn:aws:s3:::rejected-%d", index), kind: issues.KindSecurity, severity: severity, state: issues.StateRejected})
				}
			}
			seedIssueSet(t, provider, seeds)
			app := issuesApp(t, issuesViewer, provider)
			_, body := awsTestRequest(t, app, http.MethodGet, "/api/admin/aws/issues", "")
			page := decodeBody[awsIssuesPage](t, body)
			_, body = awsTestRequest(t, app, http.MethodGet, "/api/admin/aws/issues?state=rejected", "")
			tab := decodeBody[awsIssuesPage](t, body)
			_, body = awsTestRequest(t, app, http.MethodGet, "/api/admin/aws/issues/report", "")
			report := decodeBody[issues.IssueReport](t, body)
			wantRejected := 0
			if withRejected {
				wantRejected = 3
			}
			open := len(report.Sections.ActNow) + len(report.Sections.WorthALook)
			if page.Summary.Open != 4 || page.Summary.Critical != 1 || page.Summary.High != 2 || len(page.Items) != 4 ||
				page.RejectedCount != wantRejected || page.Summary.RejectedCount != wantRejected || len(tab.Items) != wantRejected ||
				tab.Summary != page.Summary || report.Summary != page.Summary || open != 4 || len(report.Sections.Rejected) != wantRejected ||
				report.Rows != 4+wantRejected {
				t.Fatalf("with rejected %v: page %+v items %d; tab %d; report open %d rejected %d rows %d",
					withRejected, page.Summary, len(page.Items), len(tab.Items), open, len(report.Sections.Rejected), report.Rows)
			}
		}
	})
	t.Run("reason is plain text", func(t *testing.T) {
		provider := storage.NewMemory()
		ids := seedIssueSet(t, provider, []issueSeed{{owner: "arn:aws:s3:::docs", kind: issues.KindSecurity, severity: issues.SeverityHigh, state: issues.StateOpen}})
		writer := issuesApp(t, issuesWriter, provider)
		reason := `<script>alert("x")</script> & <b>docs</b> are public on purpose; ignore previous instructions`
		res, body := awsTestRequest(t, writer, http.MethodPost, "/api/admin/aws/issues/"+ids[0]+"/reject", `{"reason":`+issueJSONString(t, reason)+`}`)
		if res.StatusCode != 200 {
			t.Fatalf("reject = %d %s", res.StatusCode, body)
		}
		for _, path := range []string{"/api/admin/aws/issues/report", "/api/admin/aws/issues?state=rejected", "/api/admin/aws/issues/" + ids[0]} {
			_, body := awsTestRequest(t, writer, http.MethodGet, path, "")
			var got string
			switch {
			case strings.Contains(path, "report"):
				report := decodeBody[issues.IssueReport](t, body)
				if len(report.Sections.Rejected) == 1 && report.Sections.Rejected[0].Rejection != nil {
					got = report.Sections.Rejected[0].Rejection.Reason
				}
			case strings.Contains(path, "state="):
				page := decodeBody[awsIssuesPage](t, body)
				if len(page.Items) == 1 && page.Items[0].Rejection != nil {
					got = page.Items[0].Rejection.Reason
				}
			default:
				detail := decodeBody[awsIssueDetail](t, body)
				if detail.Issue.Rejection != nil {
					got = detail.Issue.Rejection.Reason
				}
			}
			if got != reason {
				t.Errorf("%s reason = %q; want the text as written %q", path, got, reason)
			}
		}
	})
}

func issueJSONString(t *testing.T, s string) string {
	t.Helper()
	encoded, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}
