package controllers

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	chatagent "github.com/VersusControl/versus-incident/pkg/agent/ai/chat"
	"github.com/VersusControl/versus-incident/pkg/agent/ai/router"
	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/middleware"
	"github.com/VersusControl/versus-incident/pkg/services"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"

	"github.com/gofiber/fiber/v2"
)

type apiChatRunner struct{}

type rateLimitedAPIRunner struct{}

func (rateLimitedAPIRunner) RunChat(context.Context, core.ChatTask) (*core.ChatTurnResult, error) {
	return nil, router.ErrRateLimited
}

func (apiChatRunner) RunChat(ctx context.Context, _ core.ChatTask) (*core.ChatTurnResult, error) {
	core.EmitChatEvent(ctx, core.ChatEvent{Seq: 1, Kind: core.ChatEventModelDelta, Delta: "hello\nworld"})
	core.EmitChatEvent(ctx, core.ChatEvent{Seq: 2, Kind: core.ChatEventRunFinished})
	return &core.ChatTurnResult{Markdown: "hello"}, nil
}

type blockingAPIRunner struct {
	started chan struct{}
	once    sync.Once
}

type secretErrorProvider struct {
	storage.Provider
	secret string
}

type nilIncidentProvider struct {
	storage.Provider
}

func (provider *nilIncidentProvider) GetIncident(string) (*storage.IncidentRecord, error) {
	return nil, nil
}

type legacyIncidentProvider struct {
	storage.Provider
	record *storage.IncidentRecord
}

func (provider *legacyIncidentProvider) GetIncident(string) (*storage.IncidentRecord, error) {
	return provider.record, nil
}

func (provider *secretErrorProvider) ReadBlob(string) ([]byte, error) {
	return nil, fmt.Errorf("postgres %s unavailable", provider.secret)
}

func (provider *secretErrorProvider) CompareAndSwapBlob(name string, expected, replacement []byte) (bool, error) {
	return provider.Provider.(storage.BlobCAS).CompareAndSwapBlob(name, expected, replacement)
}

func (runner *blockingAPIRunner) RunChat(ctx context.Context, _ core.ChatTask) (*core.ChatTurnResult, error) {
	runner.once.Do(func() { close(runner.started) })
	<-ctx.Done()
	return nil, ctx.Err()
}

func chatTestApp(t *testing.T, provider storage.Provider, runner chatagent.TurnRunner, authorized bool) (*fiber.App, *ChatAdminController) {
	t.Helper()
	app := fiber.New(fiber.Config{Immutable: true})
	if authorized {
		app.Use(func(c *fiber.Ctx) error {
			middleware.MarkAuthorized(c)
			return c.Next()
		})
	}
	app.Use(middleware.OrgInjector())
	controller := NewChatAdminController(func(scope tenancy.OrgScope) *chatagent.Service {
		return chatagent.NewService(chatagent.NewSessionStore(provider, scope, time.Now), runner, nil, time.Now)
	})
	controller.Register(app.Group("/api"))
	return app, controller
}

func createChatSession(t *testing.T, app *fiber.App) string {
	t.Helper()
	request := httptest.NewRequest("POST", "/api/admin/chat/sessions", nil)
	response, err := app.Test(request, -1)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != fiber.StatusCreated {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("create status = %d body=%s", response.StatusCode, body)
	}
	var session chatagent.Session
	if err := json.NewDecoder(response.Body).Decode(&session); err != nil {
		t.Fatal(err)
	}
	return session.ID
}

func TestChatAdminRequiresAuth(t *testing.T) {
	loadGatewayConfig(t, "chat-test-secret")
	previous := config.GetConfig().GatewaySecret
	config.GetConfig().GatewaySecret = "chat-test-secret"
	t.Cleanup(func() { config.GetConfig().GatewaySecret = previous })
	app, _ := chatTestApp(t, storage.NewMemory(), apiChatRunner{}, false)
	response, err := app.Test(httptest.NewRequest("POST", "/api/admin/chat/sessions", nil), -1)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", response.StatusCode)
	}
}

func TestChatAdminUnavailableReturns503(t *testing.T) {
	SetChatServiceFactory(nil)
	t.Cleanup(func() { SetChatServiceFactory(nil) })
	app := fiber.New()
	app.Use(func(c *fiber.Ctx) error {
		middleware.MarkAuthorized(c)
		return c.Next()
	})
	NewChatAdminController(nil).Register(app.Group("/api"))
	response, err := app.Test(httptest.NewRequest("POST", "/api/admin/chat/sessions", nil), -1)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != fiber.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", response.StatusCode)
	}
}

