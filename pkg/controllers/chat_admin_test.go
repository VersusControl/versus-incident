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

	"github.com/VersusControl/versus-incident/pkg/agent/act"
	chatagent "github.com/VersusControl/versus-incident/pkg/agent/ai/chat"
	"github.com/VersusControl/versus-incident/pkg/agent/ai/router"
	"github.com/VersusControl/versus-incident/pkg/agent/ledger"
	"github.com/VersusControl/versus-incident/pkg/agentapi"
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
	core.EmitChatEvent(ctx, core.ChatEvent{Seq: 2, Kind: core.ChatEventApproval, Approval: &core.ChatApproval{ID: "approval-1", State: "pending"}, ApprovalNonce: "nonce-from-turn"})
	core.EmitChatEvent(ctx, core.ChatEvent{Seq: 3, Kind: core.ChatEventRunFinished})
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

func TestChatCallerContextPreservesExplicitApprovalPermission(t *testing.T) {
	allowed := true
	app := fiber.New()
	app.Use(func(c *fiber.Ctx) error {
		middleware.MarkAuthorized(c)
		middleware.SetRequestPermission(c, string(core.PermissionAgentApprove), allowed)
		return c.Next()
	})
	app.Get("/permission", func(c *fiber.Ctx) error {
		ctx := core.WithCallerAuthorization(context.Background(), callerAuthorization(c))
		if !core.CallerAuthorized(ctx, core.PermissionAgentApprove) {
			return c.SendStatus(fiber.StatusForbidden)
		}
		return c.SendStatus(fiber.StatusOK)
	})
	response, err := app.Test(httptest.NewRequest(http.MethodGet, "/permission", nil), -1)
	if err != nil || response.StatusCode != fiber.StatusOK {
		t.Fatalf("authorized caller status=%d err=%v", response.StatusCode, err)
	}
	response.Body.Close()
	allowed = false
	response, err = app.Test(httptest.NewRequest(http.MethodGet, "/permission", nil), -1)
	if err != nil || response.StatusCode != fiber.StatusForbidden {
		t.Fatalf("denied caller status=%d err=%v", response.StatusCode, err)
	}
	response.Body.Close()
}

func TestCommunityPermissionGrantIncludesAgentApproval(t *testing.T) {
	app := fiber.New()
	app.Get("/community", func(c *fiber.Ctx) error {
		grantCommunityPermissions(c)
		ctx := core.WithCallerAuthorization(c.UserContext(), callerAuthorization(c))
		if !core.CallerAuthorized(ctx, core.PermissionAgentApprove) || core.CallerActor(ctx) != communityRequestActor {
			return c.SendStatus(fiber.StatusForbidden)
		}
		return c.SendStatus(fiber.StatusOK)
	})
	response, err := app.Test(httptest.NewRequest(http.MethodGet, "/community", nil), -1)
	if err != nil || response.StatusCode != fiber.StatusOK {
		t.Fatalf("community approval permission status=%d err=%v", response.StatusCode, err)
	}
	response.Body.Close()
}

