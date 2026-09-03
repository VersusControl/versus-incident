package controllers

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	chatagent "github.com/VersusControl/versus-incident/pkg/agent/ai/chat"
	"github.com/VersusControl/versus-incident/pkg/agent/ai/router"
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

var (
	chatFactoryMu sync.RWMutex
	chatFactory   ChatServiceFactory
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

type ChatAdminController struct {
	factory ChatServiceFactory
	mu      sync.Mutex
	byOrg   map[string]*chatagent.Service
}

func NewChatAdminController(factory ChatServiceFactory) *ChatAdminController {
	return &ChatAdminController{factory: factory, byOrg: map[string]*chatagent.Service{}}
}

func (controller *ChatAdminController) Register(router fiber.Router) {
	group := router.Group("/admin/chat", adminGatewayGuard)
	group.Post("/sessions", controller.create)
	group.Get("/sessions", controller.list)
	group.Get("/sessions/:id", controller.get)
	group.Delete("/sessions/:id", controller.delete)
	group.Post("/sessions/:id/messages", controller.message)
	group.Post("/sessions/:id/cancel", controller.cancel)
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
	if err := c.BodyParser(&request); err != nil {
		auditor(middleware.ChatAuditEvent{Action: chatAuditMessageSent, Target: target, Result: middleware.ChatAuditDenied})
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid request"})
	}
	if err := hydrateIncidentAttachment(c, request.Attachment); err != nil {
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
			encoded, err := json.Marshal(event)
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

func (observer *chatStreamObserver) sendTerminal(event core.ChatEvent) {
	observer.terminalMu.Lock()
	defer observer.terminalMu.Unlock()
	if observer.terminal.Load() {
		return
	}
	timer := time.NewTimer(100 * time.Millisecond)
	defer timer.Stop()
	select {
	case observer.events <- event:
		observer.terminal.Store(true)
	case <-timer.C:
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