func TestChatAdminCRUDAndSSE(t *testing.T) {
	provider := storage.NewMemory()
	services.SetStorage(provider)
	t.Cleanup(func() { services.SetStorage(nil) })
	app, _ := chatTestApp(t, provider, apiChatRunner{}, true)
	id := createChatSession(t, app)

	body := bytes.NewBufferString(`{"message":"hello"}`)
	request := httptest.NewRequest("POST", "/api/admin/chat/sessions/"+id+"/messages", body)
	request.Header.Set("Content-Type", "application/json")
	response, err := app.Test(request, -1)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != fiber.StatusOK || !strings.Contains(string(data), "event: model_delta\n") || !strings.Contains(string(data), "event: run_finished\n") {
		t.Fatalf("SSE status=%d body=%s", response.StatusCode, data)
	}

	getResponse, _ := app.Test(httptest.NewRequest("GET", "/api/admin/chat/sessions/"+id, nil), -1)
	var session chatagent.Session
	_ = json.NewDecoder(getResponse.Body).Decode(&session)
	getResponse.Body.Close()
	if len(session.Turns) != 2 {
		t.Fatalf("persisted turns = %d, want 2", len(session.Turns))
	}
	assistant := session.Turns[1]
	if len(assistant.Events) == 0 || assistant.Events[len(assistant.Events)-1].Kind != core.ChatEventRunFinished || assistant.Events[len(assistant.Events)-1].Output != assistant.ID {
		t.Fatalf("assistant terminal was not persisted after turn: %+v", assistant)
	}
	if strings.Count(string(data), "event: run_finished\n") != 1 {
		t.Fatalf("run_finished count != 1: %s", data)
	}

	deleteResponse, _ := app.Test(httptest.NewRequest("DELETE", "/api/admin/chat/sessions/"+id, nil), -1)
	if deleteResponse.StatusCode != fiber.StatusNoContent {
		t.Fatalf("delete status = %d", deleteResponse.StatusCode)
	}
}

func TestChatAdminOversizedNaturalLanguageHintIsAccepted(t *testing.T) {
	provider := storage.NewMemory()
	services.SetStorage(provider)
	t.Cleanup(func() { services.SetStorage(nil) })
	app, _ := chatTestApp(t, provider, apiChatRunner{}, true)
	id := createChatSession(t, app)

	request := httptest.NewRequest("POST", "/api/admin/chat/sessions/"+id+"/messages", strings.NewReader(`{"message":"summarize incidents in the last 90 days"}`))
	request.Header.Set("Content-Type", "application/json")
	response, err := app.Test(request, -1)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, _ := io.ReadAll(response.Body)
	if response.StatusCode != fiber.StatusOK || !strings.Contains(string(data), "event: run_finished\n") {
		t.Fatalf("SSE status=%d body=%s", response.StatusCode, data)
	}
}

func TestChatAdminAuditEmitsExactlyOneBoundedOutcomePerMutation(t *testing.T) {
	provider := storage.NewMemory()
	app, _ := chatTestApp(t, provider, apiChatRunner{}, true)
	var mu sync.Mutex
	var events []middleware.ChatAuditEvent
	middleware.SetChatAuditHook(func(*fiber.Ctx) middleware.ChatAuditRecorder {
		return func(event middleware.ChatAuditEvent) {
			mu.Lock()
			events = append(events, event)
			mu.Unlock()
		}
	})
	t.Cleanup(func() { middleware.SetChatAuditHook(nil) })
	id := createChatSession(t, app)
	request := httptest.NewRequest("POST", "/api/admin/chat/sessions/"+id+"/messages", strings.NewReader(`{"message":"secret prompt"}`))
	request.Header.Set("Content-Type", "application/json")
	response, err := app.Test(request, -1)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	response.Body.Close()
	deleteResponse, err := app.Test(httptest.NewRequest("DELETE", "/api/admin/chat/sessions/"+id, nil), -1)
	if err != nil {
		t.Fatal(err)
	}
	deleteResponse.Body.Close()

	mu.Lock()
	defer mu.Unlock()
	want := []string{chatAuditSessionCreated, chatAuditMessageSent, chatAuditMessageResult, chatAuditSessionDeleted}
	for _, action := range want {
		count := 0
		for _, event := range events {
			if event.Action == action {
				count++
				if event.Result != middleware.ChatAuditSuccess || len(event.Target) > 128 || strings.Contains(event.Target, "secret") {
					t.Fatalf("unsafe audit event: %+v", event)
				}
			}
		}
		if count != 1 {
			t.Fatalf("action %q count=%d events=%+v", action, count, events)
		}
	}
}

