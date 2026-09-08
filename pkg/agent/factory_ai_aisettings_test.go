package agent

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"testing"
	"time"

	einowrap "github.com/VersusControl/versus-incident/pkg/agent/ai/eino"
	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

func newBuildCatalog(t *testing.T) (*Catalog, storage.Provider) {
	t.Helper()
	store := storage.NewMemory()
	cat, err := LoadCatalog(store)
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	return cat, store
}

// TestBuildAIs_ResolverConstructsIdleBundle proves item-3 constructibility:
// an off-at-boot binary (cfg.AI.Enable=false) with a model configured AND a
// registered AISettingsResolver still builds the bundle, so the runtime
// enable flag has an idle Detect agent to switch on.
func TestBuildAIs_ResolverConstructsIdleBundle(t *testing.T) {
	t.Cleanup(func() { SetAISettingsResolver(nil) })
	cat, store := newBuildCatalog(t)

	cfg := config.AgentConfig{
		AI: config.AgentAIConfig{
			Enable: false,
			Model:  "gpt-4o-mini",
		},
	}

	SetAISettingsResolver(&stubAISettings{})
	bundle := BuildAIs(cfg, cat, store, nil)
	if bundle.Detect == nil {
		t.Fatal("Detect = nil; want a constructed (idle) detect agent when a resolver is registered + model configured")
	}
}

// TestBuildAIs_NoResolver_ZeroBundle proves the OSS path is unchanged: with
// no resolver and Enable=false the bundle is zero, exactly as before.
func TestBuildAIs_NoResolver_ZeroBundle(t *testing.T) {
	SetAISettingsResolver(nil)
	t.Cleanup(func() { SetAISettingsResolver(nil) })
	cat, store := newBuildCatalog(t)

	cfg := config.AgentConfig{
		AI: config.AgentAIConfig{Enable: false, Model: "gpt-4o-mini"},
	}

	bundle := BuildAIs(cfg, cat, store, nil)
	if bundle.Detect != nil || bundle.Router != nil {
		t.Fatalf("want zero bundle in OSS (Enable=false, no resolver), got Detect=%v Router=%v", bundle.Detect, bundle.Router)
	}
}

// TestBuildAIs_ResolverButNoModel_ZeroBundle proves we never build a
// nil-key client: a resolver is registered but no model is configured, so
// the bundle stays zero rather than constructing a client that only errors
// at call time.
func TestBuildAIs_ResolverButNoModel_ZeroBundle(t *testing.T) {
	t.Cleanup(func() { SetAISettingsResolver(nil) })
	cat, store := newBuildCatalog(t)

	cfg := config.AgentConfig{
		AI: config.AgentAIConfig{Enable: false}, // Model empty
	}

	SetAISettingsResolver(&stubAISettings{})
	bundle := BuildAIs(cfg, cat, store, nil)
	if bundle.Detect != nil {
		t.Fatal("Detect != nil; want zero bundle when no model is configured (avoid nil-key client)")
	}
}

// TestBuildAIs_EnabledConstructs proves the pre-seam happy path is intact:
// Enable=true + model configured builds the detect agent regardless of any
// resolver.
func TestBuildAIs_EnabledConstructs(t *testing.T) {
	SetAISettingsResolver(nil)
	t.Cleanup(func() { SetAISettingsResolver(nil) })
	cat, store := newBuildCatalog(t)

	cfg := config.AgentConfig{
		AI: config.AgentAIConfig{Enable: true, Model: "gpt-4o-mini"},
	}

	bundle := BuildAIs(cfg, cat, store, nil)
	if bundle.Detect == nil {
		t.Fatal("Detect = nil; want a detect agent when AI is enabled at boot")
	}
}

func TestBuildAIsForScopeWithChatLocationUsesProvider(t *testing.T) {
	SetAISettingsResolver(nil)
	t.Cleanup(func() { SetAISettingsResolver(nil) })
	cat, store := newBuildCatalog(t)
	cfg := config.AgentConfig{AI: config.AgentAIConfig{Enable: true, Model: "gpt-4o-mini"}}
	calls := 0
	bundle := BuildAIsForScopeWithChatLocation(cfg, cat, store, tenancy.DefaultOrgScope(), nil, func() *time.Location {
		calls++
		return time.UTC
	})
	if bundle.ChatService == nil {
		t.Fatal("ChatService = nil")
	}
	service := bundle.ChatService(tenancy.DefaultOrgScope())
	if service == nil {
		t.Fatal("scoped chat service = nil")
	}
	if _, err := service.Send(context.Background(), "missing", "what happened today?", nil); err == nil {
		t.Fatal("missing session send succeeded")
	}
	if calls != 1 {
		t.Fatalf("location provider calls = %d, want 1", calls)
	}
}

func TestAIConstructionFailureLogDoesNotRetainRawProviderCause(t *testing.T) {
	const secret = "reflected-runtime-secret"
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	t.Cleanup(func() { log.SetOutput(previous) })

	for _, task := range []string{"detect", "chat"} {
		logAIConstructionFailure(task, config.AgentAIConfig{Provider: "gemini", Model: "gemini-test"}, errors.New("status 401: key="+secret+"\r\nforged=true"))
	}
	got := output.String()
	if strings.Contains(got, secret) || strings.Contains(got, "forged=true") {
		t.Fatalf("construction log retained raw provider cause: %q", got)
	}
	for _, field := range []string{`detect agent disabled`, `chat agent disabled`, `provider="gemini"`, `model="gemini-test"`, `class="authentication"`} {
		if !strings.Contains(got, field) {
			t.Errorf("construction log %q missing %q", got, field)
		}
	}
}

func TestAIConstructionFailureLogExplainsTrustedConfigurationErrors(t *testing.T) {
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	t.Cleanup(func() { log.SetOutput(previous) })

	_, unsupportedErr := einowrap.NewChatModel(context.Background(), config.AgentAIConfig{Provider: "unknown", Model: "model"}, einowrap.Options{})
	_, emptyModelErr := einowrap.NewChatModel(context.Background(), config.AgentAIConfig{Provider: "openai"}, einowrap.Options{})
	_, embedderErr := einowrap.NewEmbedder(context.Background(), config.AgentAIConfig{Provider: "claude", Model: "embedding"}, einowrap.Options{})
	for _, constructionErr := range []error{unsupportedErr, emptyModelErr, embedderErr} {
		var configErr *einowrap.ConfigError
		if !errors.As(constructionErr, &configErr) {
			t.Fatalf("construction error = %T, want *eino.ConfigError", constructionErr)
		}
	}
	logAIConstructionFailure("detect", config.AgentAIConfig{}, unsupportedErr)
	logAIConstructionFailure("analyze", config.AgentAIConfig{}, emptyModelErr)
	logAIConstructionFailure("find_runbook", config.AgentAIConfig{}, embedderErr)

	got := output.String()
	for _, want := range []string{
		`configuration error: eino: unsupported ai provider "unknown"`,
		`supported: claude, deepseek, gemini, ollama, openai, qwen`,
		`configuration error: eino: model is empty`,
		`unsupported ai provider "claude" for embeddings`,
		`supported: gemini, ollama, openai`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("construction diagnostics %q missing %q", got, want)
		}
	}
}
