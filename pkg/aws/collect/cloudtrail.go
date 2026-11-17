package collect

import (
	"context"
	"errors"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudtrail"
	cloudtrailtypes "github.com/aws/aws-sdk-go-v2/service/cloudtrail/types"

	"github.com/VersusControl/versus-incident/pkg/aws/changes"
	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/aws/snapshot"
	"github.com/VersusControl/versus-incident/pkg/awsauth"
)

// maxChangePages bounds one window's read. LookupEvents pages newest first,
// so a window cut short still covers its newest stretch and leaves the rest
// as a gap for the next poll.
var maxChangePages = 20

// changePageSize is the largest page LookupEvents serves.
var changePageSize int32 = 50

func cloudTrailClient(c *call) (*cloudtrail.Client, error) {
	endpoint, client, stack, err := c.env.endpoint("cloudtrail", c.endpointRegion)
	if err != nil {
		return nil, err
	}
	return cloudtrail.NewFromConfig(c.env.config(c.endpointRegion), func(options *cloudtrail.Options) {
		options.BaseEndpoint = &endpoint
		options.HTTPClient = client
		options.RetryMaxAttempts = 1
		options.APIOptions = append(options.APIOptions, stack...)
	}), nil
}

// pollChanges reads CloudTrail write events for one region from the end of
// the checkpoint's coverage, plus the most recent gap, and records exactly
// the intervals it read to the end. A failed window is never marked covered.
func (collector *Collector) pollChanges(ctx context.Context, c *call, options Options) UnitReport {
	report := UnitReport{Unit: c.unit, Outcome: OutcomeKept}
	if c.env.credentials == nil {
		report.ErrorCode = awsauth.ErrNoServerIdentity.Error()
		collector.fail(ctx, c, report.ErrorCode, nil)
		return report
	}
	checkpoint, err := collector.store.Checkpoint(c.unit.Account, c.unit.Region)
	if err != nil {
		report.ErrorCode = CodeStorage
		collector.fail(ctx, c, report.ErrorCode, nil)
		return report
	}
	api, err := cloudTrailClient(c)
	if err != nil {
		report.ErrorCode = errorCode(err)
		collector.fail(ctx, c, report.ErrorCode, nil)
		return report
	}

	unitCtx, cancel := context.WithTimeout(ctx, unitTimeout)
	defer cancel()
	// Never look further back than the store keeps changes: older reads
	// would be pruned at once and claimed coverage would be a lie.
	lookback := options.ChangeLookback
	if retention := collector.store.ChangeRetention(); retention > 0 && retention < lookback {
		lookback = retention
	}
	// CloudTrail delivers events minutes late, so the newest stretch of a
	// window is read but not claimed: it stays a gap until a later poll
	// re-reads it through the overlap.
	settled := c.now.Add(-options.ChangeOverlap)
	var found []model.Change
	var covered []changes.Interval
	code := ""
	for _, window := range checkpoint.Windows(c.now, lookback, options.ChangeOverlap) {
		events, readFrom, err := readWindow(unitCtx, api, window)
		for _, event := range events {
			if change, ok := changes.Normalize(c.unit.Account, c.unit.Region, event); ok {
				found = append(found, change)
			}
		}
		end := window.To
		if end.After(settled) {
			end = settled
		}
		if readFrom.Before(end) {
			covered = append(covered, changes.Interval{From: readFrom, To: end})
		}
		if err != nil {
			code = errorCode(err)
			break
		}
	}
	if ctx.Err() != nil {
		// Shutdown: nothing from a cut-short poll is recorded.
		return report
	}
	if err := c.writer.AppendChanges(c.unit.Region, found, covered, code); err != nil {
		if errors.Is(err, snapshot.ErrPurged) {
			return report
		}
		report.ErrorCode = CodeStorage
		collector.fail(ctx, c, report.ErrorCode, nil)
		return report
	}
	if code != "" {
		report.ErrorCode = code
		collector.fail(ctx, c, code, nil)
		return report
	}
	if err := c.writer.RecordSuccess(c.unit, nil); err != nil {
		report.ErrorCode = CodeStorage
		return report
	}
	report.Outcome = OutcomeCommitted
	report.Resources = len(found)
	return report
}

// readWindow pages LookupEvents for write events in window, newest first.
// It returns the events and the earliest time it can vouch for: window.From
// when every page was read, the oldest event seen when the page cap or an
// error stopped it early, or window.To when nothing could be read.
func readWindow(ctx context.Context, api *cloudtrail.Client, window changes.Interval) ([]changes.Event, time.Time, error) {
	from, to := window.From, window.To
	input := &cloudtrail.LookupEventsInput{
		StartTime:        &from,
		EndTime:          &to,
		MaxResults:       aws.Int32(changePageSize),
		LookupAttributes: []cloudtrailtypes.LookupAttribute{{AttributeKey: cloudtrailtypes.LookupAttributeKeyReadOnly, AttributeValue: aws.String("false")}},
	}
	var events []changes.Event
	oldest := window.To
	// partial is the coverage of a read that stopped early. Events share
	// one-second timestamps, so the second of the oldest event seen is left
	// to the next poll rather than claimed.
	partial := func() time.Time {
		if !oldest.Before(window.To) {
			return window.To
		}
		readFrom := oldest.Truncate(time.Second).Add(time.Second)
		if readFrom.Before(window.From) {
			return window.From
		}
		if readFrom.After(window.To) {
			return window.To
		}
		return readFrom
	}
	for page := 0; page < maxChangePages; page++ {
		output, err := api.LookupEvents(ctx, input)
		if err != nil {
			return events, partial(), err
		}
		for _, item := range output.Events {
			event := changes.Event{
				ID:     deref(item.EventId),
				Name:   deref(item.EventName),
				Source: deref(item.EventSource),
				Time:   derefTime(item.EventTime),
				Raw:    deref(item.CloudTrailEvent),
			}
			event.ReadOnly = deref(item.ReadOnly)
			for _, resource := range item.Resources {
				if name := deref(resource.ResourceName); name != "" {
					event.Resources = append(event.Resources, name)
				}
			}
			if event.ID == "" || event.Time.IsZero() {
				continue
			}
			events = append(events, event)
			if event.Time.Before(oldest) {
				oldest = event.Time
			}
		}
		if deref(output.NextToken) == "" {
			return events, window.From, nil
		}
		input.NextToken = output.NextToken
	}
	// The page cap stopped the read: the newest stretch down to the oldest
	// event seen is covered; the rest stays a gap.
	return events, partial(), nil
}
