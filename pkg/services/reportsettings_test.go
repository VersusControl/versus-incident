package services

import (
	"testing"

	"github.com/VersusControl/versus-incident/pkg/storage"
)

func TestReportSettings_DefaultsWhenAbsent(t *testing.T) {
	// nil store → built-in defaults (feature off).
	if got := LoadReportSettings(nil); got.Enable || got.DefaultWindow != "today" || !got.IncludeChart || got.RatePerMinute != 6 {
		t.Fatalf("nil-store defaults = %+v", got)
	}
	// fresh store with no blob → same defaults.
	if got := LoadReportSettings(storage.NewMemory()); got.Enable || got.DefaultWindow != "today" {
		t.Fatalf("empty-store defaults = %+v", got)
	}
}

func TestReportSettings_SaveLoadRoundTrip(t *testing.T) {
	st := storage.NewMemory()
	in := ReportSettings{Enable: true, DefaultChannel: "  slack  ", IncludeChart: false, RatePerMinute: 12, DefaultWindow: "24h"}
	if err := SaveReportSettings(st, in); err != nil {
		t.Fatalf("SaveReportSettings: %v", err)
	}
	got := LoadReportSettings(st)
	if !got.Enable || got.DefaultChannel != "slack" || got.IncludeChart || got.RatePerMinute != 12 || got.DefaultWindow != "24h" {
		t.Fatalf("roundtrip = %+v (channel should be trimmed)", got)
	}
}

func TestReportSettings_Sanitize(t *testing.T) {
	st := storage.NewMemory()
	// A bogus window is normalized to today; a negative rate is clamped to 0.
	if err := SaveReportSettings(st, ReportSettings{Enable: true, DefaultWindow: "year", RatePerMinute: -5}); err != nil {
		t.Fatalf("SaveReportSettings: %v", err)
	}
	got := LoadReportSettings(st)
	if got.DefaultWindow != "today" {
		t.Fatalf("bogus window not sanitized: %q", got.DefaultWindow)
	}
	if got.RatePerMinute != 0 {
		t.Fatalf("negative rate not clamped: %d", got.RatePerMinute)
	}
}

func TestSaveReportSettings_NoStorage(t *testing.T) {
	if err := SaveReportSettings(nil, ReportSettings{}); err != ErrReportNoStorage {
		t.Fatalf("err = %v, want ErrReportNoStorage", err)
	}
}
