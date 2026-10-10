package controllers

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/VersusControl/versus-incident/pkg/agent/act"
	chatagent "github.com/VersusControl/versus-incident/pkg/agent/ai/chat"
	"github.com/VersusControl/versus-incident/pkg/agent/ai/router"
	"github.com/VersusControl/versus-incident/pkg/agent/ledger"
	"github.com/VersusControl/versus-incident/pkg/agentapi"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/middleware"
	"github.com/VersusControl/versus-incident/pkg/services"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"

	"github.com/gofiber/fiber/v2"
)

const chatEventBuffer = 256

const (
	chatAuditSessionCreated = "chat.session.created"
	chatAuditMessageSent    = "chat.message.sent"
	chatAuditMessageResult  = "chat.message.result"
	chatAuditRunCancelled   = "chat.run.cancelled"
	chatAuditSessionDeleted = "chat.session.deleted"
)

type ChatServiceFactory func(tenancy.OrgScope) *chatagent.Service
type AgentApprovalServiceFactory func(tenancy.OrgScope) *act.Service

var (
	chatFactoryMu     sync.RWMutex
	chatFactory       ChatServiceFactory
	approvalFactoryMu sync.RWMutex
	approvalFactory   AgentApprovalServiceFactory
)

// SetChatServiceFactory installs the boot-built chat service factory. Routes
// are mounted before AI construction so an absent factory degrades to 503.
func SetChatServiceFactory(factory ChatServiceFactory) {
	chatFactoryMu.Lock()
	chatFactory = factory
	chatFactoryMu.Unlock()
}

func currentChatServiceFactory() ChatServiceFactory {
	chatFactoryMu.RLock()
	defer chatFactoryMu.RUnlock()
	return chatFactory
}

// SetAgentApprovalServiceFactory installs the org-scoped H4 approval service factory.
func SetAgentApprovalServiceFactory(factory AgentApprovalServiceFactory) {
	approvalFactoryMu.Lock()
	approvalFactory = factory
	approvalFactoryMu.Unlock()
}

func currentAgentApprovalServiceFactory() AgentApprovalServiceFactory {
	approvalFactoryMu.RLock()
	defer approvalFactoryMu.RUnlock()
	return approvalFactory
}

type ChatAdminController struct {
	factory       ChatServiceFactory
	mu            sync.Mutex
	byOrg         map[string]*chatagent.Service
	approvalByOrg map[string]*act.Service
}

func NewChatAdminController(factory ChatServiceFactory) *ChatAdminController {
	return &ChatAdminController{factory: factory, byOrg: map[string]*chatagent.Service{}, approvalByOrg: map[string]*act.Service{}}
}

func (controller *ChatAdminController) Register(router fiber.Router) {
	group := router.Group("/admin/chat", adminGatewayGuard)
	group.Use(func(c *fiber.Ctx) error {
		c.Set("Deprecation", "true")
		return c.Next()
	})
	group.Post("/sessions", controller.create)
	group.Get("/sessions", controller.list)
	group.Get("/sessions/:id", controller.get)
	group.Delete("/sessions/:id", controller.delete)
	group.Post("/sessions/:id/messages", controller.message)
	group.Post("/sessions/:id/cancel", controller.cancel)
	apiV1 := router.Group("/v1/agent", adminGatewayGuard)
	apiV1.Get("/bootstrap", controller.bootstrap)
	apiV1.Post("/sessions", controller.createV1)
	apiV1.Get("/sessions", controller.listV1)
	apiV1.Get("/sessions/:id", controller.getV1)
	apiV1.Delete("/sessions/:id", controller.delete)
	apiV1.Post("/sessions/:id/turns", controller.message)
	apiV1.Post("/sessions/:id/cancel", controller.cancel)
	apiV1.Get("/sessions/:id/events", controller.replayEvents)
	apiV1.Post("/proposals", controller.requireAgentApproval, controller.createProposal)
	apiV1.Get("/approvals", controller.requireAgentApproval, controller.listApprovals)
	apiV1.Post("/approvals/:id/approve", controller.requireAgentApproval, controller.approve)
	apiV1.Post("/approvals/:id/reject", controller.requireAgentApproval, controller.reject)
}

