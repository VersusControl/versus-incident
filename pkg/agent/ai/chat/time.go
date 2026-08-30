package chat

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/VersusControl/versus-incident/pkg/core"
)

var relativeTimePattern = regexp.MustCompile(`^(?:over the |in the |during the )?(?:last|past) ([1-9][0-9]*) (minute|minutes|hour|hours|day|days)$`)
var relativeTimeHintPattern = regexp.MustCompile(`\b(?:over the |in the |during the )?(?:last|past) [1-9][0-9]* (?:minute|minutes|hour|hours|day|days)\b`)
var namedTimeHintPattern = regexp.MustCompile(`\b(?:over the (?:past|last) week|last tuesday|yesterday|today|past week|last week)\b`)

const maxRelativeTimeRange = 31 * 24 * time.Hour

// ResolveTimePhrase converts the deliberately small supported phrase set into
// absolute bounds in loc. Unsupported or ambiguous phrases return an error.
func ResolveTimePhrase(phrase string, now time.Time, loc *time.Location) (core.ChatTimeRange, error) {
	if loc == nil {
		return core.ChatTimeRange{}, fmt.Errorf("timezone is required")
	}
	now = now.In(loc)
	value := strings.ToLower(strings.TrimSpace(phrase))
	resolve := func(start, end time.Time, ok bool) (core.ChatTimeRange, error) {
		if !ok || !start.Before(end) {
			return core.ChatTimeRange{}, fmt.Errorf("unsupported or ambiguous time phrase")
		}
		return core.ChatTimeRange{Start: start, End: end}, nil
	}
	shiftCivilDay := func(value time.Time, days int) (time.Time, bool) {
		year, month, day := value.Date()
		return civilDayStart(year, month, day+days, loc)
	}
	nextExistingDay := func(value time.Time, days, direction int) (time.Time, bool) {
		for offset := days; offset != days+8*direction; offset += direction {
			if start, ok := shiftCivilDay(value, offset); ok {
				return start, true
			}
		}
		return time.Time{}, false
	}

	switch value {
	case "today":
		start, startOK := shiftCivilDay(now, 0)
		end, endOK := nextExistingDay(now, 1, 1)
		return resolve(start, end, startOK && endOK && !now.Before(start) && now.Before(end))
	case "yesterday":
		start, startOK := nextExistingDay(now, -1, -1)
		end, endOK := shiftCivilDay(now, 0)
		return resolve(start, end, startOK && endOK)
	case "past week", "last week", "over the past week", "over the last week":
		return resolve(now.AddDate(0, 0, -7), now, true)
	case "last tuesday":
		daysBack := (int(now.Weekday()) - int(time.Tuesday) + 7) % 7
		if daysBack == 0 {
			daysBack = 7
		}
		start, startOK := shiftCivilDay(now, -daysBack)
		end, endOK := nextExistingDay(now, -daysBack+1, 1)
		return resolve(start, end, startOK && endOK)
	}

	match := relativeTimePattern.FindStringSubmatch(value)
	if len(match) == 3 {
		amount, err := strconv.ParseUint(match[1], 10, 32)
		if err != nil {
			return core.ChatTimeRange{}, fmt.Errorf("relative time amount is out of range")
		}
		var start time.Time
		switch match[2] {
		case "minute", "minutes":
			if amount > uint64(maxRelativeTimeRange/time.Minute) {
				return core.ChatTimeRange{}, fmt.Errorf("relative time range exceeds 31 days")
			}
			start = now.Add(-time.Duration(amount) * time.Minute)
		case "hour", "hours":
			if amount > uint64(maxRelativeTimeRange/time.Hour) {
				return core.ChatTimeRange{}, fmt.Errorf("relative time range exceeds 31 days")
			}
			start = now.Add(-time.Duration(amount) * time.Hour)
		case "day", "days":
			if amount > 31 {
				return core.ChatTimeRange{}, fmt.Errorf("relative time range exceeds 31 days")
			}
			start = now.AddDate(0, 0, -int(amount))
		}
		return resolve(start, now, true)
	}
	return core.ChatTimeRange{}, fmt.Errorf("unsupported or ambiguous time phrase")
}

func civilDayStart(year int, month time.Month, day int, loc *time.Location) (time.Time, bool) {
	target := time.Date(year, month, day, 12, 0, 0, 0, time.UTC)
	year, month, day = target.Date()
	compare := func(value time.Time) int {
		localYear, localMonth, localDay := value.In(loc).Date()
		localDate := time.Date(localYear, localMonth, localDay, 12, 0, 0, 0, time.UTC)
		return localDate.Compare(target)
	}
	anchor := time.Date(year, month, day, 12, 0, 0, 0, loc)
	low := anchor.Add(-72 * time.Hour)
	high := anchor.Add(72 * time.Hour)
	if compare(low) >= 0 || compare(high) < 0 {
		return time.Time{}, false
	}
	for high.Sub(low) > time.Nanosecond {
		middle := low.Add(high.Sub(low) / 2)
		if compare(middle) >= 0 {
			high = middle
		} else {
			low = middle
		}
	}
	return high, compare(high) == 0
}

func resolveTimeHint(message string, now time.Time, loc *time.Location) (*core.ChatTimeRange, bool, error) {
	value := strings.ToLower(message)
	named := namedTimeHintPattern.FindAllString(value, -1)
	relative := relativeTimeHintPattern.FindAllString(value, -1)
	if len(named)+len(relative) == 0 {
		return nil, false, nil
	}
	if len(named)+len(relative) != 1 {
		return nil, false, nil
	}
	phrase := ""
	if len(named) == 1 {
		phrase = named[0]
	} else if len(relative) == 1 {
		phrase = relative[0]
	}
	resolved, err := ResolveTimePhrase(phrase, now, loc)
	if err != nil {
		return nil, false, nil
	}
	return &resolved, true, nil
}
