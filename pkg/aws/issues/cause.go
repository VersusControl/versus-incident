package issues

import (
	"context"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/changes"
	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/aws/snapshot"
	"github.com/VersusControl/versus-incident/pkg/aws/suspect"
)

// CauseFinder looks up an issue's likely cause in the stored change
// history. It reads stores only, never AWS.
type CauseFinder func(ctx context.Context, issue Issue) (*Cause, error)

// ChangeHistory is the part of the snapshot store the likely-cause lookup
// reads: the inventory for the owner's neighbours, the changes, and where
// the stored change history starts.
type ChangeHistory interface {
	suspect.ChangeReader
	Resources(ctx context.Context, query snapshot.Query) (snapshot.ResourcesResult, error)
	Checkpoint(account, region string) (changes.Checkpoint, error)
}

// NewCauseFinder looks up likely causes with the same engine and words as
// the Changes page. history is how long changes are kept, read at every
// lookup; now stamps when the lookup ran.
func NewCauseFinder(store ChangeHistory, history func() time.Duration, now func() time.Time) CauseFinder {
	if now == nil {
		now = time.Now
	}
	return func(ctx context.Context, issue Issue) (*Cause, error) {
		inventory, err := store.Resources(ctx, snapshot.Query{Account: issue.Account})
		if err != nil {
			return nil, err
		}
		anchor := IssueAnchorOf(issue)
		if history != nil {
			anchor.History = history()
		}
		anchor.HistoryFrom = historyFrom(store, issue)
		return LikelyCause(ctx, anchor, inventory.Resources, suspect.NewSnapshot(store), now().UTC())
	}
}

// IssueAnchorOf is what the likely-cause search needs from an issue. An
// issue that existed when first read has no start.
func IssueAnchorOf(issue Issue) suspect.IssueAnchor {
	anchor := suspect.IssueAnchor{ID: issue.ID, Account: issue.Account, Region: issue.Region, Owner: issue.Owner}
	if issue.SinceKnown {
		anchor.Start = issue.Since
	}
	return anchor
}

// LikelyCause runs the search for one issue and says the answer in the
// design's words.
func LikelyCause(ctx context.Context, anchor suspect.IssueAnchor, inventory []model.Resource, source *suspect.Snapshot, checked time.Time) (*Cause, error) {
	result, err := suspect.Find(ctx, suspect.AnchorIssue(anchor, inventory), source, source)
	if err != nil {
		return nil, err
	}
	cause := CauseOf(result, anchor.Start, checked)
	cause.Text = suspect.IssueCauseTextFor(result, anchor)
	return cause, nil
}

// CauseOf maps a search answer onto an issue's cause. An owner that names
// no resource matches no change, and reads as none.
func CauseOf(result suspect.Result, start, checked time.Time) *Cause {
	cause := &Cause{Status: string(result.Status), Text: suspect.IssueCauseText(result, start), Checked: checked}
	switch result.Status {
	case suspect.StatusFound:
		if len(result.Candidates) > 0 {
			best := result.Candidates[0].Change
			cause.Change, cause.Actor, cause.At = best.ID, best.Actor.PrincipalARN, best.Time.UTC()
		}
	case suspect.StatusStartUnknown, suspect.StatusIncomplete, suspect.StatusNone:
	default:
		cause.Status = string(suspect.StatusNone)
	}
	return cause
}

// Found reports whether the cause names a change. Pages and alerts show
// the likely cause only then.
func (cause *Cause) Found() bool {
	return cause != nil && cause.Status == string(suspect.StatusFound)
}

// historyFrom is where the stored change history of the issue's place
// starts: the later of its Region's and the global stream's first covered
// time, since the search reads both. Zero when either is not known.
func historyFrom(store ChangeHistory, issue Issue) time.Time {
	regions := []string{model.GlobalRegion}
	if issue.Region != "" && issue.Region != model.GlobalRegion {
		regions = append(regions, issue.Region)
	}
	var from time.Time
	for _, region := range regions {
		checkpoint, err := store.Checkpoint(issue.Account, region)
		if err != nil || len(checkpoint.Covered) == 0 {
			return time.Time{}
		}
		first := checkpoint.Covered[0].From
		for _, interval := range checkpoint.Covered[1:] {
			if interval.From.Before(first) {
				first = interval.From
			}
		}
		if first.After(from) {
			from = first
		}
	}
	return from.UTC()
}
