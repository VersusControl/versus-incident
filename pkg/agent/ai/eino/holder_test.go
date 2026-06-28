package eino_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	einoollama "github.com/cloudwego/eino-ext/components/model/ollama"
	einoopenai "github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/components/model"

	einowrap "github.com/VersusControl/versus-incident/pkg/agent/ai/eino"
	"github.com/VersusControl/versus-incident/pkg/config"
)

// providerSwitch is a goroutine-safe runtime-provider override usable as a
// einowrap.RuntimeAI.Provider func. A nil value (override="") reports ok=false
// so the holder keeps the configured provider.
type providerSwitch struct {
	mu       sync.RWMutex
	override string
}

func (p *providerSwitch) set(v string) {
	p.mu.Lock()
	p.override = v
	p.mu.Unlock()
}

func (p *providerSwitch) fn(context.Context) (string, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.override == "" {
		return "", false
	}
	return p.override, true
}

const (
	holderOpenAIModel = "gpt-4o-mini"
)

func baseOpenAICfg() config.AgentAIConfig {
	return config.AgentAIConfig{Provider: "openai", APIKey: "k", Model: holderOpenAIModel, MaxTokens: 64}
}

// TestHolder_NilRuntime_UsesConfigProvider proves the OSS default: a zero
// RuntimeAI pins the configured provider and builds the model exactly once,
// so two Gets return the identical cached model — community behaviour
// unchanged, no per-call rebuild cost.
func TestHolder_NilRuntime_UsesConfigProvider(t *testing.T) {
	h := einowrap.NewChatModelHolder(baseOpenAICfg(), einowrap.Options{}, einowrap.RuntimeAI{})

	first, err := h.Get(context.Background())
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, ok := first.(*einoopenai.ChatModel); !ok {
		t.Fatalf("first model = %T, want *einoopenai.ChatModel", first)
	}

	second, err := h.Get(context.Background())
	if err != nil {
		t.Fatalf("Get(2): %v", err)
	}
	if first != second {
		t.Fatalf("signature unchanged but holder rebuilt: %p != %p", first, second)
	}
}

// TestHolder_ProviderOverride_SwitchesBackend proves a runtime provider
// override rebuilds the model to the new backend on the next Get WITHOUT a
// restart (same holder instance). openai -> ollama.
func TestHolder_ProviderOverride_SwitchesBackend(t *testing.T) {
	sw := &providerSwitch{}
	h := einowrap.NewChatModelHolder(baseOpenAICfg(), einowrap.Options{}, einowrap.RuntimeAI{Provider: sw.fn})

	// No override yet ⇒ configured provider (openai).
	m0, err := h.Get(context.Background())
	if err != nil {
		t.Fatalf("Get(openai): %v", err)
	}
	if _, ok := m0.(*einoopenai.ChatModel); !ok {
		t.Fatalf("model = %T, want *einoopenai.ChatModel", m0)
	}

	// Operator flips the runtime provider to ollama.
	sw.set("ollama")
	m1, err := h.Get(context.Background())
	if err != nil {
		t.Fatalf("Get(ollama): %v", err)
	}
	if _, ok := m1.(*einoollama.ChatModel); !ok {
		t.Fatalf("after switch model = %T, want *einoollama.ChatModel", m1)
	}

	// Flip back to openai (case-insensitively) ⇒ rebuilt openai backend.
	sw.set("OpenAI")
	m2, err := h.Get(context.Background())
	if err != nil {
		t.Fatalf("Get(openai again): %v", err)
	}
	if _, ok := m2.(*einoopenai.ChatModel); !ok {
		t.Fatalf("after switch back model = %T, want *einoopenai.ChatModel", m2)
	}
}

// TestHolder_SignatureUnchanged_NoRebuild proves a steady-state Get with an
// unchanged signature reuses the cached model (identical pointer) — no
// per-call construction cost even with a runtime resolver attached.
func TestHolder_SignatureUnchanged_NoRebuild(t *testing.T) {
	sw := &providerSwitch{}
	sw.set("ollama")
	h := einowrap.NewChatModelHolder(baseOpenAICfg(), einowrap.Options{}, einowrap.RuntimeAI{Provider: sw.fn})

	first, err := h.Get(context.Background())
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	for i := 0; i < 5; i++ {
		got, err := h.Get(context.Background())
		if err != nil {
			t.Fatalf("Get(%d): %v", i, err)
		}
		if got != first {
			t.Fatalf("Get(%d) rebuilt despite unchanged signature: %p != %p", i, got, first)
		}
	}
}

