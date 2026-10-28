package prometheus

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/core"
	prometheusapp "github.com/VersusControl/versus-incident/pkg/prometheus"
	"github.com/VersusControl/versus-incident/pkg/signalsources"
)

type fakeReader struct{}

func (fakeReader) MetadataFor(context.Context, string, time.Time, time.Time) (map[string]signalsources.MetricMeta, error) {
	return map[string]signalsources.MetricMeta{}, nil
}
func (fakeReader) LabelValuesLimited(_ context.Context, label string, _, _ time.Time, _ int, _ ...string) ([]string, error) {
	if label == "__name__" {
		return []string{"up"}, nil
	}
	return []string{"api"}, nil
}
func (fakeReader) QueryRange(context.Context, string, time.Time, time.Time, time.Duration) ([]signalsources.MetricSeries, error) {
	return nil, nil
}

func authorizedContext() context.Context {
	return core.WithCallerAuthorization(context.Background(), core.CallerAuthorization{Authenticated: true, Permissions: map[core.Permission]bool{core.PermissionInfrastructureView: true}})
}

func TestFilterAuthorizedChecksPermissionAndLiveGuard(t *testing.T) {
	service, err := prometheusapp.NewService(fakeReader{}, prometheusapp.Config{})
	if err != nil {
		t.Fatal(err)
	}
	active := true
	tools := New([]Source{{Name: "metrics", Service: service, Guard: func(context.Context) bool { return active }}})
	if got := FilterAuthorized(context.Background(), tools); len(got) != 0 {
		t.Fatalf("unauthorized tools = %v", got)
	}
	if got := FilterAuthorized(authorizedContext(), tools); len(got) != 2 {
		t.Fatalf("authorized tools = %v", got)
	}
	active = false
	if got := FilterAuthorized(authorizedContext(), tools); len(got) != 0 {
		t.Fatalf("expired tools = %v", got)
	}
	result, err := tools[0].Invoke(authorizedContext(), json.RawMessage(`{"limit":1}`))
	if err != nil || result.IsAvailable() {
		t.Fatalf("expired invoke result=%+v err=%v", result, err)
	}
}

func TestFilterAuthorizedRemovesDeniedSourcesFromCatalog(t *testing.T) {
	service, err := prometheusapp.NewService(fakeReader{}, prometheusapp.Config{})
	if err != nil {
		t.Fatal(err)
	}
	tools := New([]Source{
		{Name: "allowed", Service: service, Guard: func(context.Context) bool { return true }},
		{Name: "denied", Service: service, Guard: func(context.Context) bool { return false }},
	})
	filtered := FilterAuthorized(authorizedContext(), tools)
	if len(filtered) != 2 {
		t.Fatalf("filtered tools = %v", filtered)
	}
	for _, candidate := range filtered {
		routed := candidate.(core.SourceRoutedTool)
		if names := routed.SourceNames(); len(names) != 1 || names[0] != "allowed" {
			t.Fatalf("sources = %v", names)
		}
	}
}

func TestMultipleSourcesRequireExactName(t *testing.T) {
	service, _ := prometheusapp.NewService(fakeReader{}, prometheusapp.Config{})
	tools := New([]Source{{Name: "b", Service: service, Guard: func(context.Context) bool { return true }}, {Name: "a", Service: service, Guard: func(context.Context) bool { return true }}})
	if _, err := tools[0].Invoke(authorizedContext(), json.RawMessage(`{"limit":1}`)); err == nil {
		t.Fatal("ambiguous source accepted")
	}
	if _, err := tools[0].Invoke(authorizedContext(), json.RawMessage(`{"source":"missing","limit":1}`)); err == nil {
		t.Fatal("unknown source accepted")
	}
	if _, err := tools[0].Invoke(authorizedContext(), json.RawMessage(`{"source":"a","limit":1}`)); err != nil {
		t.Fatal(err)
	}
}