func TestChatAdminAuditMarksThrottledResult(t *testing.T) {
	provider := storage.NewMemory()
	app, _ := chatTestApp(t, provider, rateLimitedAPIRunner{}, true)
	var mu sync.Mutex
	var events []middleware.ChatAuditEvent
	middleware.SetChatAuditHook(func(*fiber.Ctx) middleware.ChatAuditRecorder {
		return func(event middleware.ChatAuditEvent) {
			mu.Lock()
			events = append(events, event)
			mu.Unlock()
		}
	})
	t.Cleanup(func() { middleware.SetChatAuditHook(nil) })

	id := createChatSession(t, app)
	request := httptest.NewRequest("POST", "/api/admin/chat/sessions/"+id+"/messages", strings.NewReader(`{"message":"hello"}`))
	request.Header.Set("Content-Type", "application/json")
	response, err := app.Test(request, -1)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if count := strings.Count(string(body), "event: run_throttled\n"); count != 1 {
		t.Fatalf("run_throttled count = %d, body=%s", count, body)
	}

	mu.Lock()
	defer mu.Unlock()
	count := 0
	for _, event := range events {
		if event.Action == chatAuditMessageResult {
			count++
			if event.Result != middleware.ChatAuditThrottled {
				t.Fatalf("message result audit = %+v, want throttled", event)
			}
		}
	}
	if count != 1 {
		t.Fatalf("message result count = %d, events=%+v", count, events)
	}
}

func TestChatAdminInternalErrorLogOmitsBackendSecret(t *testing.T) {
	secret := "postgres://operator:dsn-secret@db.internal/chat"
	provider := &secretErrorProvider{Provider: storage.NewMemory(), secret: secret}
	app, _ := chatTestApp(t, provider, apiChatRunner{}, true)
	var logs bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previous) })
	response, err := app.Test(httptest.NewRequest("POST", "/api/admin/chat/sessions", nil), -1)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if strings.Contains(logs.String(), secret) || strings.Contains(logs.String(), "dsn-secret") {
		t.Fatalf("backend secret leaked to logs: %s", logs.String())
	}
	if !strings.Contains(logs.String(), "code=backend_failure") {
		t.Fatalf("stable error code missing from logs: %s", logs.String())
	}
}

func TestChatAdminConflictAndCancel(t *testing.T) {
	runner := &blockingAPIRunner{started: make(chan struct{})}
	app, _ := chatTestApp(t, storage.NewMemory(), runner, true)
	id := createChatSession(t, app)
	firstDone := make(chan *http.Response, 1)
	go func() {
		request := httptest.NewRequest("POST", "/api/admin/chat/sessions/"+id+"/messages", strings.NewReader(`{"message":"first"}`))
		request.Header.Set("Content-Type", "application/json")
		response, _ := app.Test(request, -1)
		firstDone <- response
	}()
	<-runner.started

	second := httptest.NewRequest("POST", "/api/admin/chat/sessions/"+id+"/messages", strings.NewReader(`{"message":"second"}`))
	second.Header.Set("Content-Type", "application/json")
	secondResponse, _ := app.Test(second, -1)
	if secondResponse.StatusCode != fiber.StatusConflict {
		t.Fatalf("second status = %d, want 409", secondResponse.StatusCode)
	}
	cancelResponse, _ := app.Test(httptest.NewRequest("POST", "/api/admin/chat/sessions/"+id+"/cancel", nil), -1)
	if cancelResponse.StatusCode != fiber.StatusAccepted {
		t.Fatalf("cancel status = %d, want 202", cancelResponse.StatusCode)
	}
	select {
	case response := <-firstDone:
		data, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if !strings.Contains(string(data), "run_cancelled") {
			t.Fatalf("cancelled stream = %s", data)
		}
	case <-time.After(time.Second):
		t.Fatal("cancel did not stop SSE run")
	}
}

func TestWriteSSESupportsMultilineData(t *testing.T) {
	var output bytes.Buffer
	writer := bufio.NewWriter(&output)
	if err := writeSSE(writer, "delta", []byte("one\ntwo")); err != nil {
		t.Fatal(err)
	}
	_ = writer.Flush()
	if output.String() != "event: delta\ndata: one\ndata: two\n\n" {
		t.Fatalf("frame = %q", output.String())
	}
}

func TestChatStreamObserverReservesTerminalSlot(t *testing.T) {
	events := make(chan core.ChatEvent, 4)
	observer := &chatStreamObserver{events: events}
	for index := 0; index < 10; index++ {
		observer.OnChatEvent(core.ChatEvent{Seq: int64(index + 1), Kind: core.ChatEventModelDelta})
	}
	observer.OnChatEvent(core.ChatEvent{Seq: 11, Kind: core.ChatEventRunFinished})
	if !observer.terminal.Load() {
		t.Fatal("terminal event was not enqueued")
	}
	found := false
	for len(events) > 0 {
		if (<-events).Kind == core.ChatEventRunFinished {
			found = true
		}
	}
	if !found {
		t.Fatal("reserved slot did not contain terminal event")
	}
}

