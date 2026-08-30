package chat

import (
	"archive/zip"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/core"
)

func TestResolveTimePhrase(t *testing.T) {
	newYork, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 3, 10, 12, 30, 0, 0, newYork)
	tests := []struct {
		name      string
		phrase    string
		wantStart time.Time
		wantEnd   time.Time
		wantErr   bool
	}{
		{name: "relative hours", phrase: "last 2 hours", wantStart: now.Add(-2 * time.Hour), wantEnd: now},
		{name: "today", phrase: "today", wantStart: time.Date(2026, 3, 10, 0, 0, 0, 0, newYork), wantEnd: time.Date(2026, 3, 11, 0, 0, 0, 0, newYork)},
		{name: "yesterday crosses DST", phrase: "yesterday", wantStart: time.Date(2026, 3, 9, 0, 0, 0, 0, newYork), wantEnd: time.Date(2026, 3, 10, 0, 0, 0, 0, newYork)},
		{name: "last Tuesday means previous week", phrase: "last Tuesday", wantStart: time.Date(2026, 3, 3, 0, 0, 0, 0, newYork), wantEnd: time.Date(2026, 3, 4, 0, 0, 0, 0, newYork)},
		{name: "past week calendar duration", phrase: "over the past week", wantStart: now.AddDate(0, 0, -7), wantEnd: now},
		{name: "ambiguous weekday", phrase: "Tuesday", wantErr: true},
		{name: "unsupported", phrase: "since deploy", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, resolveErr := ResolveTimePhrase(test.phrase, now, newYork)
			if (resolveErr != nil) != test.wantErr {
				t.Fatalf("error = %v, wantErr %v", resolveErr, test.wantErr)
			}
			if test.wantErr {
				return
			}
			if !got.Start.Equal(test.wantStart) || !got.End.Equal(test.wantEnd) {
				t.Fatalf("range = [%s, %s], want [%s, %s]", got.Start, got.End, test.wantStart, test.wantEnd)
			}
		})
	}
}

func TestResolveTimePhraseRequiresTimezone(t *testing.T) {
	if _, err := ResolveTimePhrase("today", time.Now(), nil); err == nil {
		t.Fatal("expected missing timezone error")
	}
}

func TestResolveTimePhraseReturnsFullDayAtMidnight(t *testing.T) {
	location, err := time.LoadLocation("America/Santiago")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 10, 0, 0, 0, 0, location)
	resolved, err := ResolveTimePhrase("today", now, location)
	if err != nil {
		t.Fatal(err)
	}
	wantEnd := time.Date(2026, 9, 11, 0, 0, 0, 0, location)
	if !resolved.Start.Equal(now) || !resolved.End.Equal(wantEnd) {
		t.Fatalf("range = [%s, %s], want [%s, %s]", resolved.Start, resolved.End, now, wantEnd)
	}
}

func TestResolveTimePhraseYesterdayUsesCalendarBoundariesAcrossSpringForward(t *testing.T) {
	location, err := time.LoadLocation("America/Santiago")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 7, 10, 0, 0, 0, location)
	resolved, err := ResolveTimePhrase("yesterday", now, location)
	if err != nil {
		t.Fatal(err)
	}
	wantStart := time.Date(2026, 9, 6, 1, 0, 0, 0, location)
	wantEnd := time.Date(2026, 9, 7, 0, 0, 0, 0, location)
	if !resolved.Start.Equal(wantStart) || !resolved.End.Equal(wantEnd) || resolved.End.Sub(resolved.Start) != 23*time.Hour {
		t.Fatalf("range = [%s, %s] (%s), want [%s, %s] (23h)", resolved.Start, resolved.End, resolved.End.Sub(resolved.Start), wantStart, wantEnd)
	}
}

