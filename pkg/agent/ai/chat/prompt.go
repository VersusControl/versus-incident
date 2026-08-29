// Package chat implements the dedicated conversational DevOps/SRE agent.
package chat

import (
	"embed"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/VersusControl/versus-incident/pkg/agent/ai/prompt"
	"github.com/VersusControl/versus-incident/pkg/core"
)

//go:embed prompts/*.md
var promptFS embed.FS

var promptOrder = []string{
	"prompts/SOUL.md",
	"prompts/INPUTS.md",
	"prompts/OUTPUT.md",
	"prompts/RULES.md",
}

var systemPrompt = prompt.MustAssemble(promptFS, promptOrder)

// SystemPrompt returns the assembled prompt used for chat runs.
func SystemPrompt() string { return systemPrompt }

// PromptOrder returns a defensive copy of the canonical fragment order.
func PromptOrder() []string { return append([]string(nil), promptOrder...) }

func buildUserPrompt(task core.ChatTask) string {
	payload := struct {
		Message    string               `json:"message"`
		Attachment *core.ChatAttachment `json:"attachment,omitempty"`
	}{Message: task.Message, Attachment: task.Attachment}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Sprintf("message: %s", strings.TrimSpace(task.Message))
	}
	return string(encoded)
}
