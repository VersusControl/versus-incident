package signalsources

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/config"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	cwltypes "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
)

func TestCloudWatchLogs_ValidationErrors(t *testing.T) {
	if _, err := NewCloudWatchLogsSource("t", config.AgentCloudWatchLogsSourceConfig{LogGroupName: "g"}); err == nil {
		t.Errorf("expected error for missing region")
	}
	if _, err := NewCloudWatchLogsSource("t", config.AgentCloudWatchLogsSourceConfig{Region: "us-east-1"}); err == nil {
		t.Errorf("expected error for missing log_group_name")
	}
}

func TestCloudWatchLogs_Name(t *testing.T) {
	src, err := NewCloudWatchLogsSource("prod-app", config.AgentCloudWatchLogsSourceConfig{
		Region:       "us-east-1",
		LogGroupName: "/aws/lambda/x",
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if got := src.Name(); got != "cloudwatchlogs:prod-app" {
		t.Errorf("unexpected name %q", got)
	}
}

func TestCloudWatchLogs_SignalFromCWEvent(t *testing.T) {
	tsMs := int64(1745143205123)
	msg := "boom: connection refused"
	stream := "2026/05/12/[$LATEST]abc"
	id := "ev-123"

	sig, ok := signalFromCWEvent("cloudwatchlogs:test", cwltypes.FilteredLogEvent{
		Timestamp:     &tsMs,
		Message:       &msg,
		LogStreamName: &stream,
		EventId:       &id,
	})
	if !ok {
		t.Fatalf("expected signal, got skip")
	}
	if sig.Source != "cloudwatchlogs:test" {
		t.Errorf("unexpected source %q", sig.Source)
	}
	if sig.Message != msg {
		t.Errorf("unexpected message %q", sig.Message)
	}
	if !sig.Timestamp.Equal(time.UnixMilli(tsMs).UTC()) {
		t.Errorf("unexpected timestamp %v", sig.Timestamp)
	}
	if sig.Fields["log_stream"] != stream {
		t.Errorf("expected log_stream in fields, got %#v", sig.Fields)
	}
	if sig.Raw["event_id"] != id {
		t.Errorf("expected event_id in raw, got %#v", sig.Raw)
	}
}

func TestCloudWatchLogs_SignalFromCWEvent_SkipsIncomplete(t *testing.T) {
	if _, ok := signalFromCWEvent("x", cwltypes.FilteredLogEvent{}); ok {
		t.Errorf("expected skip on missing fields")
	}
	msg := "x"
	if _, ok := signalFromCWEvent("x", cwltypes.FilteredLogEvent{Message: &msg}); ok {
		t.Errorf("expected skip on missing timestamp")
	}
}

// TestCloudWatchLogs_Pull exercises Pull end-to-end with a fake AWS
// endpoint. Static credentials and a custom BaseEndpoint keep the SDK
// from reaching out to the real AWS network.
func TestCloudWatchLogs_Pull(t *testing.T) {
	t1 := int64(1745143201000)
	t2 := int64(1745143205000)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// CloudWatch Logs JSON-1.1 protocol: target header + JSON body.
		target := r.Header.Get("X-Amz-Target")
		if !strings.HasSuffix(target, "FilterLogEvents") {
			t.Errorf("unexpected target %q", target)
		}
		body, _ := io.ReadAll(r.Body)
		s := string(body)
		if !strings.Contains(s, `"logGroupName":"/aws/lambda/test"`) {
			t.Errorf("body missing log group: %s", s)
		}
		if !strings.Contains(s, `"filterPattern":"ERROR"`) {
			t.Errorf("body missing filter pattern: %s", s)
		}
		if !strings.Contains(s, `"logStreamNamePrefix":"2026/"`) {
			t.Errorf("body missing stream prefix: %s", s)
		}

		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
		// Hand-rolled JSON; the SDK is lenient about field ordering.
		w.Write([]byte(`{
			"events": [
				{"timestamp": ` + itoa(t1) + `, "message": "ERROR connection refused", "logStreamName": "2026/05/12/a", "eventId": "1"},
				{"timestamp": ` + itoa(t2) + `, "message": "ERROR timeout", "logStreamName": "2026/05/12/a", "eventId": "2"}
			],
			"searchedLogStreams": []
		}`))
	}))
	defer srv.Close()

	client := cloudwatchlogs.New(cloudwatchlogs.Options{
		Region:       "us-east-1",
		Credentials:  credentials.NewStaticCredentialsProvider("AKID", "SECRET", ""),
		BaseEndpoint: aws.String(srv.URL),
		HTTPClient:   srv.Client(),
	})
	src := &CloudWatchLogsSource{
		name: "test",
		cfg: config.AgentCloudWatchLogsSourceConfig{
			Region:          "us-east-1",
			LogGroupName:    "/aws/lambda/test",
			LogStreamPrefix: "2026/",
			FilterPattern:   "ERROR",
			PageSize:        100,
		},
		client: client,
	}

	signals, cursor, err := src.Pull(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	if len(signals) != 2 {
		t.Fatalf("expected 2 signals, got %d", len(signals))
	}
	if signals[0].Message != "ERROR connection refused" {
		t.Errorf("unexpected message %q", signals[0].Message)
	}
	if cursor.UnixMilli() != t2 {
		t.Errorf("cursor = %d, want %d", cursor.UnixMilli(), t2)
	}
}

func TestCloudWatchLogs_Pull_AdvancesStartByOneMs(t *testing.T) {
	since := time.UnixMilli(1745143200000).UTC()
	wantStart := since.UnixMilli() + 1

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		// SDK may render numbers as `1745143200001`; just check substring.
		if !strings.Contains(string(body), `"startTime":`+itoa(wantStart)) {
			t.Errorf("expected startTime=%d (since+1ms), body=%s", wantStart, string(body))
		}
		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
		w.Write([]byte(`{"events":[],"searchedLogStreams":[]}`))
	}))
	defer srv.Close()

	client := cloudwatchlogs.New(cloudwatchlogs.Options{
		Region:       "us-east-1",
		Credentials:  credentials.NewStaticCredentialsProvider("AKID", "SECRET", ""),
		BaseEndpoint: aws.String(srv.URL),
		HTTPClient:   srv.Client(),
	})
	src := &CloudWatchLogsSource{
		name:   "t",
		cfg:    config.AgentCloudWatchLogsSourceConfig{Region: "us-east-1", LogGroupName: "g", PageSize: 50},
		client: client,
	}
	if _, _, err := src.Pull(context.Background(), since); err != nil {
		t.Fatalf("pull: %v", err)
	}
}
