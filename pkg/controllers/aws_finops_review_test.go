package controllers

import (
	"net/http"
	"strings"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/aws/cost"
	"github.com/gofiber/fiber/v2"
)

// TestFinOpsDrillDownRefusesWhatWouldWasteRequests: invalid accounts never
// reach the backend; unknown pairs answer 404 and a click that would
// starve the scheduled refresh answers 429.
func TestFinOpsDrillDownRefusesWhatWouldWasteRequests(t *testing.T) {
	backend := finopsBackend(t)
	app, _ := finopsApp(t, backend, finopsAdmin, false)
	for _, account := range []string{"12345678901a", "1234", "1234567890123"} {
		res, _ := awsTestRequest(t, app, http.MethodPost, "/api/admin/aws/finops/drilldown", `{"account":"`+account+`","service":"EC2 - Other"}`)
		if res.StatusCode != fiber.StatusBadRequest {
			t.Fatalf("account %q: %d", account, res.StatusCode)
		}
	}
	if backend.drills != 0 {
		t.Fatal("an invalid account reached the backend")
	}
	for err, want := range map[error][2]any{
		cost.ErrUnknownPair:        {fiber.StatusNotFound, "not_found"},
		cost.ErrReservedForRefresh: {fiber.StatusTooManyRequests, "reserved_for_scheduled_refresh"},
		cost.ErrInvalidAccount:     {fiber.StatusBadRequest, "invalid_body"},
	} {
		backend.drillErr = err
		res, body := awsTestRequest(t, app, http.MethodPost, "/api/admin/aws/finops/drilldown", `{"account":"111111111111","service":"Invented"}`)
		if res.StatusCode != want[0].(int) || !strings.Contains(string(body), want[1].(string)) {
			t.Fatalf("%v: %d %s", err, res.StatusCode, body)
		}
	}
}

// TestFinOpsAuditNamesTheOutcome: an estimate is not a refresh, and a
// repeat freeze is not a freeze.
func TestFinOpsAuditNamesTheOutcome(t *testing.T) {
	app, events := finopsApp(t, finopsBackend(t), finopsAdmin, false)
	awsTestRequest(t, app, http.MethodPost, "/api/admin/aws/finops/refresh", `{"confirm":false}`)
	awsTestRequest(t, app, http.MethodPost, "/api/admin/aws/finops/refresh", ``)
	awsTestRequest(t, app, http.MethodPost, "/api/admin/aws/finops/refresh", `{"confirm":true}`)
	awsTestRequest(t, app, http.MethodPost, "/api/admin/aws/finops/reports", `{"month":"2026-10"}`)
	awsTestRequest(t, app, http.MethodPost, "/api/admin/aws/finops/reports", `{"month":"2026-10"}`)
	var got []string
	for _, e := range *events {
		got = append(got, e.Action)
	}
	want := []string{"aws.finops.refresh.estimated", "aws.finops.refresh.estimated", "aws.finops.refreshed",
		"aws.finops.report.frozen", "aws.finops.report.already_frozen"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("audit actions %v, want %v", got, want)
	}
}
