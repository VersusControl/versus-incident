package services

import (
	"strconv"
	"time"
)

// report_schedule.go — the pure, unit-testable decision for the recurring
// daily incident digest. The runtime loop (a 1-minute ticker in cmd/main.go)
// owns timing and delivery; this file owns ONLY the "should the digest fire
// right now?" predicate, so it can be exhaustively tested without goroutines,
// tickers, storage, or a renderer.

// ReportSendDue reports whether the scheduled daily digest should fire at
// `now`, given the current settings and the moment the digest was last sent
// (zero when it has never been sent this process). It is deliberately pure:
//
//   - Fires ONLY when both Enable and ScheduleEnabled are true.
//   - Fires ONLY on the exact wall-clock minute SendTime in the configured
//     Timezone (a 1-minute ticker visits each minute-of-hour exactly once, so
//     the exact-minute check never misses and never fires on a wrong minute).
//   - Fires at most once per LOCAL calendar day: once lastSent falls on the
//     same local day as `now`, further ticks that same day are suppressed.
//
// Because the comparison is done entirely in the configured *time.Location,
// it is DST-safe: "09:00 local" is well-defined on both sides of a clock
// change, and the once-per-local-day guard keys off the local calendar date.
func ReportSendDue(now time.Time, s ReportSettings, lastSent time.Time) bool {
	if !s.Enable || !s.ScheduleEnabled {
		return false
	}
	hour, minute, ok := parseSendTime(s.SendTime)
	if !ok {
		return false
	}
	loc := s.Location()
	local := now.In(loc)
	if local.Hour() != hour || local.Minute() != minute {
		return false
	}
	if !lastSent.IsZero() && sameLocalDay(local, lastSent.In(loc)) {
		return false
	}
	return true
}

// parseSendTime splits a validated "HH:MM" string into its hour and minute.
// ok is false for any value that does not match the 24-hour HH:MM shape, so a
// legacy/hand-edited blob with a bad send-time simply never fires.
func parseSendTime(hhmm string) (hour, minute int, ok bool) {
	if !ValidSendTime(hhmm) {
		return 0, 0, false
	}
	// ValidSendTime guarantees the exact "HH:MM" shape, so these parses and
	// the fixed offsets cannot fail.
	hour, _ = strconv.Atoi(hhmm[:2])
	minute, _ = strconv.Atoi(hhmm[3:5])
	return hour, minute, true
}

// sameLocalDay reports whether a and b fall on the same calendar day. Both
// must already be expressed in the SAME location (the caller passes local
// times) so the comparison is a true wall-clock-day check.
func sameLocalDay(a, b time.Time) bool {
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	return ay == by && am == bm && ad == bd
}
