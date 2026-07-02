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
)

// TestCloudWatchLogs_FutureTimestampDoesNotStrandCursor is the CloudWatch twin
// of the Elasticsearch stall regression. FilterLogEvents previously had no
// EndTime and the cursor was the max event timestamp, so a single future-dated
// event pinned the cursor ahead of the wall clock and every following
// `startTime = cursor + 1ms` query returned nothing. This test returns a
// future-dated event alongside a present one and proves the request carries
// `endTime` and the returned cursor is clamped to `now`, never the future ts.
func TestCloudWatchLogs_FutureTimestampDoesNotStrandCursor(t *testing.T) {
	frozenNow := time.Date(2026, 7, 2, 10, 10, 0, 0, time.UTC)
	presentMs := frozenNow.Add(-2 * time.Minute).UnixMilli() // 10:08
	futureMs := time.Date(2048, 1, 6, 0, 0, 0, 0, time.UTC).UnixMilli()

	var sawEndTime bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"endTime":`) {
			sawEndTime = true
		}
		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
		// The fake does not filter by endTime; returning the future event proves
		// the CLAMP holds even if a future-dated event reaches the source.
		w.Write([]byte(`{
			"events": [
				{"timestamp": ` + itoa(presentMs) + `, "message": "ERROR present", "logStreamName": "s", "eventId": "1"},
				{"timestamp": ` + itoa(futureMs) + `, "message": "ERROR future garbage", "logStreamName": "s", "eventId": "2"}
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
		name:   "t",
		cfg:    config.AgentCloudWatchLogsSourceConfig{Region: "us-east-1", LogGroupName: "g", PageSize: 50},
		client: client,
		nowFn:  func() time.Time { return frozenNow },
	}

	_, cursor, err := src.Pull(context.Background(), frozenNow.Add(-5*time.Minute))
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	if !sawEndTime {
		t.Errorf("FilterLogEvents request did not carry endTime — future events are not bounded")
	}
	if cursor.After(frozenNow) {
		t.Fatalf("cursor %v is ahead of now %v — future event stranded the cursor", cursor, frozenNow)
	}
	if !cursor.Equal(frozenNow) {
		// The future event's ts is the max seen; clamped, it becomes exactly now.
		t.Errorf("cursor = %v, want it clamped to now %v", cursor, frozenNow)
	}
}

// TestCloudWatchLogs_HealsPoisonedFutureSince proves a poisoned future cursor
// self-heals without hitting the API with an inverted [start,end] window.
func TestCloudWatchLogs_HealsPoisonedFutureSince(t *testing.T) {
	frozenNow := time.Date(2026, 7, 2, 10, 10, 0, 0, time.UTC)
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
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
		nowFn:  func() time.Time { return frozenNow },
	}

	poisoned := time.Date(2048, 1, 6, 0, 0, 0, 0, time.UTC)
	sigs, cursor, err := src.Pull(context.Background(), poisoned)
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	if cursor.After(frozenNow) {
		t.Errorf("cursor %v still ahead of now %v — poisoned since not healed", cursor, frozenNow)
	}
	if len(sigs) != 0 {
		t.Errorf("expected no signals from an empty caught-up window, got %d", len(sigs))
	}
	if called {
		t.Errorf("FilterLogEvents was called with an inverted [start,end] window (start>end)")
	}
}
