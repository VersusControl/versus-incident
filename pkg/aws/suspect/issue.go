package suspect

import (
	"fmt"
	"strings"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/model"
)

// AnchorIssueKind is an issue: a problem a rule found on one owner. Its
// start is when the problem started; a regress starts it again.
const AnchorIssueKind AnchorKind = "issue"

// IssueAnchor is what an issue search needs to know about the issue.
type IssueAnchor struct {
	ID      string
	Account string
	// Region is the issue's Region; empty for an account-wide issue,
	// which searches every Region of the account.
	Region string
	// Owner is the issue's stable owner: an ARN, or a key that names the
	// account (which matches no change).
	Owner string
	// Start is when the problem started. Zero means it existed when it was
	// first read, and no window exists.
	Start time.Time
	// History is how long changes are kept: the search looks back over all
	// of it. Zero takes HistoryFrom alone.
	History time.Duration
	// HistoryFrom is where the stored change history starts for the
	// account; zero when it is not known. A window that reaches before it
	// is not fully read, so the answer is incomplete.
	HistoryFrom time.Time
}

// AnchorIssue is the issue as a suspect anchor. Its targets are the owner,
// its neighbours one hop away in the graph (what the owner relates to, and
// what relates to the owner), and, through Related, the scaling groups,
// applications and network path of those. The window is the stored change
// history before the issue started.
func AnchorIssue(issue IssueAnchor, inventory []model.Resource) Anchor {
	targets := map[string]Scope{}
	var narrowed []model.Resource
	if Key(issue.Owner) != "" {
		targets[issue.Owner] = ScopeResource
		if owner, found := Resolve(issue.Owner, inventory); found {
			narrowed = append(narrowed, owner)
			narrowed = append(narrowed, Neighbours(owner, inventory)...)
		}
	}
	for arn, scope := range Related(inventory, narrowed) {
		if scope > targets[arn] {
			targets[arn] = scope
		}
	}
	var regions []string
	if issue.Region != "" {
		regions = []string{issue.Region}
	}
	lookback := issue.History
	if !issue.Start.IsZero() && !issue.HistoryFrom.IsZero() && issue.HistoryFrom.Before(issue.Start) {
		// Search the whole stored history, never further back than it
		// reaches: a window before it would read as incomplete although
		// nothing older is kept to be missing.
		if kept := issue.Start.Sub(issue.HistoryFrom); lookback <= 0 || kept < lookback {
			lookback = kept
		}
	}
	if lookback <= 0 {
		lookback = AlarmLookback
	}
	return Anchor{Kind: AnchorIssueKind, ID: issue.ID, Account: issue.Account, Regions: regions, Start: issue.Start,
		Lookback: lookback, Targets: targets}
}

// Neighbours are the inventory resources one hop from resource in the
// graph: the targets of its relations and the resources whose relations
// target it. The resource itself is not one.
func Neighbours(resource model.Resource, inventory []model.Resource) []model.Resource {
	key := Key(resource.ARN)
	want := map[string]bool{}
	for _, relation := range resource.Relations {
		if target := Key(relation.Target); target != "" && target != key {
			want[target] = true
		}
	}
	var out []model.Resource
	for _, other := range inventory {
		otherKey := Key(other.ARN)
		if otherKey == "" || otherKey == key {
			continue
		}
		if want[otherKey] {
			out = append(out, other)
			continue
		}
		for _, relation := range other.Relations {
			if Key(relation.Target) == key {
				out = append(out, other)
				break
			}
		}
	}
	return out
}

// Words the issue pages, alerts and the report use for each answer.
const (
	// IssueCauseNone: the whole window was read and no change matched.
	IssueCauseNone = "no change found before it"
	// IssueCauseStartUnknown: the issue has no start to search before.
	IssueCauseStartUnknown = "existed when first read"
	// IssueCauseIncomplete: part of the window is not in the stored change
	// history.
	IssueCauseIncomplete = "older than our change history"
)

// IssueCauseText is what an answer says about an issue. A found answer
// names the best change, who made it and how long before the start; the
// others say why there is none. An owner that names no resource matches
// no change, which reads as none.
func IssueCauseText(result Result, start time.Time) string {
	switch result.Status {
	case StatusFound:
		if len(result.Candidates) == 0 {
			return IssueCauseNone
		}
		return describeChange(result.Candidates[0].Change, start)
	case StatusStartUnknown:
		return IssueCauseStartUnknown
	case StatusIncomplete:
		return IssueCauseIncomplete
	}
	return IssueCauseNone
}

// IssueCauseTextFor is IssueCauseText for an issue whose window was
// bounded by where the stored change history starts: a fully read window
// with no change then says "no change found since our change history
// began on <date>", since nothing older is kept to search. An issue that
// started before the history keeps "older than our change history".
func IssueCauseTextFor(result Result, issue IssueAnchor) string {
	if result.Status == StatusNone && HistoryBounded(issue) {
		return IssueCauseSinceHistory + issue.HistoryFrom.UTC().Format(time.DateOnly)
	}
	return IssueCauseText(result, issue.Start)
}

// IssueCauseSinceHistory starts the answer for a window that reaches back
// to the start of the stored change history; the date follows.
const IssueCauseSinceHistory = "no change found since our change history began on "

// HistoryBounded reports whether the issue's window would reach back
// before the stored change history starts, so the search starts there.
func HistoryBounded(issue IssueAnchor) bool {
	if issue.Start.IsZero() || issue.HistoryFrom.IsZero() || !issue.HistoryFrom.Before(issue.Start) {
		return false
	}
	return issue.History <= 0 || issue.Start.Sub(issue.HistoryFrom) < issue.History
}

// describeChange reads "Runner image updated by ci-deployer, 2 h before".
func describeChange(change model.Change, start time.Time) string {
	text := strings.TrimSpace(change.Summary)
	if text == "" {
		text = strings.TrimSpace(change.EventName)
	}
	if actor := actorName(change.Actor); actor != "" {
		text += " by " + actor
	}
	if before := start.Sub(change.Time); before > 0 {
		text += ", " + roughDuration(before) + " before"
	}
	return text
}

// actorName is the last part of the principal ARN: a role, user or
// session name.
func actorName(actor model.Actor) string {
	arn := strings.TrimSpace(actor.PrincipalARN)
	if arn == "" {
		return ""
	}
	if index := strings.LastIndexAny(arn, "/:"); index >= 0 && index < len(arn)-1 {
		return arn[index+1:]
	}
	return arn
}

// roughDuration reads a duration the way a person says it: "5 min",
// "2 h", "3 days".
func roughDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "less than a minute"
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h", int(d/time.Hour))
	}
	return fmt.Sprintf("%d days", int(d/(24*time.Hour)))
}
