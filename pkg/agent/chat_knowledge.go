package agent

import (
	"sync"

	versustools "github.com/VersusControl/versus-incident/pkg/agent/ai/tools/versus"
)

// ChatKnowledgeProviders contains optional read-only providers supplied by a
// deployment before its AI bundles are built. Nil providers remain visible to
// chat as explicitly unavailable tools.
type ChatKnowledgeProviders struct {
	ServiceReliability versustools.ServiceReliabilityProvider
	AlertDecision      versustools.AlertDecisionProvider
	CapabilityStatus   versustools.CapabilityStatusProvider
}

var (
	chatKnowledgeMu   sync.Mutex
	chatKnowledgeSlot ChatKnowledgeProviders
)

// SetChatKnowledgeProviders registers optional read-only DevOps knowledge providers.
// Last registration wins; the zero value clears every provider.
func SetChatKnowledgeProviders(providers ChatKnowledgeProviders) {
	chatKnowledgeMu.Lock()
	defer chatKnowledgeMu.Unlock()
	chatKnowledgeSlot = providers
}

func chatKnowledgeProviders() ChatKnowledgeProviders {
	chatKnowledgeMu.Lock()
	defer chatKnowledgeMu.Unlock()
	return chatKnowledgeSlot
}
