// Package suspect finds the recorded AWS change that most likely started
// something: a cost increase (FinOps) or an alarm (Changes, Issues). One
// engine serves every page so the rules never drift apart:
//
//   - a change matches only on canonical ARNs, never on a short name;
//   - the account's global stream (IAM, STS, Organizations, CloudFront,
//     Route 53, global WAF) is always searched next to the anchor's Regions;
//   - a change at or after the anchor start is never a candidate;
//   - "none" is said only when the searched window is fully read and
//     settled, otherwise the answer is "incomplete";
//   - an anchor without a start time picks nothing ("start_unknown");
//   - the answer names the accounts, Regions and time range it searched.
package suspect

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/model"
)

// AnchorKind says what a search is anchored on.
type AnchorKind string

const (
	// AnchorCost is a cost increase. Its start is the end of the first day
	// the higher cost was billed: daily cost cannot place a change inside
	// that day, so every change up to the end of it may be the cause.
	AnchorCost AnchorKind = "cost"
	// AnchorAlarm is an alarm. Its start is the alarm's state transition.
	AnchorAlarm AnchorKind = "alarm"
)

// Default look-back windows before the anchor start.
const (
	// AlarmLookback: changes up to one hour before the alarm start.
	AlarmLookback = time.Hour
	// CostLookback: the two days before the first billed day, plus that day.
	CostLookback = 72 * time.Hour
	// DefaultLimit is how many candidates a result lists.
	DefaultLimit = 3
)

// Status is the answer's state.
type Status string

const (
	// StatusFound: at least one candidate.
	StatusFound Status = "found"
	// StatusNone: no candidate, and the whole window is read and settled.
	StatusNone Status = "none"
	// StatusIncomplete: no candidate, but part of the window is not read
	// or not settled yet, so a cause may be missing.
	StatusIncomplete Status = "incomplete"
	// StatusStartUnknown: the anchor has no start time, so no window exists
	// and nothing is picked.
	StatusStartUnknown Status = "start_unknown"
	// StatusResourceUnknown: the anchor names no resource that resolves to
	// a canonical ARN, so nothing can match and nothing is picked.
	StatusResourceUnknown Status = "resource_unknown"
)

// Scope ranks how a resource relates to the anchor, best last.
type Scope int

const (
	ScopeNone Scope = iota
	// ScopeNetwork: on the anchor's network path (route tables, subnets,
	// workloads routed through the same NAT gateway).
	ScopeNetwork
	// ScopeApplication: in the same application as an anchor resource.
	ScopeApplication
	// ScopeResource: an anchor resource itself, or the scaling group that
	// contains it.
	ScopeResource
)

// Anchor is what the search explains.
type Anchor struct {
	Kind AnchorKind
	// ID identifies the anchor to the caller (an alarm ARN, a cost line).
	ID      string
	Account string
	// Regions are the anchor's Regions. The global stream is always added.
	// Empty means every Region of the account.
	Regions []string
	// Start is when the anchored effect began. Zero means unknown.
	Start time.Time
	// Lookback is the window before Start; zero takes the kind's default.
	Lookback time.Duration
	// Targets are the canonical ARNs the anchor relates to, with their
	// scope. Keys are compared through Key.
	Targets map[string]Scope
	// Limit caps the listed candidates; zero takes DefaultLimit.
	Limit int
}

// ChangeSource reads recorded changes for one account and Region in
// [from, to). Region "global" is the global stream; an empty Region means
// every Region of the account.
type ChangeSource interface {
	Changes(ctx context.Context, account, region string, from, to time.Time) ([]model.Change, error)
}

// Coverage says whether [from, to) of one account and Region is fully read
// and settled: no gap, and no part still inside the delivery tail. The
// engine asks only after it read the same window from the ChangeSource.
type Coverage interface {
	Settled(account, region string, from, to time.Time) bool
}

// CoverageFunc adapts a function to Coverage.
type CoverageFunc func(account, region string, from, to time.Time) bool

// Settled implements Coverage.
func (f CoverageFunc) Settled(account, region string, from, to time.Time) bool {
	return f(account, region, from, to)
}

// Searched names what a search looked at.
type Searched struct {
	Accounts []string `json:"accounts"`
	// Regions lists the Regions searched, "global" included. It is empty
	// when AllRegions is set.
	Regions    []string  `json:"regions"`
	AllRegions bool      `json:"all_regions,omitempty"`
	From       time.Time `json:"from,omitzero"`
	To         time.Time `json:"to,omitzero"`
}

// Candidate is one change that may have started the anchor.
type Candidate struct {
	Change model.Change
	Scope  Scope
	// Shaping marks a change kind that typically moves the anchor (for a
	// cost anchor: scaling, sizing, routing, retention).
	Shaping bool
	Score   int64
	// Confidence is "likely" or "possible".
	Confidence string
	// Tied marks one of several candidates that fit equally well.
	Tied bool
}

// Result is the engine's answer.
type Result struct {
	Status   Status
	AnchorID string
	// Candidates are best first, at most the anchor's limit.
	Candidates []Candidate
	// Complete is true when the whole window was read and settled. A found
	// result may still be incomplete: a gap could hide a better cause.
	Complete bool
	Searched Searched
}

