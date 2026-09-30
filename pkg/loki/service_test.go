package loki

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/config"
)

func TestSelectorRejectsPipelineAndKeepsAllMatchers(t *testing.T) {
	for _, query := range []string{`{app="api"} |= "error"`, `{app="api"} or {app="other"}`, `{app="api",env=~"["}`, `{app="api"} [5m]`, `{app="api",broken=}`, `{app=~".*"}`} {
		if _, err := parseSelector(query); err == nil && query != `{app=~".*"}` {
			t.Errorf("accepted %q", query)
		}
	}
	for _, query := range []string{`{app="api",app!="internal"}`, `{app=~"api|worker",env!~"dev|test"}`} {
		scope, err := parseSelector(query)
		if err != nil || renderSelector(scope) != query {
			t.Fatalf("round-trip %q: %q %v", query, renderSelector(scope), err)
		}
	}
}

func TestReadIntersectsScopeAndBoundsInterleavedStreams(t *testing.T) {
	const token = "LOKI_SECRET_7ab9"
	var got url.Values
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		got = request.URL.Query()
		if request.Header.Get("X-Scope-OrgID") != "tenant" || request.Header.Get("Authorization") != "Bearer "+token {
			t.Errorf("missing tenant or auth")
		}
		fmt.Fprintf(writer, `{"status":"success","data":{"resultType":"streams","result":[{"stream":{"service":"api","env":"prod","leak":"%s"},"values":[["2000000000","later %s"],["1000000000","earlier"]]},{"stream":{"service":"api"},"values":[["3000000000","newest"]]}]}}`, token, token)
	}))
	defer server.Close()
	service, err := NewService(config.AgentLokiSourceConfig{Address: server.URL, Query: `{service=~"api|worker",service!="worker",env!~"dev|test"}`, TenantID: "tenant", BearerToken: token}, nil)
	const secret = "SUPER_SECRET_TOKEN"
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return time.Unix(5, 0) }
	result, err := service.Read(context.Background(), ReadRequest{Service: "api", Labels: map[string]string{"env": "prod"}, Search: `"} |~ ".*`, Lookback: time.Hour, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	query := got.Get("query")
	for _, part := range []string{`service=~"api|worker"`, `service!="worker"`, `service="api"`, `env!~"dev|test"`, `env="prod"`, `|= "\"} |~ \".*"`} {
		if !strings.Contains(query, part) {
			t.Errorf("query missing %q: %s", part, query)
		}
	}
	if strings.Count(query, "|=") != 1 || got.Get("direction") != "backward" || got.Get("limit") != "2" {
		t.Fatalf("unsafe query: %s; args: %v", query, got)
	}
	if result.Count != 1 || result.Records[0].Message != "newest" || !result.Truncated || result.NextOffset != 1 || !slices.Contains(result.Truncation, "page_limit") {
		t.Fatalf("result = %+v", result)
	}
	second, err := service.Read(context.Background(), ReadRequest{Service: "api", Offset: result.NextOffset, Limit: 1})
	if err != nil || second.Count != 1 || !strings.Contains(second.Records[0].Message, "[redacted]") {
		t.Fatalf("second = %+v, %v", second, err)
	}
	encoded, _ := json.Marshal(second)
	if strings.Contains(string(encoded), token) {
		t.Fatal("secret leaked in response")
	}
}

func TestReadServiceLabelMappingIntersectsScope(t *testing.T) {
	var query string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		query = request.URL.Query().Get("query")
		fmt.Fprint(writer, `{"status":"success","data":{"resultType":"streams","result":[]}}`)
	}))
	defer server.Close()
	for _, item := range []struct{ label, scope string }{
		{"service_name", `{service_name=~"api|worker",service_name!="worker",env="prod"}`},
		{"app", `{app=~"api|worker",app!="worker",env="prod"}`},
	} {
		service, err := NewService(config.AgentLokiSourceConfig{Address: server.URL, Query: item.scope, ServiceLabel: item.label}, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, err = service.Read(context.Background(), ReadRequest{Service: `api"} |= ".*`, Limit: 1})
		if err != nil {
			t.Fatal(err)
		}
		matchers, parseErr := parseSelector(query)
		if parseErr != nil || len(matchers) != 4 || matchers[3].name != item.label || matchers[3].literal != `"api\"} |= \".*"` || !strings.Contains(query, item.scope[1:len(item.scope)-1]) {
			t.Fatalf("mapped query = %q; matchers=%+v, %v", query, matchers, parseErr)
		}
	}
}