func (controller *ChatAdminController) service(c *fiber.Ctx) *chatagent.Service {
	if controller == nil {
		return nil
	}
	factory := controller.factory
	if factory == nil {
		factory = currentChatServiceFactory()
	}
	if factory == nil {
		return nil
	}
	org := strings.Clone(middleware.OrgFromContext(c))
	controller.mu.Lock()
	defer controller.mu.Unlock()
	if service := controller.byOrg[org]; service != nil {
		return service
	}
	service := factory(tenancy.NewOrgScope(org))
	controller.byOrg[org] = service
	return service
}

func (controller *ChatAdminController) approvalService(c *fiber.Ctx) *act.Service {
	factory := currentAgentApprovalServiceFactory()
	if controller == nil || factory == nil {
		return nil
	}
	org := strings.Clone(middleware.OrgFromContext(c))
	controller.mu.Lock()
	defer controller.mu.Unlock()
	if service := controller.approvalByOrg[org]; service != nil {
		return service
	}
	service := factory(tenancy.NewOrgScope(org))
	if service != nil {
		controller.approvalByOrg[org] = service
	}
	return service
}

func (controller *ChatAdminController) requireAgentApproval(c *fiber.Ctx) error {
	if controller.approvalService(c) == nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(agentapi.Error{Code: "unavailable", Message: "approval service not configured"})
	}
	allowed, explicit := middleware.RequestPermission(c, string(core.PermissionAgentApprove))
	if !explicit || !allowed {
		return c.Status(fiber.StatusForbidden).JSON(agentapi.Error{Code: "forbidden", Message: "agent:approve permission is required"})
	}
	return c.Next()
}

func approvalContext(c *fiber.Ctx) context.Context {
	return core.WithCallerAuthorization(c.UserContext(), core.CallerAuthorization{
		Actor:         requestActor(c),
		Authenticated: true,
		Permissions:   map[core.Permission]bool{core.PermissionAgentApprove: true},
		Clusters:      core.CallerClusterScope(c.UserContext()),
	})
}

func (controller *ChatAdminController) listApprovals(c *fiber.Ctx) error {
	state := c.Query("state", "pending")
	if state != "pending" && state != "all" {
		return c.Status(fiber.StatusBadRequest).JSON(agentapi.Error{Code: "invalid_request", Message: "state must be pending or all"})
	}
	approvals, err := controller.approvalService(c).Approvals(approvalContext(c), state == "pending")
	if err != nil {
		return writeApprovalError(c, err)
	}
	return c.JSON(fiber.Map{"approvals": approvals})
}

func (controller *ChatAdminController) createProposal(c *fiber.Ctx) error {
	var input act.ProposalInput
	if err := c.BodyParser(&input); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(agentapi.Error{Code: "invalid_request", Message: "invalid action proposal"})
	}
	result, err := controller.approvalService(c).Propose(approvalContext(c), input)
	if err != nil {
		return writeApprovalError(c, err)
	}
	return c.Status(fiber.StatusCreated).JSON(result)
}

func (controller *ChatAdminController) approve(c *fiber.Ctx) error {
	var request struct {
		Nonce string `json:"nonce"`
	}
	if err := c.BodyParser(&request); err != nil || strings.TrimSpace(request.Nonce) == "" {
		return c.Status(fiber.StatusBadRequest).JSON(agentapi.Error{Code: "invalid_request", Message: "nonce is required"})
	}
	approval, err := controller.approvalService(c).Approve(approvalContext(c), c.Params("id"), request.Nonce, requestActor(c))
	if err != nil {
		return writeApprovalError(c, err)
	}
	return c.JSON(approval)
}

func (controller *ChatAdminController) reject(c *fiber.Ctx) error {
	var request struct {
		Reason string `json:"reason"`
	}
	if err := c.BodyParser(&request); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(agentapi.Error{Code: "invalid_request", Message: "invalid request"})
	}
	approval, err := controller.approvalService(c).Reject(approvalContext(c), c.Params("id"), request.Reason, requestActor(c))
	if err != nil {
		return writeApprovalError(c, err)
	}
	return c.JSON(approval)
}

func requestActor(c *fiber.Ctx) string {
	return middleware.RequestActor(c)
}

