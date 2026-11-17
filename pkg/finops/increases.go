package finops

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Increase statuses. The state machine is open → expected → open | resolved,
// and open → resolved. A resolved increase that steps up again is a new
// increase with a new start day.
const (
	StatusOpen     = "open"
	StatusExpected = "expected"
	StatusResolved = "resolved"
)

// Detection defaults.
const (
	DefaultBaselineDays     = 14
	DefaultTriggerDays      = 3
	DefaultMinIncrease      = 100 * MicrosPerUnit // per month, projected
	DefaultMADFactor        = 3
	DefaultMinRelativeBP    = 2_000 // 20%
	DefaultProjectionDays   = 30
	MaxExpectedDays         = 90
	systemActor             = "system"
	reasonExpectedUntilPast = "expected_until_passed"
)

// DetectOptions tune increase detection.
type DetectOptions struct {
	BaselineDays   int
	TriggerDays    int
	MADFactor      int64
	MinRelativeBP  int64
	MinIncrease    Micros // per month, projected
	ProjectionDays int64
}

func (o DetectOptions) withDefaults() DetectOptions {
	if o.BaselineDays <= 0 {
		o.BaselineDays = DefaultBaselineDays
	}
	if o.TriggerDays <= 0 {
		o.TriggerDays = DefaultTriggerDays
	}
	if o.MADFactor <= 0 {
		o.MADFactor = DefaultMADFactor
	}
	if o.MinRelativeBP <= 0 {
		o.MinRelativeBP = DefaultMinRelativeBP
	}
	if o.MinIncrease <= 0 {
		o.MinIncrease = DefaultMinIncrease
	}
	if o.ProjectionDays <= 0 {
		o.ProjectionDays = DefaultProjectionDays
	}
	return o
}

// Detected is an increase found in the cost history.
type Detected struct {
	ID          string  `json:"id"`
	Key         LineKey `json:"key"`
	ServiceName string  `json:"service_name,omitempty"`
	// Team is the team with the most cost on the line since the increase
	// started ("" when untagged cost is the largest).
	Team          string `json:"team,omitempty"`
	TeamEstimated bool   `json:"team_estimated,omitempty"`
	StartedOn     Date   `json:"started_on"`
	// DailyBefore is the baseline median day; DailyNow the average of the
	// latest trigger-length window.
	DailyBefore   Micros `json:"daily_before_micros"`
	DailyNow      Micros `json:"daily_now_micros"`
	Threshold     Micros `json:"threshold_micros"`
	MonthlyImpact Micros `json:"monthly_impact_micros"`
	// ResolvedOn is set when the line has been back inside the band for
	// the trigger length.
	ResolvedOn Date `json:"resolved_on,omitzero"`
}

// HistoryFunc returns the first day of known history for an account (zero
// when the account has no history). Days before it are unknown, never zero.
type HistoryFunc func(account string) Date