func TestAPIV1ChatAliasesReuseSessionAndTurnRuntime(t *testing.T) {
	app, _ := chatTestApp(t, storage.NewMemory(), apiChatRunner{}, true)
	bootstrapResponse, err := app.Test(httptest.NewRequest(http.MethodGet, "/api/v1/agent/bootstrap", nil), -1)
	if err != nil || bootstrapResponse.StatusCode != fiber.StatusOK {
		t.Fatalf("API-v1 bootstrap status=%d err=%v", bootstrapResponse.StatusCode, err)
	}
	var bootstrap agentapi.Bootstrap
	if err := json.NewDecoder(bootstrapResponse.Body).Decode(&bootstrap); err != nil {
		t.Fatal(err)
	}
	bootstrapResponse.Body.Close()
	if bootstrap.APIVersion != agentapi.APIVersion || len(bootstrap.Profiles) != 1 || bootstrap.Profiles[0].Name != "chat" {
		t.Fatalf("unexpected bootstrap contract: %+v", bootstrap)
	}
	created, err := app.Test(httptest.NewRequest(http.MethodPost, "/api/v1/agent/sessions", nil), -1)
	if err != nil || created.StatusCode != fiber.StatusCreated {
		t.Fatalf("API-v1 create status=%d err=%v", created.StatusCode, err)
	}
	var session chatagent.Session
	if err := json.NewDecoder(created.Body).Decode(&session); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/agent/sessions/"+session.ID+"/turns", strings.NewReader(`{"message":"investigate checkout"}`))
	request.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
	request.Header.Set(fiber.HeaderAccept, "text/event-stream")
	response, err := app.Test(request, -1)
	if err != nil || response.StatusCode != fiber.StatusOK || !strings.HasPrefix(response.Header.Get(fiber.HeaderContentType), "text/event-stream") {
		t.Fatalf("API-v1 turn status=%d content-type=%q err=%v", response.StatusCode, response.Header.Get(fiber.HeaderContentType), err)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil || !strings.Contains(string(body), "event: model_delta") || !strings.Contains(string(body), "event: approval_required") || !strings.Contains(string(body), "nonce-from-turn") || !strings.Contains(string(body), "event: run_finished") {
		t.Fatalf("API-v1 turn stream=%q err=%v", body, err)
	}
	replayResponse, err := app.Test(httptest.NewRequest(http.MethodGet, "/api/v1/agent/sessions/"+session.ID+"/events?after=0", nil), -1)
	if err != nil || replayResponse.StatusCode != fiber.StatusOK {
		t.Fatalf("API-v1 replay status=%d err=%v", replayResponse.StatusCode, err)
	}
	var replay agentapi.Replay
	if err := json.NewDecoder(replayResponse.Body).Decode(&replay); err != nil {
		t.Fatal(err)
	}
	replayResponse.Body.Close()
	if replay.SessionID != session.ID || len(replay.Events) == 0 || replay.Events[len(replay.Events)-1].Kind != core.ChatEventRunFinished {
		t.Fatalf("unexpected replay: %+v", replay)
	}
	jsonSessionResponse, err := app.Test(httptest.NewRequest(http.MethodPost, "/api/v1/agent/sessions", nil), -1)
	if err != nil {
		t.Fatal(err)
	}
	var jsonSession agentapi.Session
	if err := json.NewDecoder(jsonSessionResponse.Body).Decode(&jsonSession); err != nil {
		t.Fatal(err)
	}
	jsonSessionResponse.Body.Close()
	jsonTurn := httptest.NewRequest(http.MethodPost, "/api/v1/agent/sessions/"+jsonSession.ID+"/turns", strings.NewReader(`{"message":"status","mode":"interactive","profile":"chat"}`))
	jsonTurn.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
	jsonResponse, err := app.Test(jsonTurn, -1)
	if err != nil || jsonResponse.StatusCode != fiber.StatusOK || !strings.HasPrefix(jsonResponse.Header.Get(fiber.HeaderContentType), fiber.MIMEApplicationJSON) {
		t.Fatalf("API-v1 JSON turn status=%d content-type=%q err=%v", jsonResponse.StatusCode, jsonResponse.Header.Get(fiber.HeaderContentType), err)
	}
	var turnResponse agentapi.TurnResponse
	if err := json.NewDecoder(jsonResponse.Body).Decode(&turnResponse); err != nil {
		t.Fatal(err)
	}
	jsonResponse.Body.Close()
	if turnResponse.SessionID != jsonSession.ID || turnResponse.Assistant.Content == "" || len(turnResponse.Assistant.Events) == 0 || turnResponse.Assistant.Events[0].Kind != core.ChatEventModelDelta {
		t.Fatalf("unexpected JSON turn response: %+v", turnResponse)
	}
	foundApproval := false
	for _, event := range turnResponse.Assistant.Events {
		if event.Kind == core.ChatEventApproval && event.Approval != nil && event.Approval.ID == "approval-1" && event.ApprovalNonce == "nonce-from-turn" {
			foundApproval = true
		}
	}
	if !foundApproval {
		t.Fatalf("JSON turn response lost approval event: %+v", turnResponse.Assistant.Events)
	}
}

type approvalRouteAdapter struct{}

func (approvalRouteAdapter) Type() act.ActionType { return "k8s.rollout_restart" }
func (approvalRouteAdapter) Destructive() bool    { return false }
func (approvalRouteAdapter) Schema() map[string]any {
	return map[string]any{"type": "object"}
}
func (approvalRouteAdapter) Validate(context.Context, act.Proposal) error { return nil }
func (approvalRouteAdapter) DryRun(context.Context, act.Proposal) (string, error) {
	return "restart workload", nil
}
func (approvalRouteAdapter) Execute(context.Context, act.Proposal) (act.Result, error) {
	return act.Result{Summary: "restart requested"}, nil
}
func (approvalRouteAdapter) Verify(context.Context, act.Proposal, act.Result) (act.Verification, error) {
	return act.Verification{Verified: true, Summary: "rollout ready"}, nil
}

func TestAgentAPIApprovalRoutesRequirePermissionAndUseBoundNonce(t *testing.T) {
	provider := storage.NewMemory()
	service, err := act.NewService(provider, "default", ledger.NewBlobWriter(provider, "default"), nil, approvalRouteAdapter{})
	if err != nil {
		t.Fatal(err)
	}
	SetAgentApprovalServiceFactory(func(tenancy.OrgScope) *act.Service { return service })
	t.Cleanup(func() { SetAgentApprovalServiceFactory(nil) })

	deniedApp := fiber.New()
	deniedApp.Use(middleware.OrgInjector())
	deniedApp.Use(func(c *fiber.Ctx) error {
		middleware.MarkAuthorized(c)
		return c.Next()
	})
	NewChatAdminController(nil).Register(deniedApp.Group("/api"))
	denied, err := deniedApp.Test(httptest.NewRequest(http.MethodGet, "/api/v1/agent/approvals", nil), -1)
	if err != nil || denied.StatusCode != fiber.StatusForbidden {
		t.Fatalf("approval without permission status=%d err=%v", denied.StatusCode, err)
	}
	deniedBootstrap, err := deniedApp.Test(httptest.NewRequest(http.MethodGet, "/api/v1/agent/bootstrap", nil), -1)
	if err != nil || deniedBootstrap.StatusCode != fiber.StatusOK {
		t.Fatalf("bootstrap without approval permission status=%d err=%v", deniedBootstrap.StatusCode, err)
	}
	var deniedFeatures agentapi.Bootstrap
	if err := json.NewDecoder(deniedBootstrap.Body).Decode(&deniedFeatures); err != nil {
		t.Fatal(err)
	}
	deniedBootstrap.Body.Close()
	if deniedFeatures.Features.Approvals || deniedFeatures.Features.Ledger {
		t.Fatalf("bootstrap exposed unavailable approval features: %+v", deniedFeatures.Features)
	}
	deniedProposal := httptest.NewRequest(http.MethodPost, "/api/v1/agent/proposals", strings.NewReader(`{"type":"k8s.rollout_restart","target":{"kind":"Deployment","namespace":"shop","name":"checkout"},"params":{},"reason":"restore service"}`))
	deniedProposal.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
	denied, err = deniedApp.Test(deniedProposal, -1)
	if err != nil || denied.StatusCode != fiber.StatusForbidden {
		t.Fatalf("proposal without permission status=%d err=%v", denied.StatusCode, err)
	}

	missingActorApp := fiber.New()
	missingActorApp.Use(middleware.OrgInjector())
	missingActorApp.Use(func(c *fiber.Ctx) error {
		middleware.MarkAuthorized(c)
		middleware.SetRequestPermission(c, string(core.PermissionAgentApprove), true)
		return c.Next()
	})
	NewChatAdminController(nil).Register(missingActorApp.Group("/api"))
	missingActorRequest := httptest.NewRequest(http.MethodPost, "/api/v1/agent/proposals", strings.NewReader(`{"type":"k8s.rollout_restart","target":{"kind":"Deployment","namespace":"shop","name":"checkout"},"params":{},"reason":"restore service"}`))
	missingActorRequest.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
	missingActorResponse, err := missingActorApp.Test(missingActorRequest, -1)
	if err != nil || missingActorResponse.StatusCode != fiber.StatusConflict {
		t.Fatalf("proposal with permission but no principal status=%d err=%v", missingActorResponse.StatusCode, err)
	}
	missingActorResponse.Body.Close()

	app := fiber.New()
	app.Use(middleware.OrgInjector())
	app.Use(func(c *fiber.Ctx) error {
		middleware.MarkAuthorized(c)
		middleware.SetRequestPermission(c, string(core.PermissionAgentApprove), true)
		middleware.SetRequestActor(c, "operator-1")
		return c.Next()
	})
	NewChatAdminController(nil).Register(app.Group("/api"))
	bootstrapResponse, err := app.Test(httptest.NewRequest(http.MethodGet, "/api/v1/agent/bootstrap", nil), -1)
	if err != nil {
		t.Fatal(err)
	}
	var bootstrap agentapi.Bootstrap
	if err := json.NewDecoder(bootstrapResponse.Body).Decode(&bootstrap); err != nil {
		t.Fatal(err)
	}
	bootstrapResponse.Body.Close()
	if !bootstrap.Features.Approvals || !bootstrap.Features.Ledger {
		t.Fatalf("approval features = %+v", bootstrap.Features)
	}
	proposalRequest := httptest.NewRequest(http.MethodPost, "/api/v1/agent/proposals", strings.NewReader(`{"type":"k8s.rollout_restart","target":{"kind":"Deployment","namespace":"shop","name":"checkout"},"params":{},"reason":"restore service"}`))
	proposalRequest.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
	proposalResponse, err := app.Test(proposalRequest, -1)
	if err != nil || proposalResponse.StatusCode != fiber.StatusCreated {
		responseBody, _ := io.ReadAll(proposalResponse.Body)
		t.Fatalf("proposal status=%d body=%s err=%v", proposalResponse.StatusCode, responseBody, err)
	}
	var proposal act.ProposalResult
	if err := json.NewDecoder(proposalResponse.Body).Decode(&proposal); err != nil {
		t.Fatal(err)
	}
	proposalResponse.Body.Close()
	if proposal.Proposal == nil || proposal.Approval == nil || proposal.Nonce == "" {
		t.Fatalf("proposal response=%+v", proposal)
	}

	listResponse, err := app.Test(httptest.NewRequest(http.MethodGet, "/api/v1/agent/approvals?state=pending", nil), -1)
	if err != nil || listResponse.StatusCode != fiber.StatusOK {
		t.Fatalf("approval list status=%d err=%v", listResponse.StatusCode, err)
	}
	var list struct {
		Approvals []act.Approval `json:"approvals"`
	}
	if err := json.NewDecoder(listResponse.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	listResponse.Body.Close()
	if len(list.Approvals) != 1 || list.Approvals[0].ID != proposal.Approval.ID {
		t.Fatalf("approval list = %+v", list)
	}

	body := strings.NewReader(`{"nonce":"` + proposal.Nonce + `"}`)
	approveRequest := httptest.NewRequest(http.MethodPost, "/api/v1/agent/approvals/"+proposal.Approval.ID+"/approve", body)
	approveRequest.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
	approved, err := app.Test(approveRequest, -1)
	if err != nil || approved.StatusCode != fiber.StatusOK {
		responseBody, _ := io.ReadAll(approved.Body)
		t.Fatalf("approval status=%d body=%s err=%v", approved.StatusCode, responseBody, err)
	}
	var result act.Approval
	if err := json.NewDecoder(approved.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	approved.Body.Close()
	if result.State != "verified" || result.Approver != "operator-1" {
		t.Fatalf("approval result = %+v", result)
	}
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
	observer.OnChatEvent(core.ChatEvent{Seq: 11, Kind: core.ChatEventApproval, Approval: &core.ChatApproval{ID: "approval-1"}, ApprovalNonce: "nonce-1"})
	observer.OnChatEvent(core.ChatEvent{Seq: 12, Kind: core.ChatEventRunFinished})
	if !observer.terminal.Load() {
		t.Fatal("terminal event was not enqueued")
	}
	found := false
	for len(events) > 0 {
		queued := <-events
		if queued.Kind == core.ChatEventRunFinished {
			found = true
		}
		if queued.Kind == core.ChatEventApproval && (queued.Approval == nil || queued.Approval.ID != "approval-1" || queued.ApprovalNonce != "nonce-1") {
			t.Fatal("approval event lost its ID or nonce")
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
