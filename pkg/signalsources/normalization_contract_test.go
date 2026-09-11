package signalsources

import (
	"testing"

	"github.com/VersusControl/versus-incident/pkg/core"
)

func assertNormalizedSignal(t *testing.T, signal core.Signal, source string) {
	t.Helper()
	if signal.Source != source || signal.Timestamp.IsZero() || signal.Message == "" || signal.Raw == nil {
		t.Fatalf("signal does not satisfy normalized contract: %#v", signal)
	}
}