// DetectIncreases walks each usage line's daily cost over complete days up
// to lastComplete and returns every increase it finds, oldest first. Days
// after lastComplete (partial days) are never read. Non-usage records (fees,
// credits, refunds, tax) never trigger an increase. Resource IDs are
// ignored: detection runs on the (account, service, usage type) series so
// an optional resource window cannot break the series.
func DetectIncreases(lines []CostLine, lastComplete Date, history HistoryFunc, opts DetectOptions) []Detected {
	opts = opts.withDefaults()
	type series struct {
		key   LineKey
		name  string
		daily map[Date]Micros
		lines []CostLine
	}
	all := map[LineKey]*series{}
	for _, l := range lines {
		if l.recordType() != RecordUsage || l.Day.After(lastComplete) {
			continue
		}
		k := l.Key()
		k.ResourceID = ""
		s := all[k]
		if s == nil {
			s = &series{key: k, daily: map[Date]Micros{}}
			all[k] = s
		}
		s.daily[l.Day] += l.NetAmortizedMicros
		if l.ServiceName != "" {
			s.name = l.ServiceName
		}
		s.lines = append(s.lines, l)
	}
	keys := make([]LineKey, 0, len(all))
	for k := range all {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return lessKey(keys[i], keys[j]) })

	var out []Detected
	for _, k := range keys {
		s := all[k]
		start := Date{}
		if history != nil {
			start = history(k.Account)
		}
		if start.IsZero() {
			continue
		}
		for _, d := range walkSeries(s.daily, start, lastComplete, opts) {
			d.Key = k
			d.ServiceName = s.name
			end := lastComplete
			if !d.ResolvedOn.IsZero() {
				end = d.ResolvedOn
			}
			var share teamShare
			for _, l := range s.lines {
				if !l.Day.Before(d.StartedOn) && !l.Day.After(end) {
					share.add(l)
				}
			}
			d.Team, d.TeamEstimated = share.top()
			d.ID = IncreaseID(k, d.StartedOn)
			out = append(out, d)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if c := out[i].StartedOn.Compare(out[j].StartedOn); c != 0 {
			return c < 0
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// IncreaseID is stable for a line and start day, so stored status survives
// re-detection.
func IncreaseID(k LineKey, started Date) string {
	k.ResourceID = ""
	return shortHash("increase", k.String(), started.String())
}

func walkSeries(daily map[Date]Micros, start, last Date, o DetectOptions) []Detected {
	B, T := o.BaselineDays, o.TriggerDays
	val := func(d Date) Micros { return daily[d] }
	var out []Detected
	var cur *Detected
	inBand := 0
	var aboveNow, aboveMonthly Micros
	for d := start.AddDays(B + T - 1); !d.After(last); d = d.AddDays(1) {
		if cur == nil {
			base := make([]int64, 0, B)
			for i := B + T - 1; i >= T; i-- {
				base = append(base, val(d.AddDays(-i)))
			}
			med := median(base)
			dev := make([]int64, len(base))
			for i, v := range base {
				dev[i] = absMicros(v - med)
			}
			mad := median(dev)
			band := max(o.MADFactor*mad, mulDiv(absMicros(med), o.MinRelativeBP, 10_000))
			threshold := med + band
			var sum Micros
			above := true
			for i := T - 1; i >= 0; i-- {
				v := val(d.AddDays(-i))
				sum += v
				if v <= threshold {
					above = false
				}
			}
			if !above {
				continue
			}
			now := mulDiv(sum, 1, int64(T))
			monthly := (now - med) * o.ProjectionDays
			if monthly < o.MinIncrease {
				continue
			}
			cur = &Detected{StartedOn: d.AddDays(-(T - 1)), DailyBefore: med, DailyNow: now,
				Threshold: threshold, MonthlyImpact: monthly}
			aboveNow, aboveMonthly = now, monthly
			inBand = 0
			continue
		}
		var sum Micros
		for i := T - 1; i >= 0; i-- {
			sum += val(d.AddDays(-i))
		}
		cur.DailyNow = mulDiv(sum, 1, int64(T))
		cur.MonthlyImpact = (cur.DailyNow - cur.DailyBefore) * o.ProjectionDays
		if val(d) <= cur.Threshold {
			inBand++
		} else {
			inBand = 0
			aboveNow, aboveMonthly = cur.DailyNow, cur.MonthlyImpact
		}
		if inBand >= T {
			// A resolved increase keeps the level it ran at while it was
			// above the band, not the back-to-normal window that resolved
			// it (which can sit below the baseline and read as negative).
			cur.DailyNow, cur.MonthlyImpact = aboveNow, aboveMonthly
			cur.ResolvedOn = d
			out = append(out, *cur)
			cur = nil
		}
	}
	if cur != nil {
		out = append(out, *cur)
	}
	return out
}

// Expected is the "mark expected" decision.
type Expected struct {
	Reason string    `json:"reason"`
	Until  Date      `json:"until"`
	By     string    `json:"by"`
	At     time.Time `json:"at"`
}

// StatusEvent is one entry of an increase's status history.
type StatusEvent struct {
	At     time.Time `json:"at"`
	By     string    `json:"by"`
	From   string    `json:"from"`
	To     string    `json:"to"`
	Reason string    `json:"reason,omitempty"`
	Until  Date      `json:"until,omitzero"`
}

// IncreaseRecord is the stored increase: the detection plus its status,
// the expected decision and the alert bookkeeping.
type IncreaseRecord struct {
	Detected
	Status   string        `json:"status"`
	Expected *Expected     `json:"expected,omitempty"`
	History  []StatusEvent `json:"history,omitempty"`
	// AlertedAt and AlertedImpact record the last alert; an increase is
	// alerted once, and again only when its impact doubles.
	AlertedAt     *time.Time `json:"alerted_at,omitempty"`
	AlertedImpact Micros     `json:"alerted_impact_micros,omitempty"`
}

// Errors of the status state machine.
var (
	ErrReasonRequired = errors.New("finops: a reason is required")
	ErrUntilInvalid   = fmt.Errorf("finops: the end date must be after today and at most %d days ahead", MaxExpectedDays)
	ErrBadTransition  = errors.New("finops: status change not allowed")
)

// Reconcile merges a fresh detection into the stored record (nil for a new
// increase) and applies the time rules: a resolved detection resolves the
// record, and an expected increase reopens by itself on its end date while
// it is still there. now is the wall clock; today the deployment's day.
func Reconcile(rec *IncreaseRecord, det Detected, today Date, now time.Time) IncreaseRecord {
	var out IncreaseRecord
	if rec != nil {
		out = *rec
		out.History = append([]StatusEvent(nil), rec.History...)
	} else {
		out.Status = StatusOpen
	}
	out.Detected = det
	switch {
	case !det.ResolvedOn.IsZero() && out.Status != StatusResolved:
		out.History = append(out.History, StatusEvent{At: now, By: systemActor, From: out.Status, To: StatusResolved})
		out.Status = StatusResolved
	case out.Status == StatusExpected && out.Expected != nil && !today.Before(out.Expected.Until):
		out.History = append(out.History, StatusEvent{At: now, By: systemActor, From: StatusExpected, To: StatusOpen, Reason: reasonExpectedUntilPast})
		out.Status = StatusOpen
	}
	return out
}

// StatusChange is an admin's request to change an increase's status.
type StatusChange struct {
	Status string `json:"status"` // expected | open
	Reason string `json:"reason,omitempty"`
	Until  Date   `json:"until,omitzero"`
}

// ApplyStatusChange validates and applies an admin status change.
// Marking expected needs a reason and an end date after today and at most
// MaxExpectedDays ahead. A resolved increase cannot be changed.
func ApplyStatusChange(rec IncreaseRecord, ch StatusChange, by string, today Date, now time.Time) (IncreaseRecord, error) {
	if rec.Status == StatusResolved {
		return rec, ErrBadTransition
	}
	out := rec
	out.History = append([]StatusEvent(nil), rec.History...)
	switch ch.Status {
	case StatusExpected:
		reason := strings.TrimSpace(ch.Reason)
		if reason == "" {
			return rec, ErrReasonRequired
		}
		if !ch.Until.After(today) || ch.Until.After(today.AddDays(MaxExpectedDays)) {
			return rec, ErrUntilInvalid
		}
		out.Expected = &Expected{Reason: reason, Until: ch.Until, By: by, At: now}
		out.History = append(out.History, StatusEvent{At: now, By: by, From: rec.Status, To: StatusExpected, Reason: reason, Until: ch.Until})
		out.Status = StatusExpected
	case StatusOpen:
		if rec.Status != StatusExpected {
			return rec, ErrBadTransition
		}
		out.History = append(out.History, StatusEvent{At: now, By: by, From: StatusExpected, To: StatusOpen, Reason: strings.TrimSpace(ch.Reason)})
		out.Status = StatusOpen
	default:
		return rec, ErrBadTransition
	}
	return out, nil
}

// ShouldAlert reports whether an increase must be sent through the alert
// path: once when open, and again only when its impact has doubled.
func ShouldAlert(rec IncreaseRecord) bool {
	if rec.Status != StatusOpen {
		return false
	}
	if rec.AlertedAt == nil {
		return true
	}
	return rec.MonthlyImpact >= 2*rec.AlertedImpact
}

// MarkAlerted records an alert.
func MarkAlerted(rec IncreaseRecord, now time.Time) IncreaseRecord {
	t := now
	rec.AlertedAt = &t
	rec.AlertedImpact = rec.MonthlyImpact
	return rec
}