func writeApprovalError(c *fiber.Ctx, err error) error {
	if errors.Is(err, act.ErrActionDenied) {
		return c.Status(fiber.StatusConflict).JSON(agentapi.Error{Code: "approval_denied", Message: "approval is no longer available"})
	}
	if errors.Is(err, ledger.ErrLedgerUnavailable) {
		return c.Status(fiber.StatusServiceUnavailable).JSON(agentapi.Error{Code: "ledger_unavailable", Message: "approval could not be recorded"})
	}
	return c.Status(fiber.StatusServiceUnavailable).JSON(agentapi.Error{Code: "backend_failure", Message: internalServerErrorMessage})
}

func (controller *ChatAdminController) create(c *fiber.Ctx) error {
	auditor := middleware.ChatAuditor(c)
	service := controller.service(c)
	if service == nil || !service.Available() {
		auditor(middleware.ChatAuditEvent{Action: chatAuditSessionCreated, Result: middleware.ChatAuditDenied})
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "chat not enabled"})
	}
	session, err := service.Create()
	if err != nil {
		auditor(middleware.ChatAuditEvent{Action: chatAuditSessionCreated, Result: middleware.ChatAuditFailure})
		return chatInternalError(c, "create_session", err)
	}
	auditor(middleware.ChatAuditEvent{Action: chatAuditSessionCreated, Target: chatAuditTarget(session.ID), Result: middleware.ChatAuditSuccess})
	return c.Status(fiber.StatusCreated).JSON(session)
}

func (controller *ChatAdminController) bootstrap(c *fiber.Ctx) error {
	approvalAllowed, explicit := middleware.RequestPermission(c, string(core.PermissionAgentApprove))
	approvalsAvailable := currentAgentApprovalServiceFactory() != nil && explicit && approvalAllowed
	var kubernetesCatalog *agentapi.KubernetesCatalog
	chatKubernetesResolverMu.RLock()
	registry := chatKubernetesRegistry
	chatKubernetesResolverMu.RUnlock()
	allowed, permissionSet := middleware.RequestPermission(c, string(core.PermissionInfrastructureView))
	if registry != nil && registry.Multiple() && permissionSet && allowed {
		ctx := c.UserContext()
		if !core.CallerAuthorizationPresent(ctx) {
			ctx = core.WithCallerAuthorization(ctx, core.CallerAuthorization{Authenticated: true, Permissions: map[core.Permission]bool{core.PermissionInfrastructureView: true}})
		}
		kubernetesCatalog = &agentapi.KubernetesCatalog{Multiple: true, Clusters: registry.Summaries(ctx, middleware.OrgFromContext(c), func(id string) bool { return core.CallerClusterAllowed(ctx, id) })}
	}
	return c.JSON(agentapi.Bootstrap{
		APIVersion: agentapi.APIVersion,
		MinClient:  agentapi.MinClient,
		Server:     agentapi.ServerInfo{Name: "Versus Incident"},
		Principal:  agentapi.Principal{Org: middleware.OrgFromContext(c)},
		Profiles:   []agentapi.Profile{{Name: "chat"}},
		Toolsets:   []agentapi.Toolset{{Name: "chat"}},
		Features:   agentapi.Features{Approvals: approvalsAvailable, Ledger: approvalsAvailable},
		Kubernetes: kubernetesCatalog,
	})
}

func (controller *ChatAdminController) createV1(c *fiber.Ctx) error {
	service := controller.service(c)
	if service == nil || !service.Available() {
		return c.Status(fiber.StatusServiceUnavailable).JSON(agentapi.Error{Code: "unavailable", Message: "chat not enabled"})
	}
	session, err := service.Create()
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(agentapi.Error{Code: "backend_failure", Message: internalServerErrorMessage})
	}
	return c.Status(fiber.StatusCreated).JSON(apiSession(session))
}

func (controller *ChatAdminController) listV1(c *fiber.Ctx) error {
	service := controller.service(c)
	if service == nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(agentapi.Error{Code: "unavailable", Message: "chat not enabled"})
	}
	sessions, err := service.List()
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(agentapi.Error{Code: "backend_failure", Message: internalServerErrorMessage})
	}
	result := make([]agentapi.Session, 0, len(sessions))
	for _, session := range sessions {
		result = append(result, apiSession(session))
	}
	return c.JSON(fiber.Map{"sessions": result})
}

