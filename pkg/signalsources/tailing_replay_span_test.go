package signalsources

import (
	"context"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
)

// nonTailingSource stands in for a source that keeps no boundary window, so the
// widening must pass it over untouched.
type nonTailingSource struct{}

func (nonTailingSource) Name() string { return "fake:plain" }

func (nonTailingSource) Pull(context.Context, time.Time) ([]core.Signal, time.Time, error) {
	return nil, time.Time{}, nil
}

// TestApplyTailReplaySpan_AddsThePersistIntervalToEveryTail pins the coupling
// rule itself: the effective re-read span is the operator's lateness budget
// PLUS one persist interval, for every tailing source and only those.
//
// The addition matters. Taking the larger of the two would spend the whole
// lateness budget on crash replay, so a source configured to tolerate late rows
// would tolerate none of them after a kill.
func TestApplyTailReplaySpan_AddsThePersistIntervalToEveryTail(t *testing.T) {
	es, err := NewElasticsearchSource("es", config.AgentElasticsearchSourceConfig{
		Addresses:     []string{"http://localhost:9200"},
		Index:         "logs-*",
		ReorderWindow: "10s",
	})
	if err != nil {
		t.Fatalf("new elasticsearch source: %v", err)
	}
	sz, err := NewSigNozSource("sz", config.AgentSignozSourceConfig{
		Address:       "http://localhost:8080",
		APIKey:        "k",
		ReorderWindow: "10s",
	})
	if err != nil {
		t.Fatalf("new signoz source: %v", err)
	}
	cw, err := NewCloudWatchLogsSource("cw", config.AgentCloudWatchLogsSourceConfig{
		Region:       "us-east-1",
		LogGroupName: "g",
	})
	if err != nil {
		t.Fatalf("new cloudwatchlogs source: %v", err)
	}

	sources := []core.SignalSource{es, sz, cw, nonTailingSource{}}
	widened := ApplyTailReplaySpan(sources, 30*time.Second)
	if len(widened) != 3 {
		t.Fatalf("widened %d sources (%v), want the 3 tailing ones", len(widened), widened)
	}

	want := map[string]TailWindow{
		"elasticsearch:es":  {Source: "elasticsearch:es", Configured: 10 * time.Second, Effective: 40 * time.Second},
		"signoz:sz":         {Source: "signoz:sz", Configured: 10 * time.Second, Effective: 40 * time.Second},
		"cloudwatchlogs:cw": {Source: "cloudwatchlogs:cw", Configured: 0, Effective: 30 * time.Second},
	}
	for _, got := range widened {
		exp, ok := want[got.Source]
		if !ok {
			t.Errorf("unexpected widened source %q", got.Source)
			continue
		}
		if got != exp {
			t.Errorf("%s: got %+v, want %+v", got.Source, got, exp)
		}
	}

	// Idempotent: the wiring runs once per process, but a second call must not
	// compound the span.
	again := ApplyTailReplaySpan(sources, 30*time.Second)
	for _, got := range again {
		if got != want[got.Source] {
			t.Errorf("second apply changed %s: got %+v, want %+v", got.Source, got, want[got.Source])
		}
	}

	if got := ApplyTailReplaySpan(sources, 0); got != nil {
		t.Errorf("a non-positive span widened %v, want nothing", got)
	}

	// TailWindows reads the same state back without touching it.
	for _, got := range TailWindows(sources) {
		if got != want[got.Source] {
			t.Errorf("TailWindows reported %s as %+v, want %+v", got.Source, got, want[got.Source])
		}
	}
}

// TestCloudWatchLogs_DefaultsToTheReplaySpan is the amplifier this rule exists
// for: a cloudwatch_logs source with no reorder_window configured used to
// re-read nothing below its cursor, so an abrupt restart dropped every event
// read since the last flush. It now carries a dedup set and re-reads one
// persist interval.
func TestCloudWatchLogs_DefaultsToTheReplaySpan(t *testing.T) {
	src, err := NewCloudWatchLogsSource("prod", config.AgentCloudWatchLogsSourceConfig{
		Region:       "us-east-1",
		LogGroupName: "g",
	})
	if err != nil {
		t.Fatalf("new source: %v", err)
	}
	if src.dedup == nil {
		t.Fatal("an unconfigured source keeps no dedup set, so a re-read would duplicate every event")
	}
	if got := src.scanWindow(); got != 0 {
		t.Errorf("scan window before wiring = %v, want 0", got)
	}

	configured, effective := src.SetTailReplaySpan(30 * time.Second)
	if configured != 0 {
		t.Errorf("configured reorder window = %v, want 0 (none was set)", configured)
	}
	if effective != 30*time.Second {
		t.Errorf("effective span = %v, want the 30s persist interval", effective)
	}
}

// TestFormatTailWindows_IsOneLine keeps the boot notice to a single readable
// line however many tails are configured.
func TestFormatTailWindows_IsOneLine(t *testing.T) {
	got := FormatTailWindows([]TailWindow{
		{Source: "signoz:logs", Configured: 2 * time.Minute, Effective: 150 * time.Second},
		{Source: "cloudwatchlogs:prod", Configured: 0, Effective: 30 * time.Second},
	})
	want := "signoz:logs 2m0s->2m30s, cloudwatchlogs:prod 0s->30s"
	if got != want {
		t.Errorf("FormatTailWindows = %q, want %q", got, want)
	}
}
