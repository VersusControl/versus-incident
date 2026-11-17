package finops

import (
	"encoding/json"
	"fmt"
	"time"
)

// Date is a calendar day with no time zone. Billing data is reported per day
// in the payer's billing calendar, so days are compared as dates, never as
// instants.
type Date struct {
	Year  int
	Month time.Month
	Day   int
}

// DateOf returns the calendar day of t in t's location.
func DateOf(t time.Time) Date {
	y, m, d := t.Date()
	return Date{Year: y, Month: m, Day: d}
}

// ParseDate parses "2006-01-02".
func ParseDate(s string) (Date, error) {
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		return Date{}, fmt.Errorf("finops: invalid date %q", s)
	}
	return DateOf(t), nil
}

// MustDate parses a date and panics on error; for fixtures and tests.
func MustDate(s string) Date {
	d, err := ParseDate(s)
	if err != nil {
		panic(err)
	}
	return d
}

// Time is midnight UTC of the day.
func (d Date) Time() time.Time { return time.Date(d.Year, d.Month, d.Day, 0, 0, 0, 0, time.UTC) }

// IsZero reports the zero Date.
func (d Date) IsZero() bool { return d.Year == 0 && d.Month == 0 && d.Day == 0 }

func (d Date) String() string {
	if d.IsZero() {
		return ""
	}
	return d.Time().Format(time.DateOnly)
}

// AddDays returns the day n days later (earlier for negative n).
func (d Date) AddDays(n int) Date { return DateOf(d.Time().AddDate(0, 0, n)) }

// Before, After and Compare order days.
func (d Date) Before(o Date) bool { return d.Compare(o) < 0 }
func (d Date) After(o Date) bool  { return d.Compare(o) > 0 }
func (d Date) Compare(o Date) int {
	switch {
	case d.Year != o.Year:
		return cmpInt(d.Year, o.Year)
	case d.Month != o.Month:
		return cmpInt(int(d.Month), int(o.Month))
	}
	return cmpInt(d.Day, o.Day)
}

// DaysUntil is the number of days from d to o (o − d).
func (d Date) DaysUntil(o Date) int {
	return int(o.Time().Sub(d.Time()).Hours() / 24)
}

// MonthStart and MonthEnd bound the day's month.
func (d Date) MonthStart() Date { return Date{d.Year, d.Month, 1} }
func (d Date) MonthEnd() Date   { return Date{d.Year, d.Month, 1}.AddDays(DaysIn(d.Year, d.Month) - 1) }

// MonthKey is "2006-01".
func (d Date) MonthKey() string { return d.Time().Format("2006-01") }

// DaysIn is the number of days in a month.
func DaysIn(year int, m time.Month) int {
	return time.Date(year, m+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

func (d Date) MarshalJSON() ([]byte, error) {
	if d.IsZero() {
		return []byte(`""`), nil
	}
	return json.Marshal(d.String())
}

func (d *Date) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	if s == "" {
		*d = Date{}
		return nil
	}
	p, err := ParseDate(s)
	if err != nil {
		return err
	}
	*d = p
	return nil
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// DateRange is an inclusive range of days.
type DateRange struct {
	Start Date `json:"start"`
	End   Date `json:"end"`
}

// Contains reports whether day is inside the range.
func (r DateRange) Contains(day Date) bool { return !day.Before(r.Start) && !day.After(r.End) }

// Days is the number of days in the range (0 when empty).
func (r DateRange) Days() int {
	if r.End.Before(r.Start) {
		return 0
	}
	return r.Start.DaysUntil(r.End) + 1
}

// Each calls fn for every day in order.
func (r DateRange) Each(fn func(Date)) {
	for d := r.Start; !d.After(r.End); d = d.AddDays(1) {
		fn(d)
	}
}