func (controller *ChatAdminController) getV1(c *fiber.Ctx) error {
	service := controller.service(c)
	if service == nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(agentapi.Error{Code: "unavailable", Message: "chat not enabled"})
	}
	session, err := service.Get(c.Params("id"))
	if errors.Is(err, chatagent.ErrSessionNotFound) {
		return c.Status(fiber.StatusNotFound).JSON(agentapi.Error{Code: "not_found", Message: "session not found"})
	}
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(agentapi.Error{Code: "backend_failure", Message: internalServerErrorMessage})
	}
	return c.JSON(apiSession(session))
}

func (controller *ChatAdminController) replayEvents(c *fiber.Ctx) error {
	after, err := strconv.ParseInt(c.Query("after", "0"), 10, 64)
	if err != nil || after < 0 {
		return c.Status(fiber.StatusBadRequest).JSON(agentapi.Error{Code: "invalid_cursor", Message: "after must be a non-negative sequence"})
	}
	service := controller.service(c)
	if service == nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(agentapi.Error{Code: "unavailable", Message: "chat not enabled"})
	}
	session, err := service.Get(c.Params("id"))
	if errors.Is(err, chatagent.ErrSessionNotFound) {
		return c.Status(fiber.StatusNotFound).JSON(agentapi.Error{Code: "not_found", Message: "session not found"})
	}
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(agentapi.Error{Code: "backend_failure", Message: internalServerErrorMessage})
	}
	events := make([]agentapi.Event, 0)
	var seq int64
	for _, turn := range session.Turns {
		for _, event := range turn.Events {
			seq++
			if seq > after {
				mapped := apiEvent(event)
				mapped.Seq = seq
				events = append(events, mapped)
			}
		}
	}
	return c.JSON(agentapi.Replay{SessionID: session.ID, After: after, Events: events})
}

func (controller *ChatAdminController) list(c *fiber.Ctx) error {
	service := controller.service(c)
	if service == nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "chat not enabled"})
	}
	sessions, err := service.List()
	if err != nil {
		return chatInternalError(c, "list sessions", err)
	}
	return c.JSON(fiber.Map{"sessions": sessions})
}

func (controller *ChatAdminController) get(c *fiber.Ctx) error {
	service := controller.service(c)
	if service == nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "chat not enabled"})
	}
	session, err := service.Get(c.Params("id"))
	if errors.Is(err, chatagent.ErrSessionNotFound) {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "not found"})
	}
	if err != nil {
		return chatInternalError(c, "get session", err)
	}
	return c.JSON(session)
}

func (controller *ChatAdminController) delete(c *fiber.Ctx) error {
	auditor := middleware.ChatAuditor(c)
	target := chatAuditTarget(c.Params("id"))
	service := controller.service(c)
	if service == nil {
		auditor(middleware.ChatAuditEvent{Action: chatAuditSessionDeleted, Target: target, Result: middleware.ChatAuditDenied})
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "chat not enabled"})
	}
	err := service.Delete(c.Params("id"))
	if errors.Is(err, chatagent.ErrSessionNotFound) {
		auditor(middleware.ChatAuditEvent{Action: chatAuditSessionDeleted, Target: target, Result: middleware.ChatAuditDenied})
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "not found"})
	}
	if errors.Is(err, chatagent.ErrRunActive) {
		auditor(middleware.ChatAuditEvent{Action: chatAuditSessionDeleted, Target: target, Result: middleware.ChatAuditDenied})
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{"error": "session has an active run"})
	}
	if err != nil {
		auditor(middleware.ChatAuditEvent{Action: chatAuditSessionDeleted, Target: target, Result: middleware.ChatAuditFailure})
		return chatInternalError(c, "delete_session", err)
	}
	auditor(middleware.ChatAuditEvent{Action: chatAuditSessionDeleted, Target: target, Result: middleware.ChatAuditSuccess})
	return c.SendStatus(fiber.StatusNoContent)
}

type chatMessageRequest struct {
	Message    string               `json:"message"`
	Attachment *core.ChatAttachment `json:"attachment,omitempty"`
	Mode       agentapi.TurnMode    `json:"mode,omitempty"`
	Profile    string               `json:"profile,omitempty"`
}