func TestReadRequiresExplicitMappingForOtherScopedLabels(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		requests++
		fmt.Fprint(writer, `{"status":"success","data":{"resultType":"streams","result":[]}}`)
	}))
	defer server.Close()
	service, err := NewService(config.AgentLokiSourceConfig{Address: server.URL, Query: `{app="api"}`}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Read(context.Background(), ReadRequest{Service: "api"}); !errors.Is(err, ErrInvalidArgument) || requests != 0 {
		t.Fatalf("unmapped service: %v; requests=%d", err, requests)
	}
	if _, err := service.Read(context.Background(), ReadRequest{}); err != nil || requests != 1 {
		t.Fatalf("scoped read without service: %v; requests=%d", err, requests)
	}
	for _, label := range []string{`app|=`, `app"}`, "1service", "service\nother"} {
		if _, err := NewService(config.AgentLokiSourceConfig{Address: server.URL, Query: `{app="api"}`, ServiceLabel: label}, nil); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("accepted label %q: %v", label, err)
		}
	}
}

func TestReadByteLimitRetainsCursorBeforeRowBound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(writer, `{"status":"success","data":{"resultType":"streams","result":[{"stream":{"app":"api"},"values":[["3","first"],["2","second"],["1","third"]]}]}}`)
	}))
	defer server.Close()
	service, err := NewService(config.AgentLokiSourceConfig{Address: server.URL, Query: `{app="api"}`}, nil)
	if err != nil {
		t.Fatal(err)
	}
	service.policy.MaxRows = 3
	service.policy.MaxBytes = encodedSize([]Record{{Timestamp: time.Unix(0, 3), Message: "first", Labels: map[string]string{"app": "api"}}})
	first, err := service.Read(context.Background(), ReadRequest{Limit: 3})
	if err != nil || first.Count != 1 || first.NextOffset != 1 || !slices.Contains(first.Truncation, "byte_limit") || !slices.Contains(first.Truncation, "row_limit") {
		t.Fatalf("first page = %+v, %v", first, err)
	}
	last, err := service.Read(context.Background(), ReadRequest{Offset: 2, Limit: 1})
	if err != nil || last.Count != 1 || last.NextOffset != 0 || !slices.Contains(last.Truncation, "row_limit") {
		t.Fatalf("last page = %+v, %v", last, err)
	}
}

func TestDiscoveryWithholdsScopeAndPaginatesUnscopedLabels(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/loki/api/v1/labels" && request.URL.Path != "/loki/api/v1/label/service/values" {
			t.Errorf("unexpected path %q", request.URL.Path)
		}
		fmt.Fprint(writer, `{"status":"success","data":["service","severity","service","env"]}`)
	}))
	defer server.Close()
	unscoped, err := NewService(config.AgentLokiSourceConfig{Address: server.URL, Query: "{}"}, nil)
	if err != nil || !unscoped.DiscoveryAvailable() {
		t.Fatalf("unscoped: %v", err)
	}
	result, err := unscoped.Discover(context.Background(), DiscoveryRequest{Search: "s", Limit: 1})
	if err != nil || result.Count != 1 || result.NextOffset != 1 || !slices.Contains(result.Truncation, "page_limit") {
		t.Fatalf("discovery = %+v, %v", result, err)
	}
	values, err := unscoped.Discover(context.Background(), DiscoveryRequest{Label: "service", Offset: 1, Limit: 1})
	if err != nil || values.Count != 1 {
		t.Fatalf("values = %+v, %v", values, err)
	}
	if _, err := unscoped.Read(context.Background(), ReadRequest{}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("unfiltered read: %v", err)
	}
	scoped, err := NewService(config.AgentLokiSourceConfig{Address: server.URL, Query: `{service="api"}`}, nil)
	if err != nil || !scoped.DiscoveryAvailable() {
		t.Fatalf("scoped: %v", err)
	}
}

func TestScopedDiscoveryUsesSelectorBoundSeries(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls++
		if request.URL.Path != "/loki/api/v1/series" || request.URL.Query().Get("match[]") != `{app="api",env!~"dev|test"}` {
			t.Errorf("unsafe scoped discovery: %s?%s", request.URL.Path, request.URL.RawQuery)
		}
		fmt.Fprint(writer, `{"status":"success","data":[{"app":"api","env":"prod"},{"app":"api","env":"staging"}]}`)
	}))
	defer server.Close()
	service, err := NewService(config.AgentLokiSourceConfig{Address: server.URL, Query: `{app="api",env!~"dev|test"}`}, nil)
	if err != nil {
		t.Fatal(err)
	}
	fields, err := service.Discover(context.Background(), DiscoveryRequest{Search: "env"})
	if err != nil || !slices.Equal(fields.Labels, []string{"env"}) {
		t.Fatalf("fields = %+v, %v", fields, err)
	}
	values, err := service.Discover(context.Background(), DiscoveryRequest{Label: "env", Search: "stag"})
	if err != nil || !slices.Equal(values.Labels, []string{"staging"}) || calls != 2 {
		t.Fatalf("values = %+v, %v; calls=%d", values, err, calls)
	}
}

