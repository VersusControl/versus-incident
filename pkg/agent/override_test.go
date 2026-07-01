package agent

import (
	"context"
	"testing"
)

// stubOverride is a test resolver that returns a fixed answer.
type stubOverride struct {
	service string
	ok      bool
	gotIn   ServiceOverrideInput
}

func (s *stubOverride) ResolveService(_ context.Context, in ServiceOverrideInput) (string, bool) {
	s.gotIn = in
	return s.service, s.ok
}

// TestResolveServiceOverride_NilSlotReturnsDetected proves the default (no
// resolver wired) path is byte-for-byte unchanged: the detected service is
// returned verbatim.
func TestResolveServiceOverride_NilSlotReturnsDetected(t *testing.T) {
	SetServiceOverride(nil)
	got := ResolveServiceOverride(context.Background(), ServiceOverrideInput{
		SourceType: OverrideSourceLog,
		Service:    "_unknown",
		Pattern:    "p-1",
	})
	if got != "_unknown" {
		t.Fatalf("nil resolver = %q, want _unknown (unchanged)", got)
	}
}

// TestResolveServiceOverride_MatchWins proves an installed resolver's answer
// wins over the detected service.
func TestResolveServiceOverride_MatchWins(t *testing.T) {
	stub := &stubOverride{service: "payments", ok: true}
	SetServiceOverride(stub)
	t.Cleanup(func() { SetServiceOverride(nil) })

	got := ResolveServiceOverride(context.Background(), ServiceOverrideInput{
		SourceType: OverrideSourceLog,
		Service:    "_unknown",
		Pattern:    "p-1",
		Message:    "boom",
	})
	if got != "payments" {
		t.Fatalf("override = %q, want payments", got)
	}
	if stub.gotIn.Pattern != "p-1" || stub.gotIn.Message != "boom" {
		t.Errorf("resolver input not passed through: %+v", stub.gotIn)
	}
}

// TestResolveServiceOverride_NoMatchKeepsDetected proves ("", false) is ignored
// (the auto-detected service is kept).
func TestResolveServiceOverride_NoMatchKeepsDetected(t *testing.T) {
	SetServiceOverride(&stubOverride{ok: false})
	t.Cleanup(func() { SetServiceOverride(nil) })

	got := ResolveServiceOverride(context.Background(), ServiceOverrideInput{
		SourceType: OverrideSourceMetric,
		Service:    "api",
		Signal:     "http_5xx",
	})
	if got != "api" {
		t.Fatalf("no-match = %q, want api (unchanged)", got)
	}
}

// TestResolveServiceOverride_BlankServiceIgnored proves a resolver that returns
// ("", true) can NEVER blank a real attribution.
func TestResolveServiceOverride_BlankServiceIgnored(t *testing.T) {
	SetServiceOverride(&stubOverride{service: "", ok: true})
	t.Cleanup(func() { SetServiceOverride(nil) })

	got := ResolveServiceOverride(context.Background(), ServiceOverrideInput{
		SourceType: OverrideSourceLog,
		Service:    "api",
	})
	if got != "api" {
		t.Fatalf("blank override = %q, want api (never blanked)", got)
	}
}

// TestSetServiceOverride_LastWins proves a second install replaces the first.
func TestSetServiceOverride_LastWins(t *testing.T) {
	SetServiceOverride(&stubOverride{service: "first", ok: true})
	SetServiceOverride(&stubOverride{service: "second", ok: true})
	t.Cleanup(func() { SetServiceOverride(nil) })

	got := ResolveServiceOverride(context.Background(), ServiceOverrideInput{
		SourceType: OverrideSourceLog, Service: "x",
	})
	if got != "second" {
		t.Fatalf("last-wins = %q, want second", got)
	}
}