func (controller *ChatAdminController) message(c *fiber.Ctx) error {
	auditor := middleware.ChatAuditor(c)
	target := chatAuditTarget(c.Params("id"))
	service := controller.service(c)
	if service == nil || !service.Available() {
		auditor(middleware.ChatAuditEvent{Action: chatAuditMessageSent, Target: target, Result: middleware.ChatAuditDenied})
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "chat not enabled"})
	}
	var request chatMessageRequest
	if isAgentAPIv1(c) {
		var wireRequest agentapi.TurnRequest
		if err := c.BodyParser(&wireRequest); err != nil {
			auditor(middleware.ChatAuditEvent{Action: chatAuditMessageSent, Target: target, Result: middleware.ChatAuditDenied})
			return c.Status(fiber.StatusBadRequest).JSON(agentapi.Error{Code: "invalid_request", Message: "invalid request"})
		}
		if (wireRequest.Mode != "" && wireRequest.Mode != agentapi.ModeInteractive && wireRequest.Mode != agentapi.ModeHeadless) || (wireRequest.Profile != "" && wireRequest.Profile != "chat") {
			auditor(middleware.ChatAuditEvent{Action: chatAuditMessageSent, Target: target, Result: middleware.ChatAuditDenied})
			return c.Status(fiber.StatusBadRequest).JSON(agentapi.Error{Code: "invalid_request", Message: "unsupported mode or profile"})
		}
		request = chatMessageRequest{Message: wireRequest.Message, Attachment: coreAttachment(wireRequest.Attachment), Mode: wireRequest.Mode, Profile: wireRequest.Profile}
	} else if err := c.BodyParser(&request); err != nil {
		auditor(middleware.ChatAuditEvent{Action: chatAuditMessageSent, Target: target, Result: middleware.ChatAuditDenied})
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid request"})
	}
	if err := hydrateIncidentAttachment(c, request.Attachment); err != nil {
		auditor(middleware.ChatAuditEvent{Action: chatAuditMessageSent, Target: target, Result: middleware.ChatAuditDenied})
		return err
	}
	if err := validateChatKubernetesAttachment(c, request.Attachment); err != nil {
		auditor(middleware.ChatAuditEvent{Action: chatAuditMessageSent, Target: target, Result: middleware.ChatAuditDenied})
		return err
	}
	id := strings.Clone(c.Params("id"))
	message := strings.Clone(request.Message)
	attachment := request.Attachment
	events := make(chan core.ChatEvent, chatEventBuffer)
	observer := &chatStreamObserver{events: events}
	runCtx := core.WithChatObserver(context.Background(), observer)
	runCtx = core.WithCallerAuthorization(runCtx, callerAuthorization(c))
	outcomes, err := service.Start(runCtx, id, message, attachment)
	if errors.Is(err, chatagent.ErrRunActive) {
		auditor(middleware.ChatAuditEvent{Action: chatAuditMessageSent, Target: target, Result: middleware.ChatAuditDenied})
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{"error": "session has an active run"})
	}
	if errors.Is(err, chatagent.ErrSessionNotFound) {
		auditor(middleware.ChatAuditEvent{Action: chatAuditMessageSent, Target: target, Result: middleware.ChatAuditDenied})
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "not found"})
	}
	if errors.Is(err, chatagent.ErrInvalidAttachment) || errors.Is(err, chatagent.ErrInvalidMessage) || errors.Is(err, chatagent.ErrInvalidTime) {
		auditor(middleware.ChatAuditEvent{Action: chatAuditMessageSent, Target: target, Result: middleware.ChatAuditDenied})
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid request"})
	}
	if err != nil {
		auditor(middleware.ChatAuditEvent{Action: chatAuditMessageSent, Target: target, Result: middleware.ChatAuditFailure})
		return chatInternalError(c, "start_chat_run", err)
	}
	auditor(middleware.ChatAuditEvent{Action: chatAuditMessageSent, Target: target, Result: middleware.ChatAuditSuccess})
	apiV1 := isAgentAPIv1(c)
	wantsSSE := !apiV1 || strings.Contains(strings.ToLower(c.Get(fiber.HeaderAccept)), "text/event-stream")
	if !wantsSSE {
		outcome := <-outcomes
		eventResults := drainChatEvents(events)
		close(events)
		if outcome.Err != nil {
			auditor(middleware.ChatAuditEvent{Action: chatAuditMessageResult, Target: target, Result: middleware.ChatAuditFailure})
			return c.Status(fiber.StatusBadGateway).JSON(agentapi.Error{Code: "run_failed", Message: "chat run failed"})
		}
		if outcome.Result == nil {
			auditor(middleware.ChatAuditEvent{Action: chatAuditMessageResult, Target: target, Result: middleware.ChatAuditFailure})
			return c.Status(fiber.StatusBadGateway).JSON(agentapi.Error{Code: "run_failed", Message: "chat run failed"})
		}
		auditor(middleware.ChatAuditEvent{Action: chatAuditMessageResult, Target: target, Result: middleware.ChatAuditSuccess})
		assistant := apiTurnResult(outcome.Result)
		assistant.Events = eventResults
		return c.JSON(agentapi.TurnResponse{SessionID: id, Assistant: assistant})
	}

	go func() {
		defer close(events)
		outcome := <-outcomes
		err := outcome.Err
		if err == nil {
			auditor(middleware.ChatAuditEvent{Action: chatAuditMessageResult, Target: target, Result: middleware.ChatAuditSuccess})
			return
		}
		result := middleware.ChatAuditFailure
		terminal := core.ChatEvent{At: time.Now().UTC(), Kind: core.ChatEventRunFailed, Error: "chat run failed"}
		if errors.Is(err, context.Canceled) {
			result = middleware.ChatAuditCancelled
			terminal.Kind = core.ChatEventRunCancelled
			terminal.Error = "run cancelled"
		} else if errors.Is(err, context.DeadlineExceeded) {
			terminal.Error = "run timed out"
		} else if errors.Is(err, router.ErrRateLimited) {
			result = middleware.ChatAuditThrottled
			terminal.Kind = "run_throttled"
			terminal.Error = "rate limit reached; retry next hour"
		}
		if errors.Is(err, chatagent.ErrRunActive) {
			terminal.Error = "session has an active run"
		}
		auditor(middleware.ChatAuditEvent{Action: chatAuditMessageResult, Target: target, Result: result})
		observer.sendTerminal(terminal)
	}()

	c.Set(fiber.HeaderContentType, "text/event-stream")
	c.Set(fiber.HeaderCacheControl, "no-cache")
	c.Set(fiber.HeaderConnection, "keep-alive")
	c.Set("X-Accel-Buffering", "no")
	c.Context().SetBodyStreamWriter(func(writer *bufio.Writer) {
		for event := range events {
			value := any(event)
			if apiV1 {
				value = apiEvent(event)
			}
			encoded, err := json.Marshal(value)
			if err != nil || writeSSE(writer, event.Kind, encoded) != nil || writer.Flush() != nil {
				break
			}
		}
		for range events {
		}
	})
	return nil
}

