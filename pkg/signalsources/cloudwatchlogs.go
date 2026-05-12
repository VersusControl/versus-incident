package signalsources

import (
	"context"
	"fmt"
	"time"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	cwltypes "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
)

// CloudWatchLogsSource pulls log events from one AWS CloudWatch Logs log
// group via FilterLogEvents. Authentication is the standard AWS SDK
// chain (env vars / shared credentials / IAM role on the host).
//
// FilterLogEvents is the right primitive here because:
//   - it is real-time (no async query lifecycle like Insights),
//   - it accepts a `startTime` filter so cursoring is trivial, and
//   - it returns events sorted by ingestion time across streams.
//
// The cursor is the maximum event timestamp seen on the previous tick
// (in milliseconds, the unit CloudWatch uses internally).
type CloudWatchLogsSource struct {
	name   string
	cfg    config.AgentCloudWatchLogsSourceConfig
	client *cloudwatchlogs.Client
}

// NewCloudWatchLogsSource validates config and returns a ready source.
// It loads the default AWS SDK config for the configured region.
func NewCloudWatchLogsSource(name string, cfg config.AgentCloudWatchLogsSourceConfig) (*CloudWatchLogsSource, error) {
	if cfg.Region == "" {
		return nil, fmt.Errorf("cloudwatchlogs source %q: region is required", name)
	}
	if cfg.LogGroupName == "" {
		return nil, fmt.Errorf("cloudwatchlogs source %q: log_group_name is required", name)
	}
	if cfg.PageSize <= 0 {
		cfg.PageSize = 500
	}
	if cfg.PageSize > 10000 {
		cfg.PageSize = 10000
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(
		context.Background(),
		awsconfig.WithRegion(cfg.Region),
	)
	if err != nil {
		return nil, fmt.Errorf("cloudwatchlogs source %q: load aws config: %w", name, err)
	}
	return &CloudWatchLogsSource{
		name:   name,
		cfg:    cfg,
		client: cloudwatchlogs.NewFromConfig(awsCfg),
	}, nil
}

func (s *CloudWatchLogsSource) Name() string { return "cloudwatchlogs:" + s.name }

// Pull issues a FilterLogEvents request with `startTime = since + 1ms`
// (CloudWatch's startTime is inclusive). It walks NextToken pagination
// until the page is short, the requested page size has been collected,
// or we've made `maxPages` calls (safety cap).
//
// Events are appended in API order. The cursor is the maximum event
// timestamp seen, never lower than `since`.
func (s *CloudWatchLogsSource) Pull(ctx context.Context, since time.Time) ([]core.Signal, time.Time, error) {
	cursor := since
	startMs := since.UTC().UnixMilli() + 1

	limit := int32(s.cfg.PageSize)
	in := &cloudwatchlogs.FilterLogEventsInput{
		LogGroupName: &s.cfg.LogGroupName,
		StartTime:    &startMs,
		Limit:        &limit,
	}
	if s.cfg.LogStreamPrefix != "" {
		in.LogStreamNamePrefix = &s.cfg.LogStreamPrefix
	}
	if s.cfg.FilterPattern != "" {
		in.FilterPattern = &s.cfg.FilterPattern
	}

	var signals []core.Signal
	const maxPages = 20
	for page := 0; page < maxPages; page++ {
		out, err := s.client.FilterLogEvents(ctx, in)
		if err != nil {
			return signals, cursor, fmt.Errorf("cloudwatch FilterLogEvents: %w", err)
		}
		for _, e := range out.Events {
			sig, ok := signalFromCWEvent(s.Name(), e)
			if !ok {
				continue
			}
			if sig.Timestamp.After(cursor) {
				cursor = sig.Timestamp
			}
			signals = append(signals, sig)
		}
		if out.NextToken == nil || *out.NextToken == "" {
			break
		}
		if len(signals) >= s.cfg.PageSize {
			break
		}
		in.NextToken = out.NextToken
	}
	return signals, cursor, nil
}

func signalFromCWEvent(srcName string, e cwltypes.FilteredLogEvent) (core.Signal, bool) {
	if e.Timestamp == nil || e.Message == nil {
		return core.Signal{}, false
	}
	ts := time.UnixMilli(*e.Timestamp).UTC()
	fields := map[string]interface{}{}
	if e.LogStreamName != nil {
		fields["log_stream"] = *e.LogStreamName
	}
	raw := map[string]interface{}{
		"message": *e.Message,
	}
	if e.LogStreamName != nil {
		raw["log_stream"] = *e.LogStreamName
	}
	if e.EventId != nil {
		raw["event_id"] = *e.EventId
	}
	return core.Signal{
		Source:    srcName,
		Timestamp: ts,
		Message:   *e.Message,
		Fields:    fields,
		Raw:       raw,
	}, true
}
