package splunk

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
	splunkapp "github.com/VersusControl/versus-incident/pkg/splunk"
)

func TestLiveSplunkNativeTools(t *testing.T) {
	address, password := os.Getenv("HARNESS_SPLUNK_URL"), os.Getenv("HARNESS_SPLUNK_PASSWORD")
	if address == "" || password == "" {
		t.Skip("HARNESS_SPLUNK_URL and HARNESS_SPLUNK_PASSWORD required")
	}
	service, err := splunkapp.NewService(config.AgentSplunkSourceConfig{
		Address: address, Search: "search index=main", Username: "admin", Password: password,
		InsecureSkipVerify: true,
	}, nil)
	if err != nil {
		t.Fatalf("construct live service: %v", err)
	}
	allowed := core.WithCallerAuthorization(context.Background(), core.CallerAuthorization{
		Authenticated: true, Permissions: map[core.Permission]bool{core.PermissionInfrastructureView: true},
	})
	for _, candidate := range New([]Source{{Name: "qa-splunk", Service: service}}) {
		t.Run(candidate.Name(), func(t *testing.T) {
			result, invokeErr := candidate.Invoke(allowed, json.RawMessage(`{"limit":20}`))
			if invokeErr != nil || result == nil || !result.Found {
				t.Errorf("live tool found=%t err=%v", result != nil && result.Found, invokeErr)
			}
		})
	}
}

func TestToolsPermissionScopeAndArguments(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		switch request.Method {
		case http.MethodPost:
			if request.FormValue("search") != "search index=main AND host=node" {
				t.Errorf("unexpected query %q", request.FormValue("search"))
			}
			fmt.Fprint(writer, `{"sid":"123.456"}`)
		case http.MethodDelete:
			writer.WriteHeader(http.StatusOK)
		default:
			if request.URL.Path == "/services/search/jobs/123.456" {
				fmt.Fprint(writer, `{"entry":[{"content":{"isDone":true}}]}`)
			} else {
				fmt.Fprint(writer, `{"results":[{"index":"main","_raw":"hello"}]}`)
			}
		}
	}))
	defer server.Close()
	service, err := splunkapp.NewService(config.AgentSplunkSourceConfig{Address: server.URL, Search: "index=main"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	tools := New([]Source{{Name: "one", Service: service}})
	for _, candidate := range tools {
		if candidate.(core.ContextAuthorizedTool).AuthorizedTool(context.Background()) != nil {
			t.Fatal("unauthorized catalog exposure")
		}
		denied, err := candidate.Invoke(context.Background(), json.RawMessage(`{"filters":{"host":"node"}}`))
		if err != nil || denied.IsAvailable() || requests != 0 {
			t.Fatalf("denied=%v err=%v requests=%d", denied, err, requests)
		}
	}
	allowed := core.WithCallerAuthorization(context.Background(), core.CallerAuthorization{Authenticated: true, Permissions: map[core.Permission]bool{core.PermissionInfrastructureView: true}})
	for _, input := range []string{`{"spl":"search index=other"}`, `{"filters":{"index":"other"}}`, `{"filters":{"host":"x|stats"}}`, `{"lookback_minutes":361}`, `{"search":"index=other"}`, `{"limit":51}`, `{"filters":{"host":"node"}}{"source":"one"}`} {
		if _, err := tools[1].Invoke(allowed, json.RawMessage(input)); err == nil || requests != 0 {
			t.Fatalf("accepted %q err=%v requests=%d", input, err, requests)
		}
	}
	result, err := tools[1].Invoke(allowed, json.RawMessage(`{"filters":{"host":"node"},"limit":1}`))
	if err != nil || !result.Found || requests != 4 {
		t.Fatalf("result=%v err=%v requests=%d", result, err, requests)
	}
}

func TestToolsRejectUnindexedResultsWithoutModelOutput(t *testing.T) {
	for _, scenario := range []struct {
		name string
		row  string
	}{
		{"missing", `{"_raw":"second"}`},
		{"wrong", `{"index":"other","_raw":"second"}`},
		{"blank", `{"index":"  ","_raw":"second"}`},
		{"numeric", `{"index":42,"_raw":"second"}`},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			deletes := 0
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				switch {
				case request.Method == http.MethodPost:
					fmt.Fprint(writer, `{"sid":"123.456"}`)
				case request.Method == http.MethodDelete:
					if request.URL.Path != "/services/search/jobs/123.456" {
						t.Errorf("unexpected cleanup path: %s", request.URL.Path)
					}
					deletes++
				case request.URL.Path == "/services/search/jobs/123.456":
					fmt.Fprint(writer, `{"entry":[{"content":{"isDone":true}}]}`)
				default:
					fmt.Fprintf(writer, `{"results":[{"index":"main","_raw":"first"},%s]}`, scenario.row)
				}
			}))
			defer server.Close()
			service, err := splunkapp.NewService(config.AgentSplunkSourceConfig{Address: server.URL, Search: "index=main"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			allowed := core.WithCallerAuthorization(context.Background(), core.CallerAuthorization{Authenticated: true, Permissions: map[core.Permission]bool{core.PermissionInfrastructureView: true}})
			for _, candidate := range New([]Source{{Name: "one", Service: service}}) {
				result, invokeErr := candidate.Invoke(allowed, json.RawMessage(`{}`))
				if invokeErr == nil || result != nil {
					t.Fatalf("%s exposed invalid results: result=%v err=%v", candidate.Name(), result, invokeErr)
				}
			}
			if deletes != 2 {
				t.Fatalf("expected both jobs deleted, got %d", deletes)
			}
		})
	}
}
