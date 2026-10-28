package eino

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
)

const maxProviderIdentifierRunes = 96

// ConfigError is a trusted local validation error produced before a provider
// SDK is called. Its message contains only bounded configuration identifiers
// and fixed guidance, so boot diagnostics may log it directly.
type ConfigError struct {
	message string
}

func (err *ConfigError) Error() string {
	if err == nil {
		return "eino: invalid AI configuration"
	}
	return err.message
}

func emptyModelConfigError(embedding bool) *ConfigError {
	if embedding {
		return &ConfigError{message: "eino: embedding model is empty"}
	}
	return &ConfigError{message: "eino: model is empty"}
}

func unsupportedProviderConfigError(provider string, embedding bool, supported []string) *ConfigError {
	provider = safeProviderIdentifier(provider, "invalid value")
	if embedding {
		return &ConfigError{message: fmt.Sprintf("eino: unsupported ai provider %q for embeddings (supported: %s)", provider, strings.Join(supported, ", "))}
	}
	return &ConfigError{message: fmt.Sprintf("eino: unsupported ai provider %q (supported: %s)", provider, strings.Join(supported, ", "))}
}

// ProviderError is the safe, public representation of a model-provider
// failure. It deliberately does not retain or unwrap the raw provider cause.
type ProviderError struct {
	Provider   string
	Model      string
	Class      string
	Diagnostic string
}

func (err *ProviderError) Error() string {
	if err == nil {
		return "AI provider unavailable"
	}
	return err.Diagnostic
}

// SafeProviderError classifies a raw provider failure in memory and returns a
// bounded error containing only safe identifiers and a fixed diagnostic.
func SafeProviderError(provider, model string, cause error) *ProviderError {
	provider = safeProviderIdentifier(provider, "openai")
	model = safeProviderIdentifier(model, "configured model")
	class := providerErrorClass(cause)
	rawClassText := ""
	if cause != nil {
		rawClassText = strings.ToLower(cause.Error())
	}
	label := strings.ToUpper(provider[:1]) + provider[1:]
	diagnostic := fmt.Sprintf("%s could not produce a response with model %q; verify provider configuration and model access.", label, model)
	switch class {
	case "authentication":
		diagnostic = fmt.Sprintf("%s authentication failed for model %q; verify the configured API key.", label, model)
	case "permission":
		diagnostic = fmt.Sprintf("%s denied access to model %q; verify model permissions.", label, model)
	case "model_not_found":
		diagnostic = fmt.Sprintf("%s model %q was not found or is unavailable to this account.", label, model)
	case "rate_limit":
		diagnostic = fmt.Sprintf("%s rate limit or quota was reached for model %q; retry later or check provider limits.", label, model)
	case "invalid_request":
		if strings.Contains(rawClassText, "temperature") && containsAny(rawClassText, "deprecated", "unsupported", "not support", "does not support") {
			diagnostic = fmt.Sprintf("%s model %q rejected the configured temperature; set AGENT_AI_TEMPERATURE=-1 to omit it, restart Versus, and retry.", label, model)
		} else {
			diagnostic = fmt.Sprintf("%s rejected the request for model %q as invalid; verify model compatibility and token limits.", label, model)
		}
	case "empty_response":
		diagnostic = fmt.Sprintf("%s model %q returned no content; verify model access and increase the completion-token budget, then retry.", label, model)
	case "timeout":
		diagnostic = fmt.Sprintf("%s timed out while calling model %q.", label, model)
	case "connection":
		diagnostic = fmt.Sprintf("Could not connect securely to %s for model %q.", label, model)
	case "validation":
		diagnostic = fmt.Sprintf("%s returned an invalid response for model %q.", label, model)
	case "unavailable":
		diagnostic = fmt.Sprintf("%s could not produce a response with model %q; verify provider configuration and model access.", label, model)
	}
	return &ProviderError{Provider: provider, Model: model, Class: class, Diagnostic: diagnostic}
}

func providerErrorClass(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	var networkError net.Error
	if errors.As(err, &networkError) {
		if networkError.Timeout() {
			return "timeout"
		}
		return "connection"
	}
	message := ""
	if err != nil {
		message = strings.ToLower(err.Error())
	}
	switch {
	case containsAny(message, "empty_response", "empty response", "no content"):
		return "empty_response"
	case strings.Contains(message, "temperature") && containsAny(message, "deprecated", "unsupported", "not support", "does not support"):
		return "invalid_request"
	case containsAny(message, "401", "unauthorized", "unauthenticated", "authentication", "invalid_api_key", "invalid api key"):
		return "authentication"
	case containsAny(message, "403", "forbidden", "permission denied"):
		return "permission"
	case containsAny(message, "404", "model_not_found", "model not found", "not_found_error"):
		return "model_not_found"
	case containsAny(message, "429", "rate limit", "rate_limit", "quota"):
		return "rate_limit"
	case containsAny(message, "400", "invalid_request", "bad request", "unsupported", "deprecated"):
		return "invalid_request"
	case containsAny(message, "timeout", "deadline exceeded"):
		return "timeout"
	case containsAny(message, "connection refused", "connection reset", "no such host", "tls", "dial tcp"):
		return "connection"
	case containsAny(message, "decode", "malformed", "invalid response", "validation"):
		return "validation"
	default:
		return "unavailable"
	}
}

func containsAny(value string, candidates ...string) bool {
	for _, candidate := range candidates {
		if strings.Contains(value, candidate) {
			return true
		}
	}
	return false
}

func safeProviderIdentifier(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	count := 0
	for _, r := range value {
		count++
		if count > maxProviderIdentifierRunes || !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || strings.ContainsRune("._:/-", r)) {
			return fallback
		}
	}
	return value
}
