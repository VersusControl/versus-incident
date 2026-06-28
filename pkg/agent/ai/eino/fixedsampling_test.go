package eino_test

import (
	"context"
	"sync"
	"testing"

	"github.com/cloudwego/eino/schema"

	einowrap "github.com/VersusControl/versus-incident/pkg/agent/ai/eino"
	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
)

// fixedSamplingFinding is the canned reply the egress server returns for the
// beta-limitation tests; its content is irrelevant, only the OUTBOUND request
// body is asserted.
var fixedSamplingFinding = core.AIFinding{
	Title:    "SLO burn rate exceeded",
	Summary:  "checkout error budget is burning 4x faster than target.",
	Severity: "high",
	Category: "slo",
}

// assertNoFixedSamplingFields fails if the decoded outbound body carries any of
// the parameters an OpenAI beta-limited / reasoning model rejects: temperature,
// top_p, n, presence_penalty, frequency_penalty.
func assertNoFixedSamplingFields(t *testing.T, body map[string]any) {
	t.Helper()
	for _, k := range []string{"temperature", "top_p", "n", "presence_penalty", "frequency_penalty"} {
		if _, ok := body[k]; ok {
			t.Errorf("outbound body must omit %q for a beta-limited model, got %v", k, body[k])
		}
	}
}

// runOpenAIEgress builds an OpenAI chat model (base or tool-calling) against an
// egress-capture server and returns the decoded outbound request body.
func runOpenAIEgress(t *testing.T, cfg config.AgentAIConfig, toolCalling bool) map[string]any {
	t.Helper()
	var (
		mu       sync.Mutex
		seenAuth string
		seenBody map[string]any
	)
	srv := newOpenAICompatServer(t, fixedSamplingFinding, &seenAuth, &seenBody, &mu)
	defer srv.Close()

	ctx := context.Background()
	msgs := []*schema.Message{schema.SystemMessage("system"), schema.UserMessage("user")}
	if toolCalling {
		cm, err := einowrap.NewToolCallingChatModel(ctx, cfg, einowrap.Options{BaseURL: srv.URL})
		if err != nil {
			t.Fatalf("NewToolCallingChatModel: %v", err)
		}
		if _, err := cm.Generate(ctx, msgs); err != nil {
			t.Fatalf("Generate: %v", err)
		}
	} else {
		cm, err := einowrap.NewChatModel(ctx, cfg, einowrap.Options{BaseURL: srv.URL})
		if err != nil {
			t.Fatalf("NewChatModel: %v", err)
		}
		if _, err := cm.Generate(ctx, msgs); err != nil {
			t.Fatalf("Generate: %v", err)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if seenBody == nil {
		t.Fatal("egress server never recorded a request body")
	}
	return seenBody
}

// TestChatModel_OpenAI_ReasoningModelOmitsTemperature is the regression test for
// the slo-advisor failure: an OpenAI beta-limited / reasoning model configured
// with the default temperature: 0.2 must send NO temperature (and no other
// fixed-sampling parameter) so the provider applies its server-side default
// instead of rejecting the request. It covers both the base (detect/slo-advisor)
// and tool-calling (analyze) paths since they share buildOpenAIChatModel.
func TestChatModel_OpenAI_ReasoningModelOmitsTemperature(t *testing.T) {
	models := []string{"o1-mini", "gpt-5.4-mini"}
	for _, modelID := range models {
		for _, toolCalling := range []bool{false, true} {
			name := modelID
			if toolCalling {
				name += "/tool-calling"
			}
			t.Run(name, func(t *testing.T) {
				cfg := config.AgentAIConfig{
					Provider:    "openai",
					APIKey:      "k",
					Model:       modelID,
					Temperature: 0.2,
					MaxTokens:   256,
				}
				body := runOpenAIEgress(t, cfg, toolCalling)
				assertNoFixedSamplingFields(t, body)
			})
		}
	}
}

// TestChatModel_OpenAI_NormalModelSendsTemperature proves the fix is surgical:
// a non-reasoning OpenAI model still sends the resolved temperature verbatim.
func TestChatModel_OpenAI_NormalModelSendsTemperature(t *testing.T) {
	cfg := config.AgentAIConfig{
		Provider:    "openai",
		APIKey:      "k",
		Model:       "gpt-4o-mini",
		Temperature: 0.2,
		MaxTokens:   256,
	}
	body := runOpenAIEgress(t, cfg, false)
	temp, ok := body["temperature"]
	if !ok {
		t.Fatalf("normal model must send temperature, body keys: %v", body)
	}
	if f, _ := temp.(float64); f < 0.19 || f > 0.21 {
		t.Errorf("temperature = %v, want ~0.2", temp)
	}
}

// TestChatModel_OpenAI_NegativeSentinelStillOmits proves the explicit operator
// override is preserved: temperature: -1 on a NON-reasoning model still omits
// the field (the automatic family detection did not subsume the sentinel).
func TestChatModel_OpenAI_NegativeSentinelStillOmits(t *testing.T) {
	cfg := config.AgentAIConfig{
		Provider:    "openai",
		APIKey:      "k",
		Model:       "gpt-4o-mini",
		Temperature: -1,
		MaxTokens:   256,
	}
	body := runOpenAIEgress(t, cfg, false)
	if _, ok := body["temperature"]; ok {
		t.Errorf("negative sentinel must omit temperature, got %v", body["temperature"])
	}
}

// TestChatModel_NonOpenAIProviderUnaffected proves the gate is OpenAI-only: a
// non-OpenAI provider (deepseek) still sends temperature even for a reasoning-
// shaped model id, because the beta-limitation is specific to OpenAI.
func TestChatModel_NonOpenAIProviderUnaffected(t *testing.T) {
	cfg := config.AgentAIConfig{
		Provider:    "deepseek",
		APIKey:      "k",
		Model:       "o1-mini", // reasoning-shaped id, but not OpenAI
		Temperature: 0.2,
		MaxTokens:   256,
	}
	body := runOpenAIEgress(t, cfg, false)
	if _, ok := body["temperature"]; !ok {
		t.Errorf("non-openai provider must be unaffected and still send temperature, body keys: %v", body)
	}
}
