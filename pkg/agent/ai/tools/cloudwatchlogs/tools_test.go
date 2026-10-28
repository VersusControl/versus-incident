package cloudwatchlogs

import (
	"context"
	"encoding/json"
	"testing"

	app "github.com/VersusControl/versus-incident/pkg/cloudwatchlogs"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
)

type fakeAPI struct{ calls int }

func (fake *fakeAPI) FilterLogEvents(context.Context, *cloudwatchlogs.FilterLogEventsInput, ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.FilterLogEventsOutput, error) {
	fake.calls++
	return &cloudwatchlogs.FilterLogEventsOutput{}, nil
}

func TestToolsRejectDeniedOrMalformedCallsBeforeAWS(t *testing.T) {
	fake := &fakeAPI{}
	reader, err := app.NewReader(fake, app.Scope{Region: "us-east-1", LogGroupName: "/prod"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	tools := New([]Source{{Name: "cloud", Reader: reader, Guard: func(context.Context) bool { return true }}, {Name: "other", Reader: reader, Guard: func(context.Context) bool { return false }}})
	if len(tools) != 2 {
		t.Fatalf("tools=%v", tools)
	}
	allowed := core.WithCallerAuthorization(context.Background(), core.CallerAuthorization{Authenticated: true, Permissions: map[core.Permission]bool{core.PermissionInfrastructureView: true}})
	if tools[0].(core.ContextAuthorizedTool).AuthorizedTool(context.Background()) != nil {
		t.Fatal("denied catalog exposed")
	}
	if result, err := tools[0].Invoke(context.Background(), json.RawMessage(`{"source":"cloud"}`)); err != nil || result.IsAvailable() {
		t.Fatalf("denied invocation: %+v %v", result, err)
	}
	if filtered := tools[0].(core.ContextAuthorizedTool).AuthorizedTool(allowed); filtered == nil || len(filtered.(core.SourceRoutedTool).SourceNames()) != 1 {
		t.Fatalf("authorization failed: %v", filtered)
	}
	for _, raw := range []string{`{"source":"missing"}`, `{"source":"other"}`, `{"source":"cloud","filter_pattern":""}`, `{"source":"cloud","search":"error*","limit":51}`, `{"source":"cloud","lookback_minutes":361}`, `{"source":"cloud","limit":2.5}`, `null`, `{}` + `{}`} {
		if result, err := tools[1].Invoke(allowed, json.RawMessage(raw)); err == nil && result.IsAvailable() {
			t.Fatalf("accepted %s: %+v", raw, result)
		}
	}
	if _, err := tools[0].Invoke(allowed, json.RawMessage(`{"source":"cloud","search":"error"}`)); err == nil {
		t.Fatal("discovery accepted unsupported search")
	}
	if fake.calls != 0 {
		t.Fatalf("invalid calls reached AWS: %d", fake.calls)
	}
	discovery, err := tools[0].Invoke(allowed, json.RawMessage(`{"source":"cloud"}`))
	if err != nil || discovery.Found || fake.calls != 1 {
		t.Fatalf("empty discovery result=%+v err=%v calls=%d", discovery, err, fake.calls)
	}
	result, err := tools[1].Invoke(allowed, json.RawMessage(`{"source":"cloud","search":"literal"}`))
	if err != nil || result.Found || fake.calls != 2 {
		t.Fatalf("empty response result=%+v err=%v calls=%d", result, err, fake.calls)
	}
}