func (controller *ChatAdminController) cancel(c *fiber.Ctx) error {
	auditor := middleware.ChatAuditor(c)
	target := chatAuditTarget(c.Params("id"))
	service := controller.service(c)
	if service == nil {
		auditor(middleware.ChatAuditEvent{Action: chatAuditRunCancelled, Target: target, Result: middleware.ChatAuditDenied})
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "chat not enabled"})
	}
	err := service.Cancel(c.Params("id"))
	if errors.Is(err, chatagent.ErrNoActiveRun) {
		auditor(middleware.ChatAuditEvent{Action: chatAuditRunCancelled, Target: target, Result: middleware.ChatAuditDenied})
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{"error": "no active run"})
	}
	if err != nil {
		auditor(middleware.ChatAuditEvent{Action: chatAuditRunCancelled, Target: target, Result: middleware.ChatAuditFailure})
		return chatInternalError(c, "cancel_run", err)
	}
	auditor(middleware.ChatAuditEvent{Action: chatAuditRunCancelled, Target: target, Result: middleware.ChatAuditSuccess})
	return c.SendStatus(fiber.StatusAccepted)
}

type chatStreamObserver struct {
	events     chan core.ChatEvent
	terminal   atomic.Bool
	terminalMu sync.Mutex
}

func (observer *chatStreamObserver) OnChatEvent(event core.ChatEvent) {
	if isTerminalChatEvent(event.Kind) {
		observer.sendTerminal(event)
		return
	}
	if event.Kind == core.ChatEventApproval {
		observer.sendPriority(event)
		return
	}
	observer.terminalMu.Lock()
	defer observer.terminalMu.Unlock()
	if observer.terminal.Load() || len(observer.events) >= cap(observer.events)-1 {
		return
	}
	select {
	case observer.events <- event:
	default:
	}
}

