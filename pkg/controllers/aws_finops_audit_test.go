package controllers

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/finops"
	"github.com/VersusControl/versus-incident/pkg/middleware"

	"github.com/gofiber/fiber/v2"
)

func auditCount(events []middleware.AdminAuditEvent, action, result string) int {
	n := 0
	for _, e := range events {
		if e.Action == action && (result == "" || e.Result == result) {
			n++
		}
	}
	return n
}

// TestFinOpsReadsAreAuditedOnlyWhereTheContractSaysSo: the report view and
// the CSV export are audited reads, one event per request; the page reads
// are not audited at all.
func TestFinOpsReadsAreAuditedOnlyWhereTheContractSaysSo(t *testing.T) {
	app, events := finopsApp(t, finopsBackend(t), finopsViewer, false)
	for _, path := range []string{"summary", "bridge", "trend", "breakdown", "increases", "savings", "accounts", "budgets", "reports"} {
		if res, _ := awsTestRequest(t, app, http.MethodGet, "/api/admin/aws/finops/"+path+"?period=2026-10", ""); res.StatusCode != fiber.StatusOK {
			t.Fatalf("%s: %d", path, res.StatusCode)
		}
	}
	if len(*events) != 0 {
		t.Fatalf("page reads are not audited: %+v", *events)
	}
	for range 2 {
		if res, _ := awsTestRequest(t, app, http.MethodGet, "/api/admin/aws/finops/report?period=2026-10", ""); res.StatusCode != fiber.StatusOK {
			t.Fatalf("report: %d", res.StatusCode)
		}
	}
	awsTestRequest(t, app, http.MethodGet, "/api/admin/aws/finops/export.csv?period=2026-10&table=savings", "")
	if got := auditCount(*events, "aws.finops.report.viewed", middleware.AdminAuditSuccess); got != 2 {
		t.Fatalf("each report view is audited once: %d of %+v", got, *events)
	}
	if got := auditCount(*events, "aws.finops.exported", middleware.AdminAuditSuccess); got != 1 || len(*events) != 3 {
		t.Fatalf("one export, one event: %+v", *events)
	}
}

// TestFinOpsDismissNeedsAReasonAndIsAuditedOnce: dismissing a saving is an
// admin write. A viewer is refused and the refusal is audited; an admin
// without a reason is refused; with a reason the item is dismissed, keeps
// the reason and the actor, and one success event is recorded.
func TestFinOpsDismissNeedsAReasonAndIsAuditedOnce(t *testing.T) {
	backend := finopsBackend(t)
	viewer, viewerEvents := finopsApp(t, backend, finopsViewer, false)
	_, body := awsTestRequest(t, viewer, http.MethodGet, "/api/admin/aws/finops/savings", "")
	var before finops.SavingsView
	if err := json.Unmarshal(body, &before); err != nil || len(before.Items) == 0 {
		t.Fatalf("savings: %v %s", err, body)
	}
	id := before.Items[0].ID
	path := "/api/admin/aws/finops/savings/" + id + "/dismiss"
	if res, _ := awsTestRequest(t, viewer, http.MethodPost, path, `{"reason":"kept for DR"}`); res.StatusCode != fiber.StatusForbidden {
		t.Fatalf("viewer dismiss %d", res.StatusCode)
	}
	if len(*viewerEvents) != 1 || (*viewerEvents)[0].Action != "rbac.denied" {
		t.Fatalf("the refused write is audited once: %+v", *viewerEvents)
	}

	admin, events := finopsApp(t, backend, finopsAdmin, false)
	for _, bad := range []string{`{"reason":"  "}`, `{}`} {
		if res, _ := awsTestRequest(t, admin, http.MethodPost, path, bad); res.StatusCode != fiber.StatusBadRequest {
			t.Fatalf("dismiss without a reason %s: %d", bad, res.StatusCode)
		}
	}
	if res, body := awsTestRequest(t, admin, http.MethodPost, path, `{"reason":"kept for DR"}`); res.StatusCode != fiber.StatusOK {
		t.Fatalf("dismiss %d %s", res.StatusCode, body)
	}
	if got := auditCount(*events, "aws.finops.saving.dismissed", middleware.AdminAuditDenied); got != 2 {
		t.Fatalf("each refused dismiss is audited once as denied: %+v", *events)
	}
	if got := auditCount(*events, "aws.finops.saving.dismissed", middleware.AdminAuditSuccess); got != 1 || len(*events) != 3 {
		t.Fatalf("one success event: %+v", *events)
	}
	_, body = awsTestRequest(t, admin, http.MethodGet, "/api/admin/aws/finops/savings", "")
	var after finops.SavingsView
	_ = json.Unmarshal(body, &after)
	found := false
	for _, it := range after.Items {
		if it.ID == id {
			found = true
			if it.Status != "dismissed" || it.Dismissed == nil || it.Dismissed.Reason != "kept for DR" {
				t.Fatalf("dismissed item %+v", it)
			}
		}
	}
	if !found || after.Total.Count != before.Total.Count-1 {
		t.Fatalf("dismissed item kept and left out of the total: found %v, %d → %d", found, before.Total.Count, after.Total.Count)
	}
}