// TestHolder_UnknownRuntimeProvider_FailsClosed proves an unknown runtime
// provider does NOT crash and does NOT switch the backend: the holder keeps
// the configured provider (openai) and returns a healthy model.
func TestHolder_UnknownRuntimeProvider_FailsClosed(t *testing.T) {
	sw := &providerSwitch{}
	sw.set("totally-not-a-provider")
	h := einowrap.NewChatModelHolder(baseOpenAICfg(), einowrap.Options{}, einowrap.RuntimeAI{Provider: sw.fn})

	m, err := h.Get(context.Background())
	if err != nil {
		t.Fatalf("Get with unknown runtime provider returned error (must fail closed): %v", err)
	}
	if _, ok := m.(*einoopenai.ChatModel); !ok {
		t.Fatalf("fail-closed model = %T, want configured *einoopenai.ChatModel", m)
	}

	// A subsequent valid override still takes effect (the bad value did not
	// poison the holder).
	sw.set("ollama")
	m2, err := h.Get(context.Background())
	if err != nil {
		t.Fatalf("Get(ollama after bad value): %v", err)
	}
	if _, ok := m2.(*einoollama.ChatModel); !ok {
		t.Fatalf("recovered model = %T, want *einoollama.ChatModel", m2)
	}
}

// TestHolder_KeyStateRebuild proves the runtime key-presence STATE is folded
// into the signature: a set⟷clear transition rebuilds the model even though
// the provider is unchanged (the key VALUE itself still hot-reloads through
// the transport without a rebuild).
func TestHolder_KeyStateRebuild(t *testing.T) {
	var keySet atomic.Bool
	rt := einowrap.RuntimeAI{
		KeySet: func(context.Context) (bool, bool) { return keySet.Load(), true },
	}
	h := einowrap.NewChatModelHolder(baseOpenAICfg(), einowrap.Options{}, rt)

	m0, err := h.Get(context.Background())
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	keySet.Store(true)
	m1, err := h.Get(context.Background())
	if err != nil {
		t.Fatalf("Get(keyset): %v", err)
	}
	if m0 == m1 {
		t.Fatalf("key-state transition did not rebuild the model")
	}
}

// TestHolder_ConcurrentGet_Safe exercises the holder under parallel access
// while an operator flips the provider, proving the mutex-guarded rebuild is
// race-free (run with -race). Every Get must return a non-nil model of a
// supported concrete type.
func TestHolder_ConcurrentGet_Safe(t *testing.T) {
	sw := &providerSwitch{}
	h := einowrap.NewChatModelHolder(baseOpenAICfg(), einowrap.Options{}, einowrap.RuntimeAI{Provider: sw.fn})

	// Flipper toggles the provider while readers hammer Get.
	stop := make(chan struct{})
	go func() {
		toggle := false
		for {
			select {
			case <-stop:
				return
			default:
				if toggle {
					sw.set("ollama")
				} else {
					sw.set("openai")
				}
				toggle = !toggle
			}
		}
	}()

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				m, err := h.Get(context.Background())
				if err != nil {
					t.Errorf("concurrent Get: %v", err)
					return
				}
				switch m.(type) {
				case *einoopenai.ChatModel, *einoollama.ChatModel:
				default:
					t.Errorf("concurrent Get returned unexpected backend %T", m)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(stop)
}

// TestEmbedderHolder_FailsClosedToEmbeddingRegistry proves the embedder holder
// validates runtime providers against the (smaller) embedding registry: a
// runtime switch to a chat-only provider (deepseek has no embedder) fails
// closed to the configured embedding provider rather than erroring.
func TestEmbedderHolder_FailsClosedToEmbeddingRegistry(t *testing.T) {
	sw := &providerSwitch{}
	sw.set("deepseek") // valid chat provider, NOT an embedding provider
	cfg := config.AgentAIConfig{Provider: "openai", APIKey: "k", Model: "text-embedding-3-small"}
	h := einowrap.NewEmbedderHolder(cfg, einowrap.Options{}, einowrap.RuntimeAI{Provider: sw.fn})

	if _, err := h.Get(context.Background()); err != nil {
		t.Fatalf("embedder holder must fail closed (not error) on chat-only runtime provider: %v", err)
	}

	// ollama IS an embedding provider, so it switches.
	sw.set("ollama")
	if _, err := h.Get(context.Background()); err != nil {
		t.Fatalf("Get(ollama embedder): %v", err)
	}
}

// compile-time guard: the shared chat holders satisfy the documented artifact
// types the detect/analyze/slo-advisor paths depend on.
var (
	_ *einowrap.Holder[model.BaseChatModel]        = einowrap.NewChatModelHolder(config.AgentAIConfig{}, einowrap.Options{}, einowrap.RuntimeAI{})
	_ *einowrap.Holder[model.ToolCallingChatModel] = einowrap.NewToolCallingChatModelHolder(config.AgentAIConfig{}, einowrap.Options{}, einowrap.RuntimeAI{})
)