func (observer *chatStreamObserver) sendPriority(event core.ChatEvent) {
	observer.terminalMu.Lock()
	defer observer.terminalMu.Unlock()
	if observer.terminal.Load() {
		return
	}
	if len(observer.events) >= cap(observer.events)-1 {
		retained := make([]core.ChatEvent, 0, len(observer.events))
	drainPriority:
		for len(observer.events) > 0 {
			select {
			case queued := <-observer.events:
				if queued.Kind == core.ChatEventApproval {
					retained = append(retained, queued)
				}
			default:
				break drainPriority
			}
			if len(observer.events) == 0 {
				break
			}
		}
		for _, queued := range retained {
			observer.events <- queued
		}
	}
	select {
	case observer.events <- event:
	default:
	}
}

func (observer *chatStreamObserver) sendTerminal(event core.ChatEvent) {
	observer.terminalMu.Lock()
	defer observer.terminalMu.Unlock()
	if observer.terminal.Load() {
		return
	}
	if len(observer.events) >= cap(observer.events) {
		retained := make([]core.ChatEvent, 0, len(observer.events))
	drainTerminal:
		for len(observer.events) > 0 {
			select {
			case queued := <-observer.events:
				if queued.Kind == core.ChatEventApproval {
					retained = append(retained, queued)
				}
			default:
				break drainTerminal
			}
			if len(observer.events) == 0 {
				break
			}
		}
		for _, queued := range retained {
			observer.events <- queued
		}
	}
	select {
	case observer.events <- event:
		observer.terminal.Store(true)
	default:
	}
}

func drainChatEvents(events <-chan core.ChatEvent) []agentapi.Event {
	result := make([]agentapi.Event, 0)
	for {
		select {
		case event, open := <-events:
			if !open {
				return result
			}
			result = append(result, apiEvent(event))
		default:
			return result
		}
	}
}

func isTerminalChatEvent(kind string) bool {
	return kind == core.ChatEventRunFinished || kind == core.ChatEventRunFailed || kind == core.ChatEventRunCancelled || kind == "run_throttled"
}

func writeSSE(writer *bufio.Writer, event string, data []byte) error {
	if _, err := fmt.Fprintf(writer, "event: %s\n", event); err != nil {
		return err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if _, err := fmt.Fprintf(writer, "data: %s\n", line); err != nil {
			return err
		}
	}
	_, err := writer.WriteString("\n")
	return err
}

func hydrateIncidentAttachment(c *fiber.Ctx, attachment *core.ChatAttachment) error {
	if attachment == nil || attachment.Incident == nil || attachment.Incident.ID == "" {
		return nil
	}
	store := services.Storage()
	if store == nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "storage not configured"})
	}
	record, err := store.GetIncident(attachment.Incident.ID)
	if errors.Is(err, storage.ErrNotFound) || (err == nil && record == nil) || (record != nil && storage.NormalizeOrgID(record.OrgID) != middleware.OrgFromContext(c)) {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "incident not found"})
	}
	if err != nil {
		return chatInternalError(c, "get incident attachment", err)
	}
	status := "open"
	if record.Resolved {
		status = "resolved"
	} else if record.AckedAt != nil {
		status = "acknowledged"
	}
	attachment.Incident = &core.ChatIncidentContext{
		ID: record.ID, Title: record.Title, Service: record.Service, Status: status, Created: record.CreatedAt,
	}
	return nil
}

func chatInternalError(c *fiber.Ctx, operation string, err error) error {
	log.Printf("chat admin failure: operation=%q request_id=%q code=%s", operation, chatAuditTarget(c.Get("X-Request-ID")), chatErrorCode(err))
	return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": internalServerErrorMessage})
}

func chatErrorCode(err error) string {
	switch {
	case errors.Is(err, chatagent.ErrSessionNotFound):
		return "not_found"
	case errors.Is(err, chatagent.ErrStoreConflict):
		return "storage_conflict"
	case errors.Is(err, chatagent.ErrSessionTooLarge):
		return "size_limit"
	case errors.Is(err, router.ErrRateLimited):
		return "rate_limited"
	default:
		return "backend_failure"
	}
}

func chatAuditTarget(value string) string {
	value = strings.ReplaceAll(strings.ReplaceAll(strings.ToValidUTF8(value, ""), "\r", ""), "\n", "")
	for len(value) > 128 {
		_, size := utf8.DecodeLastRuneInString(value)
		value = value[:len(value)-size]
	}
	return strings.Clone(value)
}

