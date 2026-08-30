package middleware

import (
	"sync/atomic"

	"github.com/gofiber/fiber/v2"
)

const (
	ChatAuditSuccess   = "success"
	ChatAuditFailure   = "failure"
	ChatAuditDenied    = "denied"
	ChatAuditCancelled = "cancelled"
	ChatAuditThrottled = "throttled"
)

type ChatAuditEvent struct {
	Action string
	Target string
	Result string
}

type ChatAuditRecorder func(ChatAuditEvent)

type ChatAuditHook func(*fiber.Ctx) ChatAuditRecorder

var chatAuditSlot atomic.Value

func SetChatAuditHook(hook ChatAuditHook) {
	if hook == nil {
		chatAuditSlot = atomic.Value{}
		return
	}
	chatAuditSlot.Store(hook)
}

func ChatAuditor(c *fiber.Ctx) ChatAuditRecorder {
	if hook, ok := chatAuditSlot.Load().(ChatAuditHook); ok && hook != nil {
		return hook(c)
	}
	return func(ChatAuditEvent) {}
}
