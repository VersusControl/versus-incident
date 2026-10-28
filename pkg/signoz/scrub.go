package signoz

import (
	"strings"

	"github.com/VersusControl/versus-incident/pkg/core"
)

const exactSecretReplacement = "[REDACTED:SIGNOZ_API_KEY]"

// ScrubExactSecret removes every occurrence of secret from value.
func ScrubExactSecret(value, secret string) string {
	if secret == "" {
		return value
	}
	return strings.ReplaceAll(value, secret, exactSecretReplacement)
}

// ScrubExactSecretValue returns a fresh copy of JSON-shaped maps and arrays
// with the exact secret removed from every map key and string value.
func ScrubExactSecretValue(value any, secret string) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, child := range typed {
			out[ScrubExactSecret(key, secret)] = ScrubExactSecretValue(child, secret)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for index, child := range typed {
			out[index] = ScrubExactSecretValue(child, secret)
		}
		return out
	case string:
		return ScrubExactSecret(typed, secret)
	default:
		return value
	}
}

// ScrubSignalExactSecret returns a fresh signal with the exact secret removed
// from every provider-controlled string and nested structure.
func ScrubSignalExactSecret(signal core.Signal, secret string) core.Signal {
	signal.Source = ScrubExactSecret(signal.Source, secret)
	signal.Severity = ScrubExactSecret(signal.Severity, secret)
	signal.Message = ScrubExactSecret(signal.Message, secret)
	if signal.Fields != nil {
		signal.Fields = ScrubExactSecretValue(signal.Fields, secret).(map[string]any)
	}
	if signal.Raw != nil {
		signal.Raw = ScrubExactSecretValue(signal.Raw, secret).(map[string]any)
	}
	return signal
}
