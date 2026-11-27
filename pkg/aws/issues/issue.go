// Package issues turns what the AWS collectors read into issues: one
// problem on one stable owner, with its evidence, what to do, and a life
// cycle (open, acknowledged, rejected, resolved, regressed) that never
// resolves on data it could not read.
//
// Rules are pure functions over a read-only View; they never call AWS.
// Advance applies one evaluation's findings to the stored issues as a pure
// function, the Evaluator runs it after collector commits on one replica at
// a time, and Summarize is the one count every reader shows.
package issues

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
	"time"
)

// Kind is what an issue threatens.
type Kind string

// Issue kinds.
const (
	KindReliability Kind = "reliability"
	KindSecurity    Kind = "security"
)

// Severity ranks an issue.
type Severity string

// Severities, highest first.
const (
	SeverityCritical Severity = "critical"
	SeverityHigh     Severity = "high"
	SeverityMedium   Severity = "medium"
	SeverityLow      Severity = "low"
)

// Rank orders severities: critical is highest. An unknown severity ranks
// below low.
func (severity Severity) Rank() int {
	switch severity {
	case SeverityCritical:
		return 4
	case SeverityHigh:
		return 3
	case SeverityMedium:
		return 2
	case SeverityLow:
		return 1
	}
	return 0
}

// Valid reports whether severity is one of the four severities.
func (severity Severity) Valid() bool { return severity.Rank() > 0 }

// Tier is where the queue lists an issue.
type Tier string

// Tiers.
const (
	TierActNow     Tier = "act_now"
	TierWorthALook Tier = "worth_a_look"
)

// TierFor is the default tier of a severity: critical and high act now.
func TierFor(severity Severity) Tier {
	if severity.Rank() >= SeverityHigh.Rank() {
		return TierActNow
	}
	return TierWorthALook
}

// State is where an issue is in its life cycle.
type State string

// States.
const (
	StateOpen         State = "open"
	StateAcknowledged State = "acknowledged"
	StateRejected     State = "rejected"
	StateResolved     State = "resolved"
	StateRegressed    State = "regressed"
)

// Active reports whether the issue counts as open: open, acknowledged or
// regressed.
func (state State) Active() bool {
	return state == StateOpen || state == StateAcknowledged || state == StateRegressed
}

// Confirm says whether the issue's data was read in the latest evaluation.
type Confirm string

// Confirm values.
const (
	Confirmed   Confirm = "confirmed"
	CantConfirm Confirm = "cant_confirm"
)

// EvidenceStatus is how sure one link of the evidence chain is.
type EvidenceStatus string

// Evidence statuses.
const (
	EvidenceProven  EvidenceStatus = "proven"
	EvidenceAssumed EvidenceStatus = "assumed"
	EvidenceNotRead EvidenceStatus = "not_read"
)

// Evidence is one link of an issue's evidence chain.
type Evidence struct {
	Text   string         `json:"text"`
	Status EvidenceStatus `json:"status"`
	// Reason says why the link is assumed or not read.
	Reason string    `json:"reason,omitempty"`
	Source string    `json:"source,omitempty"`
	AsOf   time.Time `json:"as_of,omitzero"`
}

// Step is one thing the user does: a console path or a command they run.
type Step struct {
	Text    string `json:"text"`
	Console string `json:"console,omitempty"`
	Command string `json:"command,omitempty"`
}

// Cause is the likely cause of an issue. The evaluator leaves it empty; the
// likely-cause lookup fills it.
type Cause struct {
	// Status is the lookup's answer: found, none, start_unknown or
	// incomplete.
	Status string `json:"status"`
	Text   string `json:"text,omitempty"`
	Change string `json:"change,omitempty"`
	// Resource is the resource the likely cause changed, when the change
	// names one.
	Resource string    `json:"resource,omitempty"`
	Actor    string    `json:"actor,omitempty"`
	At       time.Time `json:"at,omitzero"`
	Checked  time.Time `json:"checked,omitzero"`
}

// Rejection is a user's decision that an issue does not fit how the
// company works. It holds until the issue's material evidence changes, its
// severity rises above SeverityAtReject, or ReviewOn comes.
type Rejection struct {
	Reason string    `json:"reason"`
	By     string    `json:"by"`
	At     time.Time `json:"at"`
	// ReviewOn is the day the rejection lapses, YYYY-MM-DD in UTC; empty
	// keeps it until the evidence changes or the severity rises.
	ReviewOn         string   `json:"review_on,omitempty"`
	Digest           string   `json:"digest"`
	SeverityAtReject Severity `json:"severity_at_reject"`
}

// Issue is one problem on one stable owner.
type Issue struct {
	ID       string   `json:"id"`
	Rule     string   `json:"rule"`
	Kind     Kind     `json:"kind"`
	Severity Severity `json:"severity"`
	Tier     Tier     `json:"tier"`
	Account  string   `json:"account"`
	Region   string   `json:"region,omitempty"`
	// Owner is the stable owner: an ARN, or a key that names the account.
	Owner   string  `json:"owner"`
	GroupID string  `json:"group_id,omitempty"`
	Title   string  `json:"title"`
	State   State   `json:"state"`
	Confirm Confirm `json:"confirm"`
	// Since is when the problem started, when SinceKnown; otherwise it
	// existed before it could be seen.
	Since        time.Time  `json:"since,omitzero"`
	SinceKnown   bool       `json:"since_known"`
	FirstSeen    time.Time  `json:"first_seen"`
	OpenedAt     time.Time  `json:"opened_at"`
	ResolvedAt   time.Time  `json:"resolved_at,omitzero"`
	Rejection    *Rejection `json:"rejection,omitempty"`
	Evidence     []Evidence `json:"evidence,omitempty"`
	Cause        *Cause     `json:"cause,omitempty"`
	Steps        []Step     `json:"steps,omitempty"`
	HygieneCount int        `json:"hygiene_count,omitempty"`
	// Alerted is what the alert path last recorded on the issue; nil
	// before its first alert.
	Alerted *AlertMark `json:"alerted,omitempty"`
	// Digest is the hash of the latest finding's Material.
	Digest  string `json:"digest"`
	Version int64  `json:"version"`
	// UpdatedAt is when the issue was last written.
	UpdatedAt time.Time `json:"updated_at"`
	// Org is the organization the issue was read from.
	Org string `json:"-"`
}

// IssueID is the stable ID of the issue one rule raises on one owner. It
// does not change across node churn, restarts or replicas.
func IssueID(rule, owner string) string {
	sum := sha256.Sum256([]byte(rule + "\x00" + owner))
	return hex.EncodeToString(sum[:])
}

// Material is the facts of a finding that would change the user's answer:
// for a key, which addresses and services use it, not when it was last
// used; for a bucket, which principals reach it, not the object count. A
// rejection holds while its digest is unchanged.
type Material map[string]string

// Digest hashes the material, independent of map order. Empty material
// has a fixed digest.
func (material Material) Digest() string {
	keys := make([]string, 0, len(material))
	for key := range material {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var text strings.Builder
	for _, key := range keys {
		text.WriteString(key)
		text.WriteByte(0)
		text.WriteString(material[key])
		text.WriteByte(0)
	}
	sum := sha256.Sum256([]byte(text.String()))
	return hex.EncodeToString(sum[:])
}