func apiSession(session *chatagent.Session) agentapi.Session {
	result := agentapi.Session{
		ID: session.ID, Status: string(session.Status), Seeded: session.Seeded,
		CreatedAt: session.CreatedAt, UpdatedAt: session.UpdatedAt,
		Turns: make([]agentapi.Turn, 0, len(session.Turns)),
	}
	for _, turn := range session.Turns {
		mapped := agentapi.Turn{
			ID: turn.ID, Role: string(turn.Role), Content: turn.Content, CreatedAt: turn.CreatedAt,
			Attachment: apiAttachment(turn.Attachment),
			ToolCalls:  make([]agentapi.ToolCall, 0, len(turn.ToolCalls)),
			Citations:  make([]agentapi.Citation, 0, len(turn.Citations)),
			Events:     make([]agentapi.Event, 0, len(turn.Events)),
		}
		for _, call := range turn.ToolCalls {
			mapped.ToolCalls = append(mapped.ToolCalls, agentapi.ToolCall{CallID: call.CallID, Name: call.Name, Args: call.Args, Output: call.Output})
		}
		for _, citation := range turn.Citations {
			mapped.Citations = append(mapped.Citations, agentapi.Citation{Tool: citation.Tool, Label: citation.Label, Locator: citation.Locator})
		}
		for _, event := range turn.Events {
			mapped.Events = append(mapped.Events, apiEvent(event))
		}
		result.Turns = append(result.Turns, mapped)
	}
	return result
}

func apiAttachment(attachment *core.ChatAttachment) *agentapi.Attachment {
	if attachment == nil {
		return nil
	}
	encoded, err := json.Marshal(attachment)
	if err != nil {
		return nil
	}
	var mapped agentapi.Attachment
	if json.Unmarshal(encoded, &mapped) != nil {
		return nil
	}
	return &mapped
}

func coreAttachment(attachment *agentapi.Attachment) *core.ChatAttachment {
	if attachment == nil {
		return nil
	}
	encoded, err := json.Marshal(attachment)
	if err != nil {
		return nil
	}
	var mapped core.ChatAttachment
	if json.Unmarshal(encoded, &mapped) != nil {
		return nil
	}
	return &mapped
}

func apiTurnResult(result *core.ChatTurnResult) agentapi.TurnResult {
	mapped := agentapi.TurnResult{Content: result.Markdown, Duration: result.DurationMs}
	for _, citation := range result.Citations {
		mapped.Citations = append(mapped.Citations, agentapi.Citation{Tool: citation.Tool, Label: citation.Label, Locator: citation.Locator})
	}
	for _, call := range result.ToolCalls {
		mapped.ToolCalls = append(mapped.ToolCalls, agentapi.ToolCall{CallID: call.CallID, Name: call.Name, Args: call.Args, Output: call.Output})
	}
	return mapped
}

func isAgentAPIv1(c *fiber.Ctx) bool {
	return strings.Contains(c.Path(), "/v1/agent/")
}

func apiEvent(event core.ChatEvent) agentapi.Event {
	mapped := agentapi.Event{
		Seq: event.Seq, At: event.At, Kind: event.Kind, Delta: event.Delta,
		Tool: event.Tool, CallID: event.CallID, Args: event.Args,
		Output: event.Output, Duration: event.DurationMs,
	}
	if event.Approval != nil {
		mapped.Approval = &agentapi.Approval{
			ID: event.Approval.ID, ProposalID: event.Approval.ProposalID, RunID: event.Approval.RunID,
			Type: event.Approval.Type, Target: event.Approval.Target, Effect: event.Approval.Effect,
			Risk: event.Approval.Risk, State: event.Approval.State, ExpiresAt: event.Approval.ExpiresAt,
		}
		mapped.ApprovalNonce = event.ApprovalNonce
	}
	if event.Error != "" {
		failure := agentapi.Error{Code: "run_failed", Message: "chat run failed"}
		switch event.Kind {
		case core.ChatEventRunCancelled:
			failure.Code, failure.Message = "run_cancelled", "run cancelled"
		case "run_throttled":
			failure.Code, failure.Message = "rate_limited", "rate limit reached; retry later"
		}
		mapped.Error = &failure
	}
	return mapped
}