// Find searches the changes before the anchor start for the likely cause.
// A read error is returned as is, never turned into an empty answer.
func Find(ctx context.Context, anchor Anchor, source ChangeSource, coverage Coverage) (Result, error) {
	result := Result{AnchorID: anchor.ID, Searched: Searched{Accounts: []string{anchor.Account}, Regions: []string{}}}
	regions := searchRegions(anchor.Regions)
	if len(regions) == 1 && regions[0] == "" {
		result.Searched.AllRegions = true
	} else {
		result.Searched.Regions = regions
	}
	if anchor.Start.IsZero() {
		result.Status = StatusStartUnknown
		return result, nil
	}
	targets := make(map[string]Scope, len(anchor.Targets))
	for arn, scope := range anchor.Targets {
		if key := Key(arn); key != "" && scope > targets[key] {
			targets[key] = scope
		}
	}
	lookback := anchor.Lookback
	if lookback <= 0 {
		lookback = defaultLookback(anchor.Kind)
	}
	start := anchor.Start.UTC()
	from := start.Add(-lookback)
	result.Searched.From, result.Searched.To = from, start

	seen := map[string]bool{}
	var found []Candidate
	for _, region := range regions {
		changes, err := source.Changes(ctx, anchor.Account, region, from, start)
		if err != nil {
			return Result{}, err
		}
		for _, change := range changes {
			if seen[change.ID] || change.ErrorCode != "" || change.Account != anchor.Account ||
				change.Time.Before(from) || !change.Time.Before(start) {
				continue
			}
			best := ScopeNone
			for _, arn := range change.Resources {
				if scope := targets[Key(arn)]; scope > best {
					best = scope
				}
			}
			if best == ScopeNone {
				continue
			}
			seen[change.ID] = true
			candidate := Candidate{Change: change, Scope: best, Shaping: anchor.Kind == AnchorCost && CostShaping(change.EventName)}
			candidate.Score = score(candidate, start, lookback)
			found = append(found, candidate)
		}
	}
	result.Complete = true
	for _, region := range regions {
		if !coverage.Settled(anchor.Account, region, from, start) {
			result.Complete = false
		}
	}
	if len(targets) == 0 {
		// Nothing to match on: no candidate is ever forced. Complete still
		// says whether the window was fully read.
		result.Status = StatusResourceUnknown
		return result, nil
	}
	if len(found) == 0 {
		result.Status = StatusNone
		if !result.Complete {
			result.Status = StatusIncomplete
		}
		return result, nil
	}
	result.Status = StatusFound
	result.Candidates = rank(found, start, anchor.Limit)
	return result, nil
}

func defaultLookback(kind AnchorKind) time.Duration {
	if kind == AnchorCost {
		return CostLookback
	}
	return AlarmLookback
}

// searchRegions is the anchor's Regions plus the global stream, sorted and
// de-duplicated; [""] means every Region, the global stream included.
func searchRegions(regions []string) []string {
	set := map[string]bool{}
	for _, region := range regions {
		if region = strings.TrimSpace(region); region != "" {
			set[region] = true
		}
	}
	if len(set) == 0 {
		return []string{""}
	}
	out := make([]string, 0, len(set)+1)
	for region := range set {
		if region != model.GlobalRegion {
			out = append(out, region)
		}
	}
	sort.Strings(out)
	return append(out, model.GlobalRegion)
}

// Score weights: scope first, then the change kind, then who made it, then
// how close before the start it happened.
const (
	weightScope   = 1_000_000
	weightShaping = 500_000
	weightActor   = 100_000
)

func score(candidate Candidate, start time.Time, lookback time.Duration) int64 {
	value := int64(candidate.Scope) * weightScope
	if candidate.Shaping {
		value += weightShaping
	}
	switch candidate.Change.Actor.Class {
	case model.ActorPipeline, model.ActorPerson:
		value += weightActor
	case model.ActorUnknown, "":
		value += weightActor / 2
	}
	// A change closer before the start ranks higher.
	value += int64((lookback - start.Sub(candidate.Change.Time)) / time.Minute)
	return value
}

// rank orders candidates best first. Two or more at the top scope tie: none
// is singled out; they are all Possible, the most recent first.
func rank(found []Candidate, start time.Time, limit int) []Candidate {
	if limit <= 0 {
		limit = DefaultLimit
	}
	sort.SliceStable(found, func(i, j int) bool {
		if found[i].Score != found[j].Score {
			return found[i].Score > found[j].Score
		}
		return found[i].Change.ID < found[j].Change.ID
	})
	top := found[0].Scope
	tied := len(found) > 1 && found[1].Scope == top
	if tied {
		sort.SliceStable(found, func(i, j int) bool {
			ti, tj := found[i].Scope == top, found[j].Scope == top
			if ti != tj {
				return ti
			}
			if ti {
				di, dj := start.Sub(found[i].Change.Time), start.Sub(found[j].Change.Time)
				if di != dj {
					return di < dj
				}
				return found[i].Change.ID < found[j].Change.ID
			}
			return found[i].Score > found[j].Score
		})
	}
	if len(found) > limit {
		found = found[:limit]
	}
	for index := range found {
		candidate := &found[index]
		candidate.Confidence = "possible"
		atTop := candidate.Scope == top
		if !tied && atTop && (candidate.Scope >= ScopeApplication || (candidate.Scope == ScopeNetwork && candidate.Shaping)) {
			candidate.Confidence = "likely"
		}
		candidate.Tied = tied && atTop
	}
	return found
}
