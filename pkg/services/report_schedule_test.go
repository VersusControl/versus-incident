package services

import (
	"testing"
	"time"
)

// scheduled builds a settings value with the daily digest fully enabled at the
// given wall-clock send time in the given timezone.
func scheduled(sendTime, tz string) ReportSettings {
	return ReportSettings{
		Enable:          true,
		ScheduleEnabled: true,
		DefaultWindow:   "24h",
		SendTime:        sendTime,
		Timezone:        tz,
	}
}

func TestReportSendDue_FiresAtTheMinuteOncePerDay(t *testing.T) {
	s := scheduled("09:00", "UTC")

	// Exactly 09:00 UTC, never sent → fire.
	at0900 := time.Date(2026, 7, 10, 9, 0, 0, 0, time.UTC)
	if !ReportSendDue(at0900, s, time.Time{}) {
		t.Fatal("want fire at 09:00 UTC with no prior send")
	}

	// Same minute but already sent this local day → suppressed.
	if ReportSendDue(at0900, s, at0900) {
		t.Fatal("must not fire twice in the same local day")
	}
	// Later the same day at a non-send minute → no fire regardless of lastSent.
	at1300 := time.Date(2026, 7, 10, 13, 0, 0, 0, time.UTC)
	if ReportSendDue(at1300, s, at0900) {
		t.Fatal("must not fire on a non-send minute")
	}

	// The NEXT local day at 09:00 → fire again (once-per-day, not once-ever).
	next0900 := time.Date(2026, 7, 11, 9, 0, 0, 0, time.UTC)
	if !ReportSendDue(next0900, s, at0900) {
		t.Fatal("want fire on the next local day at the send time")
	}
}

func TestReportSendDue_WrongMinuteNeverFires(t *testing.T) {
	s := scheduled("09:00", "UTC")
	for _, at := range []time.Time{
		time.Date(2026, 7, 10, 8, 59, 0, 0, time.UTC),
		time.Date(2026, 7, 10, 9, 1, 0, 0, time.UTC),
		time.Date(2026, 7, 10, 10, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 10, 21, 0, 0, 0, time.UTC),
	} {
		if ReportSendDue(at, s, time.Time{}) {
			t.Fatalf("must not fire at %s (wrong minute)", at.Format(time.RFC3339))
		}
	}
}

func TestReportSendDue_DisabledOrUnscheduledNeverFires(t *testing.T) {
	at := time.Date(2026, 7, 10, 9, 0, 0, 0, time.UTC)

	// Schedule on but feature disabled.
	if ReportSendDue(at, ReportSettings{Enable: false, ScheduleEnabled: true, SendTime: "09:00", Timezone: "UTC"}, time.Time{}) {
		t.Fatal("must not fire when Enable is false")
	}
	// Feature on but schedule disabled.
	if ReportSendDue(at, ReportSettings{Enable: true, ScheduleEnabled: false, SendTime: "09:00", Timezone: "UTC"}, time.Time{}) {
		t.Fatal("must not fire when ScheduleEnabled is false")
	}
	// A malformed send-time never fires.
	if ReportSendDue(at, ReportSettings{Enable: true, ScheduleEnabled: true, SendTime: "9am", Timezone: "UTC"}, time.Time{}) {
		t.Fatal("must not fire with a malformed send_time")
	}
}

func TestReportSendDue_HonorsTimezone(t *testing.T) {
	// 09:00 in Asia/Ho_Chi_Minh (UTC+7) is 02:00 UTC.
	s := scheduled("09:00", "Asia/Ho_Chi_Minh")

	fireUTC := time.Date(2026, 7, 10, 2, 0, 0, 0, time.UTC) // == 09:00 ICT
	if !ReportSendDue(fireUTC, s, time.Time{}) {
		t.Fatal("want fire at 02:00 UTC (== 09:00 Asia/Ho_Chi_Minh)")
	}
	// 09:00 UTC is 16:00 ICT — the wrong local minute → no fire.
	noFire := time.Date(2026, 7, 10, 9, 0, 0, 0, time.UTC)
	if ReportSendDue(noFire, s, time.Time{}) {
		t.Fatal("must not fire at 09:00 UTC for an ICT schedule")
	}

	// The once-per-day guard keys off the LOCAL calendar day. A send at 02:00
	// UTC (still July 10 in ICT) must suppress a later same-ICT-day check.
	lastSent := fireUTC
	sameICTDay := time.Date(2026, 7, 10, 2, 0, 0, 0, time.UTC)
	if ReportSendDue(sameICTDay, s, lastSent) {
		t.Fatal("must not re-fire on the same local (ICT) day")
	}
}

// TestReportSendDue_DSTSane proves the wall-clock send time fires on BOTH
// sides of a daylight-saving transition: "09:00 local" is well-defined whether
// New York is on EST (UTC-5) or EDT (UTC-4), so the digest keeps a stable
// local send time across the change instead of drifting by an hour.
func TestReportSendDue_DSTSane(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	s := scheduled("09:00", "America/New_York")

	// Mid-January: EST (UTC-5). 09:00 local.
	winter := time.Date(2026, 1, 15, 9, 0, 0, 0, ny)
	if !ReportSendDue(winter, s, time.Time{}) {
		t.Fatal("want fire at 09:00 local during EST")
	}
	// Mid-July: EDT (UTC-4). 09:00 local.
	summer := time.Date(2026, 7, 15, 9, 0, 0, 0, ny)
	if !ReportSendDue(summer, s, time.Time{}) {
		t.Fatal("want fire at 09:00 local during EDT")
	}
	// A non-send local minute during EDT → no fire.
	off := time.Date(2026, 7, 15, 10, 0, 0, 0, ny)
	if ReportSendDue(off, s, time.Time{}) {
		t.Fatal("must not fire at 10:00 local")
	}
}
