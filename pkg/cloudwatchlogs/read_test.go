package cloudwatchlogs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
)

type fakeAPI struct {
	inputs []*cloudwatchlogs.FilterLogEventsInput
	pages  []*cloudwatchlogs.FilterLogEventsOutput
	err    error
	errs   []error
}

func (fake *fakeAPI) FilterLogEvents(_ context.Context, input *cloudwatchlogs.FilterLogEventsInput, _ ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.FilterLogEventsOutput, error) {
	copy := *input
	fake.inputs = append(fake.inputs, &copy)
	if len(fake.errs) > 0 {
		err := fake.errs[0]
		fake.errs = fake.errs[1:]
		if err != nil {
			return nil, err
		}
	}
	if fake.err != nil {
		return nil, fake.err
	}
	if len(fake.pages) == 0 {
		return &cloudwatchlogs.FilterLogEventsOutput{}, nil
	}
	page := fake.pages[0]
	fake.pages = fake.pages[1:]
	return page, nil
}

func event(stream, message string, millis int64) types.FilteredLogEvent {
	return types.FilteredLogEvent{LogStreamName: aws.String(stream), Message: aws.String(message), Timestamp: aws.Int64(millis)}
}

func TestReaderPinsScopeAndSamplesFields(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	fake := &fakeAPI{pages: []*cloudwatchlogs.FilterLogEventsOutput{
		{Events: []types.FilteredLogEvent{event("app-a", `{"service":"api","arn:aws:123456789012:x":1}`, now.UnixMilli()), event("other", `{"escape":1}`, now.UnixMilli())}, NextToken: aws.String("next")},
		{Events: []types.FilteredLogEvent{event("app-b", `{"level":"warn"}`, now.UnixMilli())}},
	}}
	reader, err := NewReader(fake, Scope{Region: "us-east-1", LogGroupName: "/app", LogStreamPrefix: "app-", FilterPattern: "ERROR"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	reader.now = func() time.Time { return now }
	result, err := reader.Discover(context.Background(), Request{Limit: 5})
	if err != nil || !result.Sampled || result.Count != 2 || len(result.Fields) != 3 || strings.Contains(strings.Join(result.Fields, ","), "123456789012") {
		t.Fatalf("sampled fields: %+v, %v", result, err)
	}
	if len(fake.inputs) != 2 || aws.ToString(fake.inputs[0].LogGroupName) != "/app" || aws.ToString(fake.inputs[0].LogStreamNamePrefix) != "app-" || aws.ToString(fake.inputs[0].FilterPattern) != "ERROR" || aws.ToString(fake.inputs[1].NextToken) != "next" || aws.ToInt64(fake.inputs[0].EndTime)-aws.ToInt64(fake.inputs[0].StartTime) != int64(time.Hour/time.Millisecond) {
		t.Fatalf("unscoped request: %+v", fake.inputs)
	}
}

func TestReaderBoundsAndErrors(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name  string
		pages []*cloudwatchlogs.FilterLogEventsOutput
		want  string
	}{
		{"cycle", []*cloudwatchlogs.FilterLogEventsOutput{{NextToken: aws.String("repeat")}, {NextToken: aws.String("repeat")}}, "pagination_cycle"},
		{"pages", []*cloudwatchlogs.FilterLogEventsOutput{{NextToken: aws.String("one")}, {NextToken: aws.String("two")}, {NextToken: aws.String("three")}}, "page_limit"},
		{"bytes", []*cloudwatchlogs.FilterLogEventsOutput{{Events: []types.FilteredLogEvent{event("app", strings.Repeat("x", MaxBytes), now.UnixMilli())}}}, "byte_limit"},
		{"rows", []*cloudwatchlogs.FilterLogEventsOutput{{Events: []types.FilteredLogEvent{event("app", "hello", now.UnixMilli()), event("app", "hello", now.UnixMilli())}}}, "result_limit"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := &fakeAPI{pages: test.pages}
			reader, _ := NewReader(fake, Scope{Region: "us-east-1", LogGroupName: "/app"}, nil)
			reader.now = func() time.Time { return now }
			limit := 20
			if test.name == "rows" {
				limit = 1
			}
			result, err := reader.Read(context.Background(), Request{Limit: limit, Search: ""})
			if err != nil || !result.Truncated || len(result.Truncation) != 1 || result.Truncation[0] != test.want || len(fake.inputs) > MaxRequests {
				t.Fatalf("result=%+v calls=%d err=%v", result, len(fake.inputs), err)
			}
		})
	}
	fake := &fakeAPI{err: errors.New("private AWS details")}
	reader, _ := NewReader(fake, Scope{Region: "us-east-1", LogGroupName: "/app"}, nil)
	if _, err := reader.Read(context.Background(), Request{LookbackMinutes: 361}); !errors.Is(err, ErrScope) || len(fake.inputs) != 0 {
		t.Fatalf("invalid lookback made requests: %v", err)
	}
	if _, err := reader.Read(context.Background(), Request{}); err == nil || len(fake.inputs) != 1 {
		t.Fatalf("backend error: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := reader.Read(ctx, Request{}); !errors.Is(err, context.Canceled) || len(fake.inputs) != 1 {
		t.Fatalf("canceled request made AWS call: %v", err)
	}
	empty := &fakeAPI{}
	reader, _ = NewReader(empty, Scope{Region: "us-east-1", LogGroupName: "/app"}, nil)
	result, err := reader.Discover(context.Background(), Request{})
	if err != nil || result.Count != 0 || !result.Sampled || result.Truncated || len(empty.inputs) != 1 {
		t.Fatalf("empty discovery: %+v %v", result, err)
	}
}

func TestReaderLaterPageFailuresKeepPartialEvidence(t *testing.T) {
	for _, failure := range []struct {
		name string
		err  error
		page *cloudwatchlogs.FilterLogEventsOutput
	}{
		{name: "oversized response", err: ErrResponse},
		{name: "backend failure", err: errors.New("private backend details")},
		{name: "invalid page", page: &cloudwatchlogs.FilterLogEventsOutput{Events: make([]types.FilteredLogEvent, pageSize+1)}},
	} {
		t.Run(failure.name, func(t *testing.T) {
			now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
			fake := &fakeAPI{pages: []*cloudwatchlogs.FilterLogEventsOutput{
				{Events: []types.FilteredLogEvent{event("app", `{"level":"error"}`, now.UnixMilli())}, NextToken: aws.String("next")},
				failure.page,
			}, errs: []error{nil, failure.err}}
			reader, _ := NewReader(fake, Scope{Region: "us-east-1", LogGroupName: "/app"}, nil)
			reader.now = func() time.Time { return now }
			for _, discover := range []bool{false, true} {
				fake.pages = []*cloudwatchlogs.FilterLogEventsOutput{
					{Events: []types.FilteredLogEvent{event("app", `{"level":"error"}`, now.UnixMilli())}, NextToken: aws.String("next")},
					failure.page,
				}
				fake.errs = []error{nil, failure.err}
				var result Result
				var err error
				if discover {
					result, err = reader.Discover(context.Background(), Request{})
				} else {
					result, err = reader.Read(context.Background(), Request{})
				}
				if err != nil || result.Count != 1 || !result.Truncated || len(result.Truncation) != 1 || result.Truncation[0] != "response_limit" || (discover && len(result.Fields) != 1) || (!discover && len(result.Records) != 1) {
					t.Fatalf("discover=%t result=%+v err=%v", discover, result, err)
				}
			}
		})
	}
}

func TestReaderSafePreservesNonIdentifiers(t *testing.T) {
	reader, _ := NewReader(&fakeAPI{}, Scope{Region: "us-east-1", LogGroupName: "/app"}, nil)
	input := "12:34:56.789 1780000000000 dead:beef:cafe 1234567890123 123456789012 2001:db8::1 arn:aws:logs:us-east-1:123456789012:log-group:x 192.0.2.10"
	output := reader.Safe(input)
	for _, preserved := range []string{"12:34:56.789", "1780000000000", "dead:beef:cafe", "1234567890123"} {
		if !strings.Contains(output, preserved) {
			t.Fatalf("lost %s: %s", preserved, output)
		}
	}
	for _, secret := range []string{" 123456789012 ", "2001:db8::1", "arn:aws:logs", "192.0.2.10"} {
		if strings.Contains(output, secret) {
			t.Fatalf("retained %s: %s", secret, output)
		}
	}
}

func TestReaderSafeIPv6Punctuation(t *testing.T) {
	reader, _ := NewReader(&fakeAPI{}, Scope{Region: "us-east-1", LogGroupName: "/app"}, nil)
	for _, test := range []struct {
		input, want string
	}{
		{"2001:db8::1.", "[redacted]."},
		{"2001:db8::1:", "[redacted]:"},
		{"[2001:db8::1]:443", "[[redacted]]:443"},
		{"2001:db8::1", "[redacted]"},
		{"12:34:56.789", "12:34:56.789"},
		{"1780000000000", "1780000000000"},
		{"1234567890123", "1234567890123"},
		{"dead:beef:cafe", "dead:beef:cafe"},
		{"2001:db8::1::", "[redacted]::"},
		{"2001:db8::1..", "[redacted].."},
		{"2001:db8::1.Retrying", "[redacted].Retrying"},
		{"2001:db8::1.Error", "[redacted].Error"},
		{"2001:db8::1.Failed", "[redacted].Failed"},
		{"2001:db8::1.Closing", "[redacted].Closing"},
		{"2001:db8::1.2001:db8::2", "[redacted].[redacted]"},
		{"2001:db8::1.2001:db8::2.2001:db8::3", "[redacted].[redacted].[redacted]"},
		{"2001:db8::1:xyz", "[redacted]:xyz"},
		{"fe80::1%eth0.", "[redacted]."},
		{"fe80::1%en-0.", "[redacted]."},
		{"fe80::1%eth0.Retrying", "[redacted].Retrying"},
		{"2001:db8::1-2001:db8::2", "[redacted]-[redacted]"},
		{"2001:db8::1_2001:db8::2", "[redacted]_[redacted]"},
		{"2001:db8::1.2001:db8::2", "[redacted].[redacted]"},
		{"2001:db8::1:2001:db8::2", "[redacted]:[redacted]"},
		{"2001:db8::1:2001:db8::2:2001:db8::3", "[redacted]:[redacted]:[redacted]"},
		{"fe80::1%eth0-2001:db8::2", "[redacted]-[redacted]"},
		{"fe80::1%eth0_2001:db8::2", "[redacted]_[redacted]"},
		{"fe80::1%eth0.2001:db8::2", "[redacted].[redacted]"},
		{"fe80::1%eth0:2001:db8::2", "[redacted]:[redacted]"},
		{"[fe80::1%eth0]:443", "[[redacted]]:443"},
		{"Error::retry-2001:db8::7", "Error::retry-[redacted]"},
		{"std::io::Error tokio::runtime Foo::Bar a::b::c ::", "std::io::Error tokio::runtime Foo::Bar a::b::c ::"},
		{"db::cache::get", "db::cache::get"},
		{strings.Repeat("a", 129) + "-2001:db8::2", strings.Repeat("a", 129) + "-[redacted]"},
		{strings.Repeat("a:", 65) + "2001:db8::2", "[redacted]"},
		{strings.Repeat("a:", 65) + "2001:db8::2! next", "[redacted]! next"},
		{strings.Repeat("a:", 65525) + "-2001:db8::2", "[redacted]-[redacted]"},
	} {
		t.Run(test.input, func(t *testing.T) {
			if got := reader.Safe(test.input); got != test.want {
				t.Fatalf("Safe(%q) = %q, want %q", test.input, got, test.want)
			}
		})
	}
}

func TestReaderDiscoveryStopsAtFirstFieldCap(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	fields := map[string]int{}
	for index := 0; index < MaxFields+1; index++ {
		fields[fmt.Sprintf("field_%02d", index)] = index
	}
	message, _ := json.Marshal(fields)
	fake := &fakeAPI{pages: []*cloudwatchlogs.FilterLogEventsOutput{{Events: []types.FilteredLogEvent{
		event("app", string(message), now.UnixMilli()), event("app", `{"later":true}`, now.UnixMilli()),
	}, NextToken: aws.String("next")}}}
	reader, _ := NewReader(fake, Scope{Region: "us-east-1", LogGroupName: "/app"}, nil)
	reader.now = func() time.Time { return now }
	result, err := reader.Discover(context.Background(), Request{})
	if err != nil || !result.Truncated || len(result.Truncation) != 1 || result.Truncation[0] != "field_or_byte_limit" || result.Count != 1 || len(result.Fields) != MaxFields || len(fake.inputs) != 1 {
		t.Fatalf("result=%+v calls=%d err=%v", result, len(fake.inputs), err)
	}
}
