package agent

import (
	"bytes"
	"context"
	"log"
	"strings"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// captureLog redirects the standard logger into a buffer for the duration of a
// test and returns everything written.
func captureLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	prevOut := log.Writer()
	prevFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	})
	fn()
	return buf.String()
}

// forgedServiceWorker builds a worker whose service matcher captures anything
// between svc[ and ], newlines included, so a log line can carry a service
// label an attacker chose in full.
func forgedServiceWorker(t *testing.T, src core.SignalSource) *Worker {
	t.Helper()
	cat, err := LoadCatalog(storage.NewMemory())
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	m, errs := NewRegexMatcher(config.AgentRegexConfig{DefaultPattern: ".*"})
	if len(errs) > 0 {
		t.Fatalf("NewRegexMatcher: %v", errs)
	}
	svc, errs := NewServiceMatcher([]string{`(?s)svc\[(.+?)\]`})
	if len(errs) > 0 {
		t.Fatalf("NewServiceMatcher: %v", errs)
	}
	w, err := NewWorker(WorkerOptions{
		Cfg:      config.AgentConfig{Mode: "training"},
		Sources:  []core.SignalSource{src},
		Matcher:  m,
		Miner:    NewMiner(0.4, 4, 100),
		Catalog:  cat,
		Services: svc,
	})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}
	return w
}

// TestWorker_ServiceLabelCannotForgeLogLine is the log-injection guard for the
// agent tick. The service label, the pattern id and the pattern template are all
// derived from third-party log content, so a newline inside any of them used to
// break the log line in two and let the writer of that log author a whole
// fabricated entry — enough to fake a discovery, a verdict, or an alert in an
// operator's log or SIEM.
func TestWorker_ServiceLabelCannotForgeLogLine(t *testing.T) {
	const forged = "agent: new service discovered: payments-prod"
	signals := []core.Signal{
		{Message: "svc[checkout\n" + forged + "] request failed id=1"},
		{Message: "svc[checkout\n" + forged + "] request failed id=2"},
	}
	src := &batchSource{name: "es", signals: signals}
	w := forgedServiceWorker(t, src)

	out := captureLog(t, func() {
		w.tickSource(context.Background(), src, "training")
	})

	if !strings.Contains(out, "new service discovered") {
		t.Fatalf("the discovery line was not logged at all; the guard would pass vacuously:\n%s", out)
	}
	// The raw newline must never reach the log: every line the attacker's text
	// spans is escaped into the one line that quotes it.
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == forged {
			t.Fatalf("a forged log line was emitted verbatim:\n%s", out)
		}
	}
	if !strings.Contains(out, `\n`+forged) {
		t.Fatalf("the embedded newline was not escaped; want it rendered as \\n:\n%s", out)
	}
}

// TestWorker_PatternTemplateCannotForgeLogLine covers the same class for the
// mined pattern template and pattern id, which the discovery line also carries
// straight from log content.
func TestWorker_PatternTemplateCannotForgeLogLine(t *testing.T) {
	const forged = "agent[detect]: emitted pattern=p1 service=payments severity=critical"
	signals := []core.Signal{
		{Message: "svc[checkout] boom\n" + forged + " id=1"},
		{Message: "svc[checkout] boom\n" + forged + " id=2"},
	}
	src := &batchSource{name: "es", signals: signals}
	w := forgedServiceWorker(t, src)

	out := captureLog(t, func() {
		w.tickSource(context.Background(), src, "training")
	})

	if !strings.Contains(out, "new pattern") {
		t.Fatalf("the new-pattern line was not logged at all; the guard would pass vacuously:\n%s", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "agent[detect]: emitted pattern=") {
			t.Fatalf("a forged log line was emitted verbatim:\n%s", out)
		}
	}
}
