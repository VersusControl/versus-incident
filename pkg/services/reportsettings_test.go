package services

import (
	"testing"
	"time"

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
	// Scheduler defaults: off, 09:00, UTC — on both the nil and empty store.
	for _, got := range []ReportSettings{LoadReportSettings(nil), LoadReportSettings(storage.NewMemory())} {
		if got.ScheduleEnabled || got.SendTime != "09:00" || got.Timezone != "UTC" {
			t.Fatalf("scheduler defaults = %+v, want off/09:00/UTC", got)
		}
	}
}

func TestReportSettings_SaveLoadRoundTrip(t *testing.T) {
	st := storage.NewMemory()
	in := ReportSettings{Enable: true, DefaultChannel: "  slack  ", IncludeChart: false, RatePerMinute: 12, DefaultWindow: "24h",
		ScheduleEnabled: true, SendTime: "  07:30  ", Timezone: "  Asia/Ho_Chi_Minh  "}
	if err := SaveReportSettings(st, in); err != nil {
		t.Fatalf("SaveReportSettings: %v", err)
	}
	got := LoadReportSettings(st)
	if !got.Enable || got.DefaultChannel != "slack" || got.IncludeChart || got.RatePerMinute != 12 || got.DefaultWindow != "24h" {
		t.Fatalf("roundtrip = %+v (channel should be trimmed)", got)
	}
	// The new scheduler fields round-trip, trimmed.
	if !got.ScheduleEnabled || got.SendTime != "07:30" || got.Timezone != "Asia/Ho_Chi_Minh" {
		t.Fatalf("scheduler roundtrip = %+v (send_time/timezone should be trimmed)", got)
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
	// Empty send-time/timezone fall back to the built-in defaults.
	if got.SendTime != "09:00" || got.Timezone != "UTC" {
		t.Fatalf("empty send_time/timezone not defaulted: %+v", got)
	}
}

func TestSaveReportSettings_NoStorage(t *testing.T) {
	if err := SaveReportSettings(nil, ReportSettings{}); err != ErrReportNoStorage {
		t.Fatalf("err = %v, want ErrReportNoStorage", err)
	}
}

func TestReportSettings_Location(t *testing.T) {
	// UTC and a valid IANA name resolve; a bogus zone falls back to UTC.
	if loc := (ReportSettings{Timezone: "UTC"}).Location(); loc != time.UTC {
		t.Fatalf("UTC location = %v, want time.UTC", loc)
	}
	if loc := (ReportSettings{Timezone: ""}).Location(); loc != time.UTC {
		t.Fatalf("empty tz location = %v, want time.UTC", loc)
	}
	loc := (ReportSettings{Timezone: "Asia/Ho_Chi_Minh"}).Location()
	if loc.String() != "Asia/Ho_Chi_Minh" {
		t.Fatalf("IANA location = %q, want Asia/Ho_Chi_Minh", loc.String())
	}
	if bad := (ReportSettings{Timezone: "Mars/Phobos"}).Location(); bad != time.UTC {
		t.Fatalf("bogus tz location = %v, want UTC fallback", bad)
	}
}

func TestValidSendTimeAndTimezone(t *testing.T) {
	goodTimes := []string{"00:00", "09:00", "23:59", "07:30", "13:05"}
	for _, v := range goodTimes {
		if !ValidSendTime(v) {
			t.Fatalf("ValidSendTime(%q) = false, want true", v)
		}
	}
	badTimes := []string{"", "9:00", "24:00", "23:60", "09:5", "0900", "aa:bb", "12:34:56"}
	for _, v := range badTimes {
		if ValidSendTime(v) {
			t.Fatalf("ValidSendTime(%q) = true, want false", v)
		}
	}
	for _, v := range []string{"UTC", "Asia/Ho_Chi_Minh", "America/New_York", "Europe/London"} {
		if !ValidTimezone(v) {
			t.Fatalf("ValidTimezone(%q) = false, want true", v)
		}
	}
	for _, v := range []string{"", "Not/AZone", "Mars/Phobos", "GMT+7 "} {
		if ValidTimezone(v) {
			t.Fatalf("ValidTimezone(%q) = true, want false", v)
		}
	}
}
