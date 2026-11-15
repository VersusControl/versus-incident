package controllers

import (
	"encoding/json"
	"io"
	"os"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/services"
	"github.com/VersusControl/versus-incident/pkg/storage"

	"github.com/gofiber/fiber/v2"
)

// postWebhook sends one webhook body and returns the status and decoded body.
func postWebhook(t *testing.T, app *fiber.App, body string) (int, map[string]interface{}) {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/incidents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	var decoded map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp.StatusCode, decoded
}

// A stored webhook incident answers 201 with the channel failure in the body,
// so a sender that retries on 5xx does not store the alert again.
func TestCreateIncident_ChannelFailureAfterStoreAnswersCreated(t *testing.T) {
	loadGatewayConfig(t, "secret")
	cfg := config.GetConfig()
	prev := cfg.Alert
	cfg.Alert.Lark = config.LarkConfig{
		Enable:       true,
		WebhookURL:   "http://127.0.0.1:1/unused",
		TemplatePath: filepath.Join(t.TempDir(), "missing.tmpl"),
	}
	t.Cleanup(func() { cfg.Alert = prev })
	mem := storage.NewMemory()
	services.SetStorage(mem)
	t.Cleanup(func() { services.SetStorage(nil) })

	app := fiber.New()
	app.Post("/api/incidents", CreateIncident)

	status, body := postWebhook(t, app, `{"Logs":"[ERROR] boom","ServiceName":"checkout"}`)
	if status != fiber.StatusCreated {
		t.Fatalf("status = %d, want 201; body %v", status, body)
	}
	if body["notify_status"] != "failed" || !strings.HasPrefix(body["notify_error"].(string), "lark: ") || body["incident_id"] == "" {
		t.Fatalf("body = %v, want incident_id with notify_status failed and the lark reason", body)
	}

	status, body = postWebhook(t, app, `[{"Logs":"a"},{"Logs":"b"}]`)
	if status != fiber.StatusCreated || body["count"] != float64(2) {
		t.Fatalf("array status = %d body = %v, want 201 count 2", status, body)
	}
	if rows, _ := body["incidents"].([]interface{}); len(rows) != 2 {
		t.Fatalf("array incidents = %v, want two outcomes", body["incidents"])
	}

	recs, err := mem.ListIncidents(0)
	if err != nil {
		t.Fatalf("ListIncidents: %v", err)
	}
	if len(recs) != 3 {
		t.Fatalf("stored incidents = %d, want 3", len(recs))
	}
}

// Without storage the alert is not recorded, so the failure stays a 500 and a
// retry is the only delivery path.
func TestCreateIncident_ChannelFailureWithoutStoreAnswers500(t *testing.T) {
	loadGatewayConfig(t, "secret")
	cfg := config.GetConfig()
	prev := cfg.Alert
	cfg.Alert.Lark = config.LarkConfig{
		Enable:       true,
		WebhookURL:   "http://127.0.0.1:1/unused",
		TemplatePath: filepath.Join(t.TempDir(), "missing.tmpl"),
	}
	t.Cleanup(func() { cfg.Alert = prev })
	services.SetStorage(nil)

	app := fiber.New()
	app.Post("/api/incidents", CreateIncident)
	if status, body := postWebhook(t, app, `{"Logs":"boom"}`); status != fiber.StatusInternalServerError {
		t.Fatalf("status = %d body = %v, want 500", status, body)
	}
}

