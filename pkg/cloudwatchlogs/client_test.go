package cloudwatchlogs

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (transport roundTrip) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func TestToolClientPinsRegionAndAttempts(t *testing.T) {
	profile := t.TempDir() + "/config"
	if err := os.WriteFile(profile, []byte("[profile hostile]\nregion = us-east-1\nendpoint_url = https://profile.attacker.invalid\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_CONFIG_FILE", profile)
	t.Setenv("AWS_PROFILE", "hostile")
	t.Setenv("AWS_ACCESS_KEY_ID", "example")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "example")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AWS_ENDPOINT_URL", "https://global.attacker.invalid")
	t.Setenv("AWS_ENDPOINT_URL_CLOUDWATCH_LOGS", "https://service.attacker.invalid")
	t.Setenv("AWS_MAX_ATTEMPTS", "9")
	for _, route := range []struct{ region, host string }{
		{"us-east-1", "logs.us-east-1.amazonaws.com"},
		{"cn-north-1", "logs.cn-north-1.amazonaws.com.cn"},
		{"us-gov-west-1", "logs.us-gov-west-1.amazonaws.com"},
	} {
		t.Run(route.region, func(t *testing.T) {
			calls := 0
			client := &http.Client{Transport: roundTrip(func(request *http.Request) (*http.Response, error) {
				calls++
				if request.URL.Scheme != "https" || request.URL.Host != route.host || request.Header.Get("Authorization") == "" {
					t.Errorf("unexpected request: %s", request.URL)
				}
				return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: io.NopCloser(strings.NewReader("unavailable")), Header: make(http.Header)}, nil
			})}
			api, err := NewToolAPIWithHTTPClient(context.Background(), route.region, client)
			if err != nil {
				t.Fatal(err)
			}
			group := "/prod"
			_, err = api.FilterLogEvents(context.Background(), &cloudwatchlogs.FilterLogEventsInput{LogGroupName: &group})
			if err == nil || calls != 1 {
				t.Fatalf("attempts=%d err=%v", calls, err)
			}
		})
	}
}

func TestToolClientCapsResponseAndRejectsInvalidRegion(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "example")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "example")
	if _, err := NewToolAPIWithHTTPClient(context.Background(), "us-east-1.attacker.invalid", nil); err == nil {
		t.Fatal("accepted unapproved region")
	}
	for _, declared := range []bool{false, true} {
		calls := 0
		body := strings.Repeat("x", maxResponseBytes+1)
		client := &http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) {
			calls++
			length := int64(-1)
			if declared {
				length = int64(len(body))
			}
			return &http.Response{StatusCode: http.StatusOK, ContentLength: length, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
		})}
		api, err := NewToolAPIWithHTTPClient(context.Background(), "us-east-1", client)
		if err != nil {
			t.Fatal(err)
		}
		group := "/prod"
		if _, err := api.FilterLogEvents(context.Background(), &cloudwatchlogs.FilterLogEventsInput{LogGroupName: &group}); err == nil || calls != 1 {
			t.Fatalf("declared=%t calls=%d err=%v", declared, calls, err)
		}
	}
}

func TestToolClientAcceptsFullEventPage(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "example")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "example")
	message := strings.Repeat("x", 2048)
	var events []string
	for index := 0; index < 100; index++ {
		events = append(events, fmt.Sprintf(`{"message":%q,"timestamp":1780000000000,"logStreamName":"app"}`, message))
	}
	body := fmt.Sprintf(`{"events":[%s]}`, strings.Join(events, ","))
	client := &http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, ContentLength: int64(len(body)), Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
	api, err := NewToolAPIWithHTTPClient(context.Background(), "us-east-1", client)
	if err != nil {
		t.Fatal(err)
	}
	group := "/prod"
	page, err := api.FilterLogEvents(context.Background(), &cloudwatchlogs.FilterLogEventsInput{LogGroupName: &group})
	if err != nil || page == nil || len(page.Events) != 100 {
		t.Fatalf("full page: events=%v err=%v", page, err)
	}
}

func TestToolClientOversizedSecondPageReturnsPartial(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "example")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "example")
	now := time.Now().UTC()
	first := fmt.Sprintf(`{"events":[{"message":"evidence","timestamp":%d,"logStreamName":"app"}],"nextToken":"next"}`, now.UnixMilli())
	requests := 0
	client := &http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) {
		requests++
		body := first
		if requests == 2 {
			body = strings.Repeat("x", maxResponseBytes+1)
		}
		return &http.Response{StatusCode: http.StatusOK, ContentLength: int64(len(body)), Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
	api, err := NewToolAPIWithHTTPClient(context.Background(), "us-east-1", client)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := NewReader(api, Scope{Region: "us-east-1", LogGroupName: "/prod"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := reader.Read(context.Background(), Request{})
	if err != nil || requests != 2 || result.Count != 1 || !result.Truncated || len(result.Truncation) != 1 || result.Truncation[0] != "response_limit" || result.Records[0].Message != "evidence" {
		t.Fatalf("requests=%d result=%+v err=%v", requests, result, err)
	}
}
