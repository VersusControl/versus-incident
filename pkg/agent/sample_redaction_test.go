package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/agent/ai/detect"
	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// learnOneSample drives a log brain through one Group+Learn tick over a single
// signal (using the real pipeline redactor) and returns the catalog + the
// learned pattern key, so a test can inspect the stored redacted sample ring.
// A nil matcher disables the regex pre-filter (every non-empty Message learns);
// a nil service matcher makes Extract yield "" → "_unknown".
func learnOneSample(t *testing.T, red core.Scrubber, sig core.Signal) (*Catalog, string) {
	t.Helper()
	cat, err := LoadCatalog(storage.NewMemory())
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	b := newLogBrain("src", NewMiner(0.4, 4, 100), cat, nil, nil, 0.2, config.AgentCatalogConfig{}, red)
	ctx := context.Background()
	obs, err := b.Group(ctx, []core.Signal{sig})
	if err != nil {
		t.Fatalf("Group: %v", err)
	}
	if len(obs) != 1 {
		t.Fatalf("Group returned %d observations, want 1", len(obs))
	}
	if err := b.Learn(ctx, obs); err != nil {
		t.Fatalf("Learn: %v", err)
	}
	return cat, obs[0].Key
}

// TestSampleRing_SecretOnlyInRaw_NeverStoredOrPrompted is the redaction-guarantee
// gate (design §2): a secret planted ONLY in Signal.Raw — with a clean Message,
// exactly the post-redaction shape the worker produces — must reach NEITHER the
// stored Pattern.Samples NOR the detect prompt. It proves Signal.Raw is never a
// source for the sample ring: the ring is built from the redacted Message only.
func TestSampleRing_SecretOnlyInRaw_NeverStoredOrPrompted(t *testing.T) {
	red, errs := NewRedactor(false, nil)
	if len(errs) != 0 {
		t.Fatalf("NewRedactor: %v", errs)
	}
	sig := core.Signal{
		Source:  "src",
		Message: "db connection refused for the reporting worker",
		Raw: map[string]interface{}{
			"raw_line": "db connection refused password=" + rawOnlySecret,
			"headers":  map[string]interface{}{"authorization": "Bearer " + rawOnlySecret},
		},
	}
	cat, key := learnOneSample(t, red, sig)

	p := cat.Get(key)
	if p == nil {
		t.Fatal("pattern not stored")
	}
	if len(p.Samples) == 0 {
		t.Fatal("expected a captured sample on the same tick the pattern was learned")
	}
	for _, s := range p.Samples {
		if strings.Contains(s, rawOnlySecret) {
			t.Fatalf("Signal.Raw leaked into a stored sample: %q", s)
		}
	}
	// The stored sample is the (unchanged, secret-free) redacted Message.
	if got := p.Samples[len(p.Samples)-1]; got != sig.Message {
		t.Errorf("stored sample = %q, want the redacted Message %q", got, sig.Message)
	}

	// Prompt-facing sample: the detect prompt is built from sampleMessages
	// (reads .Message), so it must be clean too.
	samples := sampleMessages([]core.Signal{sig}, 3)
	_, user := detect.BuildPrompt(core.AgentResult{
		Verdict:       core.VerdictUnknown,
		PatternID:     key,
		Template:      p.Template,
		SampleSignals: []core.Signal{sig},
		Frequency:     1,
	}, sig.Source, "_unknown", samples)
	if strings.Contains(user, rawOnlySecret) {
		t.Fatalf("Signal.Raw leaked into the detect prompt:\n%s", user)
	}
}

// TestSampleRing_SecretInMessage_ReScrubbedBeforeStorage plants a secret in the
// Message itself (simulating a not-yet-scrubbed source) and proves the
// defence-in-depth re-scrub inside RecordSample→PushSample redacts it before it
// reaches storage.
func TestSampleRing_SecretInMessage_ReScrubbedBeforeStorage(t *testing.T) {
	red, errs := NewRedactor(false, nil)
	if len(errs) != 0 {
		t.Fatalf("NewRedactor: %v", errs)
	}
	sig := core.Signal{
		Source:  "src",
		Message: "login rejected password=hunter2 for the admin console",
	}
	cat, key := learnOneSample(t, red, sig)

	p := cat.Get(key)
	if p == nil || len(p.Samples) == 0 {
		t.Fatal("expected a captured sample")
	}
	got := p.Samples[len(p.Samples)-1]
	if strings.Contains(got, "hunter2") {
		t.Fatalf("secret survived the re-scrub in the stored sample: %q", got)
	}
	if !strings.Contains(got, "<REDACTED:") {
		t.Errorf("expected a redaction token in the stored sample: %q", got)
	}
}
