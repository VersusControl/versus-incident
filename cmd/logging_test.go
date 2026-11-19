package main

import (
	"bytes"
	"log"
	"log/slog"
	"strings"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/logging"
)

// captureStartupLog points the standard logger at a buffer and restores the
// logger and the leveled-logging level when the test ends.
func captureStartupLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	oldOut, oldFlags, oldPrefix := log.Writer(), log.Flags(), log.Prefix()
	oldLevel := logging.Level()
	buf := &bytes.Buffer{}
	log.SetOutput(buf)
	log.SetFlags(0)
	log.SetPrefix("")
	t.Cleanup(func() {
		log.SetOutput(oldOut)
		log.SetFlags(oldFlags)
		log.SetPrefix(oldPrefix)
		logging.Setup(logging.LevelName(oldLevel))
	})
	return buf
}

func TestConfigureLoggingAppliesLogLevel(t *testing.T) {
	cases := []struct {
		env       string
		wantLevel slog.Level
		wantStart string
		wantDebug bool
	}{
		{env: "debug", wantLevel: slog.LevelDebug, wantStart: "logging: level=debug\n", wantDebug: true},
		{env: "", wantLevel: slog.LevelInfo, wantStart: "logging: level=info\n", wantDebug: false},
		{env: "info", wantLevel: slog.LevelInfo, wantStart: "logging: level=info\n", wantDebug: false},
	}
	for _, tc := range cases {
		t.Run("LOG_LEVEL="+tc.env, func(t *testing.T) {
			t.Setenv("LOG_LEVEL", tc.env)
			buf := captureStartupLog(t)

			configureLogging()
			if got := logging.Level(); got != tc.wantLevel {
				t.Fatalf("level = %v, want %v", got, tc.wantLevel)
			}
			if got := buf.String(); got != tc.wantStart {
				t.Fatalf("startup wrote %q, want %q", got, tc.wantStart)
			}

			buf.Reset()
			logging.Debugf("probe: debug detail")
			if got := strings.Contains(buf.String(), "DEBUG probe: debug detail"); got != tc.wantDebug {
				t.Fatalf("debug line shown = %v, want %v (output %q)", got, tc.wantDebug, buf.String())
			}
		})
	}
}
