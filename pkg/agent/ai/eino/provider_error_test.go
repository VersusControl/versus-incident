package eino_test

import (
	"errors"
	"strings"
	"testing"

	einowrap "github.com/VersusControl/versus-incident/pkg/agent/ai/eino"
)

func TestSafeProviderErrorClassifiesWithoutRetainingCause(t *testing.T) {
	const secret = "runtime-provider-secret"
	tests := []struct {
		cause string
		class string
	}{
		{"status 401 invalid_api_key " + secret, "authentication"},
		{"status 403 permission denied " + secret, "permission"},
		{"status 404 model_not_found " + secret, "model_not_found"},
		{"status 429 rate_limit " + secret, "rate_limit"},
		{"status 400 invalid_request " + secret, "invalid_request"},
		{"deadline exceeded " + secret, "timeout"},
		{"connection refused " + secret, "connection"},
		{"malformed invalid response " + secret, "validation"},
		{"provider exploded " + secret, "unavailable"},
	}
	for _, test := range tests {
		t.Run(test.class, func(t *testing.T) {
			safe := einowrap.SafeProviderError("claude\r\nforged=true", "model\r\nforged=true", errors.New(test.cause))
			if safe.Class != test.class {
				t.Fatalf("class = %q, want %q", safe.Class, test.class)
			}
			encoded := safe.Provider + safe.Model + safe.Diagnostic + safe.Error()
			if strings.Contains(encoded, secret) || strings.Contains(encoded, "\r") || strings.Contains(encoded, "\n") || strings.Contains(encoded, "forged=true") {
				t.Fatalf("safe provider error retained untrusted input: %+v", safe)
			}
		})
	}
}

func TestSafeProviderErrorRejectsEntireUnsafeIdentifiers(t *testing.T) {
	safe := einowrap.SafeProviderError("gemini\r\nforged", "model/ok\nsecret", errors.New("provider failed"))
	if safe.Provider != "openai" || safe.Model != "configured model" {
		t.Fatalf("unsafe identifiers were partially retained: %+v", safe)
	}
}

func TestSafeProviderErrorActionableResponseClassifications(t *testing.T) {
	empty := einowrap.SafeProviderError("claude", "model", errors.New("AI returned no content"))
	if empty.Class != "empty_response" || !strings.Contains(empty.Diagnostic, "completion-token budget") {
		t.Fatalf("empty response classification = %+v", empty)
	}
	temperature := einowrap.SafeProviderError("claude", "model", errors.New("this model does not support temperature"))
	if temperature.Class != "invalid_request" || !strings.Contains(temperature.Diagnostic, "AGENT_AI_TEMPERATURE=-1") {
		t.Fatalf("temperature classification = %+v", temperature)
	}
}