func TestResolveTimePhraseHandlesSkippedAndSubHourCivilBoundaries(t *testing.T) {
	kiritimati, err := time.LoadLocation("Pacific/Kiritimati")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(1995, time.January, 1, 12, 0, 0, 0, kiritimati)
	yesterday, err := ResolveTimePhrase("yesterday", now, kiritimati)
	if err != nil {
		t.Fatal(err)
	}
	if !yesterday.Start.Before(yesterday.End) || civilDate(yesterday.Start) != "1994-12-30" {
		t.Fatalf("skipped-day yesterday = [%s, %s]", yesterday.Start, yesterday.End)
	}

	pitcairn, err := time.LoadLocation("Pacific/Pitcairn")
	if err != nil {
		t.Fatal(err)
	}
	now = time.Date(1998, time.April, 27, 0, 30, 0, 0, pitcairn)
	today, err := ResolveTimePhrase("today", now, pitcairn)
	if err != nil {
		t.Fatal(err)
	}
	if now.Before(today.Start) || !now.Before(today.End) {
		t.Fatalf("sub-hour boundary excluded now=%s from [%s, %s]", now, today.Start, today.End)
	}
}

func TestResolveTimePhraseAllZonePostcondition(t *testing.T) {
	archive, err := zip.OpenReader(filepath.Join(runtime.GOROOT(), "lib", "time", "zoneinfo.zip"))
	if err != nil {
		t.Skipf("zoneinfo archive unavailable: %v", err)
	}
	defer archive.Close()
	dates := []time.Time{
		time.Date(1970, 1, 1, 12, 0, 0, 0, time.UTC),
		time.Date(1993, 8, 22, 12, 0, 0, 0, time.UTC),
		time.Date(1995, 1, 1, 12, 0, 0, 0, time.UTC),
		time.Date(2011, 12, 31, 12, 0, 0, 0, time.UTC),
		time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC),
	}
	for _, file := range archive.File {
		if strings.HasSuffix(file.Name, "/") || strings.HasPrefix(file.Name, "Etc/") {
			continue
		}
		location, loadErr := time.LoadLocation(file.Name)
		if loadErr != nil {
			continue
		}
		for _, date := range dates {
			now := time.Date(date.Year(), date.Month(), date.Day(), 12, 0, 0, 0, location)
			for _, phrase := range []string{"today", "yesterday", "last tuesday"} {
				resolved, resolveErr := ResolveTimePhrase(phrase, now, location)
				if resolveErr != nil {
					continue
				}
				if !resolved.Start.Before(resolved.End) {
					t.Fatalf("%s %s %s returned [%s, %s]", file.Name, now, phrase, resolved.Start, resolved.End)
				}
				if phrase == "today" && (now.Before(resolved.Start) || !now.Before(resolved.End)) {
					t.Fatalf("%s today excluded now=%s from [%s, %s]", file.Name, now, resolved.Start, resolved.End)
				}
			}
		}
	}
}

func TestResolveTimePhraseCivilBoundariesAcrossTimezones(t *testing.T) {
	zones := []string{
		"America/Asuncion", "America/Chicago", "America/Denver", "America/Halifax",
		"America/Havana", "America/Los_Angeles", "America/New_York", "America/Santiago",
		"America/St_Johns", "America/Toronto", "Australia/Lord_Howe", "Australia/Sydney",
		"Europe/Berlin", "Europe/London", "Pacific/Auckland", "Asia/Beirut",
	}
	phrases := []string{"today", "yesterday", "last tuesday"}
	for _, zone := range zones {
		location, err := time.LoadLocation(zone)
		if err != nil {
			t.Fatal(err)
		}
		for year := 2024; year <= 2028; year++ {
			for day := time.Date(year, time.January, 1, 12, 0, 0, 0, location); day.Year() == year; day = day.AddDate(0, 0, 1) {
				for _, hour := range []int{0, 1} {
					now := time.Date(day.Year(), day.Month(), day.Day(), hour, 30, 0, 0, location)
					for _, phrase := range phrases {
						resolved, resolveErr := ResolveTimePhrase(phrase, now, location)
						if resolveErr != nil {
							t.Fatalf("%s %s %s: %v", zone, now, phrase, resolveErr)
						}
						if !resolved.Start.Before(resolved.End) {
							t.Fatalf("%s %s %s: non-increasing range [%s, %s]", zone, now, phrase, resolved.Start, resolved.End)
						}
						span := resolved.End.Sub(resolved.Start)
						if span < 22*time.Hour || span > 26*time.Hour {
							t.Fatalf("%s %s %s: unexpected span %s", zone, now, phrase, span)
						}
						wantStart := expectedCivilStart(phrase, now)
						if got := civilDate(resolved.Start); got != wantStart {
							t.Fatalf("%s %s %s: start date %s, want %s", zone, now, phrase, got, wantStart)
						}
						if phrase == "last tuesday" && resolved.Start.Weekday() != time.Tuesday {
							t.Fatalf("%s %s: last tuesday starts on %s", zone, now, resolved.Start.Weekday())
						}
					}
				}
			}
		}
	}
}