func TestHydrateIncidentAttachmentReplacesClientFields(t *testing.T) {
	provider := storage.NewMemory()
	if err := provider.SaveIncident(&storage.IncidentRecord{
		ID: "incident-1", OrgID: storage.DefaultOrgID, Title: "safe title", Service: "api",
		CreatedAt: time.Now(), Content: map[string]any{"password": "must-not-egress"},
	}); err != nil {
		t.Fatal(err)
	}
	services.SetStorage(provider)
	t.Cleanup(func() { services.SetStorage(nil) })
	app := fiber.New()
	var hydrated *core.ChatAttachment
	app.Post("/hydrate", func(c *fiber.Ctx) error {
		hydrated = &core.ChatAttachment{Incident: &core.ChatIncidentContext{ID: "incident-1", Title: "client raw payload", Severity: "forged"}}
		return hydrateIncidentAttachment(c, hydrated)
	})
	response, err := app.Test(httptest.NewRequest("POST", "/hydrate", nil), -1)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	encoded, _ := json.Marshal(hydrated)
	if strings.Contains(string(encoded), "must-not-egress") || strings.Contains(string(encoded), "client raw payload") || hydrated.Incident.Title != "safe title" {
		t.Fatalf("hydrated attachment = %s", encoded)
	}
}

func TestHydrateIncidentAttachmentHandlesNilAndLegacyDefaultRecords(t *testing.T) {
	t.Run("nil record is not found", func(t *testing.T) {
		services.SetStorage(&nilIncidentProvider{Provider: storage.NewMemory()})
		t.Cleanup(func() { services.SetStorage(nil) })
		app := fiber.New()
		app.Post("/hydrate", func(c *fiber.Ctx) error {
			return hydrateIncidentAttachment(c, &core.ChatAttachment{Incident: &core.ChatIncidentContext{ID: "missing"}})
		})
		response, err := app.Test(httptest.NewRequest("POST", "/hydrate", nil), -1)
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != fiber.StatusNotFound {
			t.Fatalf("status = %d, want 404", response.StatusCode)
		}
	})

	t.Run("empty org belongs to default scope", func(t *testing.T) {
		provider := &legacyIncidentProvider{
			Provider: storage.NewMemory(),
			record:   &storage.IncidentRecord{ID: "legacy", OrgID: "", Title: "legacy", CreatedAt: time.Now()},
		}
		services.SetStorage(provider)
		t.Cleanup(func() { services.SetStorage(nil) })
		app := fiber.New()
		var attachment = &core.ChatAttachment{Incident: &core.ChatIncidentContext{ID: "legacy"}}
		app.Post("/hydrate", func(c *fiber.Ctx) error { return hydrateIncidentAttachment(c, attachment) })
		response, err := app.Test(httptest.NewRequest("POST", "/hydrate", nil), -1)
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != fiber.StatusOK || attachment.Incident.Title != "legacy" {
			t.Fatalf("status = %d attachment = %+v", response.StatusCode, attachment.Incident)
		}
	})
}

func TestChatAdminFailureStatuses(t *testing.T) {
	provider := storage.NewMemory()
	services.SetStorage(provider)
	t.Cleanup(func() { services.SetStorage(nil) })
	app, _ := chatTestApp(t, provider, apiChatRunner{}, true)
	id := createChatSession(t, app)

	tests := []struct {
		name   string
		method string
		path   string
		body   string
		status int
	}{
		{name: "get missing session", method: "GET", path: "/api/admin/chat/sessions/missing", status: fiber.StatusNotFound},
		{name: "delete missing session", method: "DELETE", path: "/api/admin/chat/sessions/missing", status: fiber.StatusNotFound},
		{name: "message missing session", method: "POST", path: "/api/admin/chat/sessions/missing/messages", body: `{"message":"hello"}`, status: fiber.StatusNotFound},
		{name: "cancel idle session", method: "POST", path: "/api/admin/chat/sessions/" + id + "/cancel", status: fiber.StatusConflict},
		{name: "malformed request", method: "POST", path: "/api/admin/chat/sessions/" + id + "/messages", body: `{`, status: fiber.StatusBadRequest},
		{name: "invalid attachment", method: "POST", path: "/api/admin/chat/sessions/" + id + "/messages", body: `{"message":"hello","attachment":{"service":"` + strings.Repeat("a", 257) + `"}}`, status: fiber.StatusBadRequest},
		{name: "missing incident attachment", method: "POST", path: "/api/admin/chat/sessions/" + id + "/messages", body: `{"message":"hello","attachment":{"incident":{"id":"missing"}}}`, status: fiber.StatusNotFound},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
			if test.body != "" {
				request.Header.Set("Content-Type", "application/json")
			}
			response, err := app.Test(request, -1)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if response.StatusCode != test.status {
				body, _ := io.ReadAll(response.Body)
				t.Fatalf("status = %d, want %d body=%s", response.StatusCode, test.status, body)
			}
		})
	}
}