// A stored incident whose on-call escalation fails still answers 201, and the
// failure is readable afterwards on the incident detail and its list row, so a
// failed escalation is never invisible. A row without a failure carries an
// empty oncall_error.
func TestCreateIncident_OnCallFailureAnswersCreatedAndIsRecorded(t *testing.T) {
	loadGatewayConfig(t, "secret")
	cfg := config.GetConfig()
	prev := cfg.OnCall
	cfg.OnCall.Enable = true
	t.Cleanup(func() { cfg.OnCall = prev })
	// A workflow without a queue backend refuses every escalation.
	core.SetOnCallWorkflow(core.NewOnCallWorkflow(nil, nil))
	t.Cleanup(func() { core.SetOnCallWorkflow(nil) })
	mem := storage.NewMemory()
	services.SetStorage(mem)
	t.Cleanup(func() { services.SetStorage(nil) })

	app := fiber.New()
	app.Post("/api/incidents", CreateIncident)
	status, body := postWebhook(t, app, `{"Logs":"[ERROR] boom","ServiceName":"checkout","severity":"critical"}`)
	if status != fiber.StatusCreated {
		t.Fatalf("status = %d, want 201; body %v", status, body)
	}
	onCallErr, _ := body["oncall_error"].(string)
	id, _ := body["incident_id"].(string)
	if onCallErr == "" || id == "" {
		t.Fatalf("body = %v, want incident_id and oncall_error", body)
	}
	if err := mem.SaveIncident(&storage.IncidentRecord{ID: "quiet", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("SaveIncident: %v", err)
	}

	ctrl := NewIncidentAdminController()
	admin := fiber.New()
	admin.Get("/incidents", ctrl.list)
	admin.Get("/incidents/:id", ctrl.get)
	getJSON := func(path string, out any) {
		t.Helper()
		resp, err := admin.Test(httptest.NewRequest("GET", path, nil))
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != fiber.StatusOK {
			t.Fatalf("GET %s: status = %d", path, resp.StatusCode)
		}
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("GET %s: decode: %v", path, err)
		}
	}

	var detail map[string]any
	getJSON("/incidents/"+id, &detail)
	if detail["oncall_error"] != onCallErr || detail["id"] != id {
		t.Fatalf("detail = %v, want id %q with oncall_error %q", detail, id, onCallErr)
	}
	stored, err := mem.GetIncident(id)
	if err != nil {
		t.Fatalf("GetIncident: %v", err)
	}
	wantSeverity := services.ExtractSeverity(stored.Content)
	if wantSeverity == "" {
		t.Fatalf("stored content %v carries no severity; the detail check would prove nothing", stored.Content)
	}
	if detail["severity"] != wantSeverity {
		t.Fatalf("detail severity = %v, want %q (the list row's value)", detail["severity"], wantSeverity)
	}
	var quiet map[string]any
	getJSON("/incidents/quiet", &quiet)
	if v, present := quiet["oncall_error"]; !present || v != "" {
		t.Fatalf("detail without a failure: oncall_error = %v (present %v), want empty string", v, present)
	}

	var list struct {
		Incidents []map[string]any `json:"incidents"`
	}
	getJSON("/incidents", &list)
	rows := map[string]map[string]any{}
	for _, row := range list.Incidents {
		rows[row["id"].(string)] = row
	}
	if rows[id]["severity"] != detail["severity"] {
		t.Fatalf("list severity = %v, detail severity = %v, want equal", rows[id]["severity"], detail["severity"])
	}
	if rows[id]["oncall_error"] != onCallErr {
		t.Fatalf("list row = %v, want oncall_error %q", rows[id], onCallErr)
	}
	if v, present := rows["quiet"]["oncall_error"]; !present || v != "" {
		t.Fatalf("list row without a failure: oncall_error = %v (present %v), want empty string", v, present)
	}
}

// The intake response is unauthenticated, so a channel failure whose transport
// error carries the channel's credential (a Lark hook token in the path, a
// Teams signature in the query) must come back, and be stored, with the URL
// cut to scheme://host.
func TestCreateIncident_ChannelErrorURLsAreRedacted(t *testing.T) {
	loadGatewayConfig(t, "secret")
	cfg := config.GetConfig()
	prev := cfg.Alert
	t.Cleanup(func() { cfg.Alert = prev })
	tpl := filepath.Join(t.TempDir(), "msg.tmpl")
	if err := os.WriteFile(tpl, []byte("{{.Logs}}"), 0o600); err != nil {
		t.Fatalf("write template: %v", err)
	}
	cfg.Alert.Lark = config.LarkConfig{Enable: true, WebhookURL: "http://127.0.0.1:1/open-apis/bot/v2/hook/SECRETHOOK", TemplatePath: tpl}
	cfg.Alert.MSTeams = config.MSTeamsConfig{Enable: true, PowerAutomateURL: "http://127.0.0.1:1/workflows/x/invoke?api-version=1&sig=SECRETSIG", TemplatePath: tpl}
	mem := storage.NewMemory()
	services.SetStorage(mem)
	t.Cleanup(func() { services.SetStorage(nil) })

	app := fiber.New()
	app.Post("/api/incidents", CreateIncident)
	req := httptest.NewRequest("POST", "/api/incidents", strings.NewReader(`{"Logs":"[ERROR] boom"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != fiber.StatusCreated {
		t.Fatalf("status = %d body %s, want 201", resp.StatusCode, raw)
	}
	if strings.Contains(string(raw), "SECRET") || !strings.Contains(string(raw), "http://127.0.0.1:1") {
		t.Fatalf("intake body = %s, want channel errors with the URL cut to scheme://host", raw)
	}
	recs, err := mem.ListIncidents(0)
	if err != nil || len(recs) != 1 {
		t.Fatalf("ListIncidents: %d, %v", len(recs), err)
	}
	if recs[0].NotifyError == "" || strings.Contains(recs[0].NotifyError, "SECRET") {
		t.Fatalf("stored notify_error = %q, want a redacted failure", recs[0].NotifyError)
	}
}