func expectedCivilStart(phrase string, now time.Time) string {
	year, month, day := now.Date()
	delta := 0
	switch phrase {
	case "yesterday":
		delta = -1
	case "last tuesday":
		delta = -((int(now.Weekday()) - int(time.Tuesday) + 7) % 7)
		if delta == 0 {
			delta = -7
		}
	}
	return civilDate(time.Date(year, month, day+delta, 12, 0, 0, 0, time.UTC))
}

func civilDate(value time.Time) string {
	year, month, day := value.Date()
	return time.Date(year, month, day, 0, 0, 0, 0, time.UTC).Format(time.DateOnly)
}

func TestResolveTimePhraseRejectsOverflow(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	for _, phrase := range []string{"last 99999999999999999999 hours", "last 9999999999 days"} {
		t.Run(phrase, func(t *testing.T) {
			resolved, err := ResolveTimePhrase(phrase, now, time.UTC)
			if err == nil {
				t.Fatalf("range = [%s, %s], want oversized phrase rejected", resolved.Start, resolved.End)
			}
		})
	}
}

func TestResolveTimeHintUsesBoundariesAndIgnoresMixedPhrases(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	for _, message := range []string{
		"what happened at the last weekend party",
		"compare yesterday with the last 2 hours",
	} {
		resolved, ok, err := resolveTimeHint(message, now, time.UTC)
		if message == "what happened at the last weekend party" && (ok || resolved != nil || err != nil) {
			t.Fatalf("message %q resolved to %+v err=%v", message, resolved, err)
		}
		if message != "what happened at the last weekend party" && (ok || resolved != nil || err != nil) {
			t.Fatalf("mixed message resolved=%+v ok=%v err=%v", resolved, ok, err)
		}
	}
	resolved, ok, err := resolveTimeHint("show the last 2 hours", now, time.UTC)
	if err != nil || !ok || resolved == nil || !resolved.Start.Equal(now.Add(-2*time.Hour)) {
		t.Fatalf("specific relative hint = %+v ok=%v err=%v", resolved, ok, err)
	}
	resolved, ok, err = resolveTimeHint("show the last 90 days", now, time.UTC)
	if err != nil || ok || resolved != nil {
		t.Fatalf("oversized hint = %+v ok=%v err=%v, want ignored", resolved, ok, err)
	}
}

func TestResolveTimePhraseAllowsThirtyOneCalendarDaysAcrossFallback(t *testing.T) {
	newYork, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2025, 11, 10, 12, 0, 0, 0, newYork)
	rangeValue, err := ResolveTimePhrase("last 31 days", now, newYork)
	if err != nil {
		t.Fatal(err)
	}
	attachment := core.ChatAttachment{Time: &rangeValue}
	if err := validateAttachment(&attachment); err != nil {
		t.Fatalf("31 calendar day range rejected: %v", err)
	}
}