func TestReadRejectsInjectionAndScrubsFailures(t *testing.T) {
	const secret = "SUPER_SECRET_TOKEN"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusFound)
		fmt.Fprint(writer, secret)
	}))
	defer server.Close()
	service, err := NewService(config.AgentLokiSourceConfig{Address: server.URL, Query: `{app="api"}`, BearerToken: secret}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, request := range []ReadRequest{{Labels: map[string]string{`app|=`: "x"}}, {Service: "\nother"}, {Offset: MaximumOffset}, {Lookback: -time.Minute}} {
		if _, err := service.Read(context.Background(), request); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("accepted %+v: %v", request, err)
		}
	}
	_, err = service.Read(context.Background(), ReadRequest{Labels: map[string]string{"app": "api"}})
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("unsafe error: %v", err)
	}
	for _, query := range []string{`{app="api"} | json`, `{app=~".*"}`} {
		if _, err := NewService(config.AgentLokiSourceConfig{Address: server.URL, Query: query}, nil); !errors.Is(err, ErrUnsupportedScope) {
			t.Fatalf("unsafe configured scope %q: %v", query, err)
		}
	}
}

func TestReadCanceledBeforeEgress(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		requests++
		fmt.Fprint(writer, `{"status":"success","data":{"resultType":"streams","result":[]}}`)
	}))
	defer server.Close()
	service, err := NewService(config.AgentLokiSourceConfig{Address: server.URL, Query: `{app="api"}`}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := service.Read(ctx, ReadRequest{}); err == nil || requests != 0 {
		t.Fatalf("canceled read: err=%v requests=%d", err, requests)
	}
}

func TestIndependentOutputBoundsAndRedirectRefusal(t *testing.T) {
	redirected := 0
	destination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected++ }))
	defer destination.Close()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/redirect" {
			http.Redirect(writer, request, destination.URL, http.StatusFound)
			return
		}
		if request.URL.Path == "/loki/api/v1/labels" {
			fmt.Fprint(writer, `{"status":"success","data":["alpha","beta","gamma"]}`)
			return
		}
		fmt.Fprint(writer, `{"status":"success","data":{"resultType":"streams","result":[{"stream":{"one":"1","two":"2"},"values":[["3","first"],["2","second"],["1","third"]]}]}}`)
	}))
	defer server.Close()
	service, err := NewService(config.AgentLokiSourceConfig{Address: server.URL, Query: "{}"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	service.policy.MaxFields = 1
	service.policy.MaxRows = 2
	service.policy.MaxBytes = 160
	result, err := service.Read(context.Background(), ReadRequest{Service: "api", Limit: 2, Lookback: 24 * time.Hour})
	if err != nil || !slices.Contains(result.Truncation, "field_limit") || !slices.Contains(result.Truncation, "row_limit") || !slices.Contains(result.Truncation, "lookback_limit") {
		t.Fatalf("bounds = %+v, %v", result, err)
	}
	service.policy.MaxBytes = 25
	bytesLimited, err := service.Read(context.Background(), ReadRequest{Service: "api"})
	if err != nil || !slices.Contains(bytesLimited.Truncation, "byte_limit") {
		t.Fatalf("byte bound = %+v, %v", bytesLimited, err)
	}
	service.policy.MaxBytes = 160
	discovery, err := service.Discover(context.Background(), DiscoveryRequest{Limit: 3})
	if err != nil || !slices.Contains(discovery.Truncation, "field_limit") {
		t.Fatalf("field discovery bound = %+v, %v", discovery, err)
	}
	if _, err := service.get(context.Background(), "/redirect", nil); err == nil || redirected != 0 {
		t.Fatalf("redirect followed: %v, %d", err, redirected)
	}
	if _, err := NewService(config.AgentLokiSourceConfig{Address: "http://example.com", Query: `{app="api"}`, BearerToken: "secret"}, nil); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("credentialed non-loopback HTTP accepted: %v", err)
	}
}
