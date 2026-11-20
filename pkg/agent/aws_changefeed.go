package agent

import (
	"context"
	"strconv"
	"time"

	awstools "github.com/VersusControl/versus-incident/pkg/agent/ai/tools/aws"
	commontools "github.com/VersusControl/versus-incident/pkg/agent/ai/tools/common"
	"github.com/VersusControl/versus-incident/pkg/aws/snapshot"
	"github.com/VersusControl/versus-incident/pkg/core"
)

// AWS change records in the recent_changes feed.
const (
	awsChangeKind = "aws_change"
	// awsGapKind marks a stretch whose AWS changes are unknown, so an empty
	// feed is never read as "nothing changed" when it was not read.
	awsGapKind = "aws_changes_unknown"
	// awsFeedPages bounds the pages one recent_changes call reads.
	awsFeedPages = 10
	awsFeedLimit = 200
)

// awsChangeFeed is the AWS change list as a change_feed source: recorded
// CloudTrail writes, bursts and stack groups from stored state, plus one
// record per stretch that was not read.
type awsChangeFeed struct {
	feed awstools.ChangesFunc
	now  func() time.Time
}

func newAWSChangeFeed(feed awstools.ChangesFunc) commontools.ChangeFeed {
	if feed == nil {
		return nil
	}
	return awsChangeFeed{feed: feed, now: time.Now}
}

func (feed awsChangeFeed) Changes(ctx context.Context, since time.Time) ([]commontools.ChangeRecord, error) {
	records := []commontools.ChangeRecord{}
	if !core.CallerAuthorized(ctx, core.PermissionInfrastructureView) {
		return records, nil
	}
	answerer := feed.feed()
	if answerer == nil {
		return records, nil
	}
	window := feed.now().Sub(since)
	if window <= 0 {
		return records, nil
	}
	// The change list serves at most its edition's window: 24 hours in the
	// open-source build.
	window = min(window, awstools.LimitsOf(answerer).MaxWindow)
	from, until := feed.now().Add(-window), feed.now()
	// unknown marks the whole window: a failed or cut-short read must never
	// read as "no AWS changes", even when another source answered.
	unknown := func(why string) commontools.ChangeRecord {
		return commontools.ChangeRecord{Timestamp: from, Service: "aws", Kind: awsGapKind,
			Summary: "AWS changes unknown from " + from.UTC().Format(time.RFC3339) + " to " + until.UTC().Format(time.RFC3339) + " (" + why + ")"}
	}
	request := snapshot.FeedRequest{Window: window, Limit: awsFeedLimit}
	truncated, more := false, false
	for page := 0; page < awsFeedPages; page++ {
		answer, err := answerer.Answer(ctx, request)
		if err != nil {
			// The error is reported as a record, not returned: the merged
			// feed drops a failed source when another one succeeds.
			return append(records, unknown("the AWS change history could not be read")), nil
		}
		truncated = truncated || answer.Summary.Truncated
		if page == 0 {
			for _, gap := range answer.Gaps {
				if gap.Reason == string(snapshot.GapArriving) {
					continue
				}
				records = append(records, commontools.ChangeRecord{Timestamp: gap.From, Service: "aws", Kind: awsGapKind,
					Summary: "AWS changes unknown in " + gap.Account + " " + gap.Region + " from " + gap.From.UTC().Format(time.RFC3339) +
						" to " + gap.To.UTC().Format(time.RFC3339) + " (" + gap.Reason + ")"})
			}
		}
		for _, item := range answer.Items {
			if item.Kind == snapshot.ItemRoutine {
				continue
			}
			summary := awstools.MaskText(item.Actor + ": " + item.Summary)
			if item.ErrorCode != nil {
				summary += " (failed: " + *item.ErrorCode + ")"
			}
			records = append(records, commontools.ChangeRecord{Timestamp: item.Time, Service: item.Service, Kind: awsChangeKind,
				Summary: summary, Ref: "aws-change:" + item.ID})
		}
		more = answer.NextCursor != nil
		if !more {
			break
		}
		request.Cursor = *answer.NextCursor
	}
	switch {
	case truncated:
		records = append(records, unknown("the AWS change list hit its read cap; more changes may exist"))
	case more:
		records = append(records, unknown("only the newest "+strconv.Itoa(awsFeedPages*awsFeedLimit)+" AWS changes were read; older ones may exist"))
	}
	return records, nil
}
