package breaklogs

import "testing"

func TestLevelJSONAndLeadingTokens(t *testing.T) {
	for text, want := range map[string]string{`{"level":"ERROR"}`: "error", `{"severity":"warn"}`: "warning", "[ERROR] failed": "error", "warn: slow": "warning", "debug trace": "debug", "an error later": "info", `{"level":"unknown","severity":"critical"}`: "error"} {
		if got := Level(text); got != want {
			t.Errorf("%q: %s != %s", text, got, want)
		}
	}
}
