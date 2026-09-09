package elasticsearch

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/config"
)

func TestServiceScopesEveryOperationAndProjectsResults(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		paths = append(paths, request.URL.Path)
		switch {
		case strings.HasPrefix(request.URL.Path, "/_cat/indices/"):
			_, _ = writer.Write([]byte(`[{"index":"logs-2026","status":"green","docs.count":"7"}]`))
		case strings.HasSuffix(request.URL.Path, "/_mapping"):
			if got, want := request.URL.Query().Get("filter_path"), "*.mappings.properties.@timestamp,*.mappings.properties.event.properties.id,*.mappings.properties.message"; got != want {
				t.Errorf("mapping filter_path = %q, want %q", got, want)
			}
			_, _ = writer.Write([]byte(`{"logs-2026":{"mappings":{"properties":{"@timestamp":{"type":"date"},"message":{"type":"text"},"password":{"type":"keyword"},"user_email":{"type":"keyword"}}}}}`))
		case strings.HasPrefix(request.URL.Path, "/_cat/shards/"):
			_, _ = writer.Write([]byte(`[{"index":"logs-2026","shard":"0","prirep":"p","state":"STARTED","docs":"7","store":"1kb","node":"node-a"}]`))
		case strings.HasSuffix(request.URL.Path, "/_search"):
			var body map[string]any
			_ = json.NewDecoder(request.Body).Decode(&body)
			if body["size"] != float64(2) || body["track_total_hits"] != float64(searchTotalThreshold) || body["timeout"] != "5s" || body["terminate_after"] != float64(searchTerminateAfter) {
				t.Errorf("search body bounds = %#v", body)
			}
			if !strings.Contains(mustJSON(body["query"]), "service:api") {
				t.Errorf("configured query missing from %#v", body["query"])
			}
			_, _ = writer.Write([]byte(`{"hits":{"total":{"value":3,"relation":"eq"},"hits":[{"_id":"1","_source":{"@timestamp":"now","message":"ok","secret":"hidden"}},{"_id":"2","_source":{"message":"warn"}},{"_id":"3","_source":{"message":"bounded"}}]}}`))
		}
	}))
	defer server.Close()

	service, err := NewService(config.AgentElasticsearchSourceConfig{Addresses: []string{server.URL}, AllowLoopback: true, Index: "logs-*", Query: "service:api", TimeField: "@timestamp", MessageField: "message"})
	if err != nil {
		t.Fatal(err)
	}
	indices, err := service.ListIndices(context.Background(), 1)
	if err != nil || len(indices.Items) != 1 || indices.Items[0].Documents != 7 || indices.Total != 1 || indices.Truncated {
		t.Fatalf("indices = %#v, err = %v", indices, err)
	}
	mappings, err := service.Mappings(context.Background(), 2)
	if err != nil || len(mappings.Items) != 1 || len(mappings.Items[0].Fields) != 2 || mappings.Items[0].Fields[0].Name != "@timestamp" || mappings.Items[0].Fields[1].Name != "message" || mappings.TotalIndices != 1 || mappings.Truncated {
		t.Fatalf("mappings = %#v, err = %v", mappings, err)
	}
	if mustJSON(mappings.AllowedFields) != `["@timestamp","event.id","message"]` || mappings.AllowedFieldsTruncated {
		t.Fatalf("mapping allowed fields = %#v truncated=%v", mappings.AllowedFields, mappings.AllowedFieldsTruncated)
	}
	if encoded := mustJSON(mappings); strings.Contains(encoded, "password") || strings.Contains(encoded, "user_email") {
		t.Fatalf("mapping leaked unconfigured sensitive fields: %s", encoded)
	}
	shards, err := service.Shards(context.Background(), 1)
	if err != nil || len(shards.Items) != 1 || !shards.Items[0].Primary || shards.Total != 1 || shards.Truncated {
		t.Fatalf("shards = %#v, err = %v", shards, err)
	}
	result, err := service.Search(context.Background(), SearchOptions{QueryBody: json.RawMessage(`{"query":{"match":{"message":"error"}}}`), Fields: []string{"message"}, Limit: 2})
	if err != nil || result.Total != 3 || result.Relation != "eq" || !result.Truncated || len(result.Hits) != 2 || result.Hits[0].Fields["message"] != "ok" {
		t.Fatalf("search = %#v, err = %v", result, err)
	}
	if _, leaked := result.Hits[0].Fields["secret"]; leaked {
		t.Fatal("unconfigured source field leaked")
	}
	wantPaths := []string{"/_cat/indices/logs-*", "/logs-*/_mapping", "/_cat/shards/logs-*", "/logs-*/_search"}
	for index, path := range paths {
		if path != wantPaths[index] {
			t.Errorf("request path[%d] = %q, want %q", index, path, wantPaths[index])
		}
	}
}

func TestClientAuthFailoverBoundsAndSecretSafeErrors(t *testing.T) {
	const username, password, apiKey = "reader", "super-secret-password", "super-secret-key"
	failing := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Error(writer, password+apiKey, http.StatusInternalServerError)
	}))
	defer failing.Close()
	var authorization string
	healthy := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		authorization = request.Header.Get("Authorization")
		_, _ = writer.Write([]byte(`[]`))
	}))
	defer healthy.Close()

	client, err := NewClient(config.AgentElasticsearchSourceConfig{Addresses: []string{healthy.URL}, AllowLoopback: true, Index: "logs-*", Username: username, Password: password})
	if err != nil {
		t.Fatal(err)
	}
	client.setTestBounds(healthy.Client(), 0, 0)
	service := &Service{client: client}
	if _, err := service.ListIndices(context.Background(), 1); err != nil {
		t.Fatalf("failover: %v", err)
	}
	wantBasic := "Basic " + base64.StdEncoding.EncodeToString([]byte(username+":"+password))
	if authorization != wantBasic {
		t.Fatalf("authorization = %q", authorization)
	}

	apiClient, err := NewClient(config.AgentElasticsearchSourceConfig{Addresses: []string{healthy.URL}, AllowLoopback: true, Index: "logs-*", APIKey: apiKey})
	if err != nil {
		t.Fatal(err)
	}
	apiClient.setTestBounds(healthy.Client(), 0, 0)
	if err := apiClient.readJSON(context.Background(), apiClient.toolPolicy, http.MethodGet, "/_cat/indices", nil, nil, &[]any{}); err != nil {
		t.Fatal(err)
	}
	if authorization != "ApiKey "+apiKey {
		t.Fatalf("API key authorization = %q", authorization)
	}

	onlyFailing, _ := NewClient(config.AgentElasticsearchSourceConfig{Addresses: []string{failing.URL}, AllowLoopback: true, Index: "logs-*", APIKey: apiKey})
	onlyFailing.setTestBounds(failing.Client(), 0, 0)
	err = onlyFailing.readJSON(context.Background(), onlyFailing.toolPolicy, http.MethodGet, "/_cat/indices", nil, nil, &[]any{})
	if !errors.Is(err, ErrBackend) || strings.Contains(err.Error(), password) || strings.Contains(err.Error(), apiKey) {
		t.Fatalf("unsafe backend error = %v", err)
	}

	large := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = writer.Write([]byte(strings.Repeat("x", 128)))
	}))
	defer large.Close()
	bounded, _ := NewClient(config.AgentElasticsearchSourceConfig{Addresses: []string{large.URL}, AllowLoopback: true, Index: "logs-*"})
	bounded.setTestBounds(nil, 0, 16)
	if err := bounded.readJSON(context.Background(), bounded.toolPolicy, http.MethodGet, "/_cat/indices", nil, nil, &[]any{}); !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("large response error = %v", err)
	}
}

func TestClientFailoverDecodesSuccessfulResponseIntoFreshDestination(t *testing.T) {
	malformed := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = writer.Write([]byte(`{"name":"stale","fields":{"stale":"value"},"items":["stale"],"count":"wrong"}`))
	}))
	defer malformed.Close()
	healthy := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = writer.Write([]byte(`{"name":"fresh","count":2}`))
	}))
	defer healthy.Close()

	client, err := NewClient(config.AgentElasticsearchSourceConfig{
		Addresses: []string{malformed.URL, healthy.URL}, AllowLoopback: true, Index: "logs-*",
	})
	if err != nil {
		t.Fatal(err)
	}
	output := struct {
		Name   string            `json:"name"`
		Fields map[string]string `json:"fields"`
		Items  []string          `json:"items"`
		Count  int               `json:"count"`
	}{Fields: map[string]string{"caller": "value"}, Items: []string{"caller"}}
	if err := client.readJSON(context.Background(), client.toolPolicy, http.MethodGet, "/_mapping", nil, nil, &output); err != nil {
		t.Fatal(err)
	}
	if output.Name != "fresh" || output.Count != 2 || output.Fields != nil || output.Items != nil {
		t.Fatalf("failover output retained stale fields: %#v", output)
	}
}

func TestClientFailoverPreservesHighestValueError(t *testing.T) {
	server := func(status int, body string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			writer.WriteHeader(status)
			_, _ = writer.Write([]byte(body))
		}))
	}
	unauthorized := server(http.StatusUnauthorized, "unauthorized")
	defer unauthorized.Close()
	forbidden := server(http.StatusForbidden, "forbidden")
	defer forbidden.Close()
	notFound := server(http.StatusNotFound, "not found")
	defer notFound.Close()
	backend := server(http.StatusInternalServerError, "backend")
	defer backend.Close()
	tooLarge := server(http.StatusOK, strings.Repeat("x", 128))
	defer tooLarge.Close()

	tests := []struct {
		name      string
		addresses []string
		want      error
	}{
		{name: "authorization beats not found", addresses: []string{unauthorized.URL, notFound.URL}, want: ErrUnauthorized},
		{name: "later authorization beats backend", addresses: []string{backend.URL, forbidden.URL}, want: ErrForbidden},
		{name: "response size beats backend", addresses: []string{tooLarge.URL, backend.URL}, want: ErrResponseTooLarge},
		{name: "not found beats backend", addresses: []string{notFound.URL, backend.URL}, want: ErrNotFound},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client, err := NewClient(config.AgentElasticsearchSourceConfig{Addresses: test.addresses, AllowLoopback: true, Index: "logs-*"})
			if err != nil {
				t.Fatal(err)
			}
			client.setTestBounds(nil, 0, 16)
			err = client.readJSON(context.Background(), client.toolPolicy, http.MethodGet, "/_mapping", nil, nil, &map[string]any{})
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestSearchRejectsUnsafeAndUnboundedArguments(t *testing.T) {
	service, err := NewService(config.AgentElasticsearchSourceConfig{Addresses: []string{"http://localhost:9200"}, AllowLoopback: true, Index: "logs-*", MessageField: "message"})
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		`{"size":1000}`,
		`{"query":{"script":{"script":"return true"}}}`,
		`{"query":{"terms":{"user":{"index":"other","id":"1","path":"name"}}}}`,
		`{"query":{"match":{"password":"secret"}}}`,
		`{"query":{"wildcard":{"message":"err*"}}}`,
		`{"query":{"regexp":{"message":".*"}}}`,
		`{"query":{"fuzzy":{"message":"error"}}}`,
		`{"query":{"match":{"message":{"query":"error","fuzziness":"AUTO"}}}}`,
		`{"query":{"prefix":{"message":"err"}}}`,
		`{"query":{"nested":{"path":"message","query":{"match_all":{}}}}}`,
		`{"query":{"has_child":{"type":"child","query":{"match_all":{}}}}}`,
		`{"query":{"has_parent":{"parent_type":"parent","query":{"match_all":{}}}}}`,
		`{"query":{"percolate":{"field":"message","document":{}}}}`,
		`{"query":{"more_like_this":{"fields":["message"]}}}`,
		`{"query":{"term":{"_id":"one"}}}`,
		`{"sort":[{"password":"asc"}]}`,
		`{"sort":[{"@timestamp":{"order":"asc","missing":"_last"}}]}`,
		`{"sort":[{"@timestamp":{"missing":"_last"}}]}`,
		`{"sort":[{"@timestamp":{"order":"sideways"}}]}`,
		`{"sort":[{"@timestamp":{"order":"asc","nested":{"path":"host"}}}]}`,
		`{"aggs":{"all":{"terms":{"field":"message"}}}}`,
	} {
		if _, _, _, err := service.searchBody(SearchOptions{QueryBody: json.RawMessage(body)}, MaximumLimit); !errors.Is(err, ErrInvalidArguments) {
			t.Errorf("searchBody(%s) error = %v", body, err)
		}
	}
	if _, _, _, err := service.searchBody(SearchOptions{Fields: []string{"password"}}, 1); !errors.Is(err, ErrInvalidArguments) {
		t.Fatalf("unconfigured field error = %v", err)
	}
	oversized := json.RawMessage(`{"query":{"match":{"message":"` + strings.Repeat("x", maximumRequestBytes) + `"}}}`)
	if _, _, _, err := service.searchBody(SearchOptions{QueryBody: oversized}, 1); !errors.Is(err, ErrInvalidArguments) {
		t.Fatalf("oversized body error = %v", err)
	}
}

func TestClientEnforcesOperationTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		<-request.Context().Done()
	}))
	defer server.Close()
	client, err := NewClient(config.AgentElasticsearchSourceConfig{Addresses: []string{server.URL}, AllowLoopback: true, Index: "logs-*"})
	if err != nil {
		t.Fatal(err)
	}
	client.setTestBounds(nil, 20*time.Millisecond, 0)
	err = client.readJSON(context.Background(), client.toolPolicy, http.MethodGet, "/_cat/indices", nil, nil, &[]any{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout error = %v", err)
	}
}

func TestClientRejectsUnsafeCredentialTransportAndURLUserinfo(t *testing.T) {
	for _, test := range []struct {
		cfg    config.AgentElasticsearchSourceConfig
		reason string
	}{
		{cfg: config.AgentElasticsearchSourceConfig{Addresses: []string{"http://legacy:password@localhost:9200"}, AllowLoopback: true, Index: "logs-*"}, reason: "URL userinfo is forbidden"},
		{cfg: config.AgentElasticsearchSourceConfig{Addresses: []string{"http://localhost:9200"}, AllowLoopback: true, Index: "logs-*", Username: "reader", Password: "secret"}, reason: "credentials require verified HTTPS"},
		{cfg: config.AgentElasticsearchSourceConfig{Addresses: []string{"http://localhost:9200"}, AllowLoopback: true, Index: "logs-*", APIKey: "secret"}, reason: "credentials require verified HTTPS"},
		{cfg: config.AgentElasticsearchSourceConfig{Addresses: []string{"https://localhost:9200"}, AllowLoopback: true, Index: "logs-*", APIKey: "secret", InsecureSkipVerify: true}, reason: "insecure_skip_verify cannot be enabled"},
		{cfg: config.AgentElasticsearchSourceConfig{Addresses: []string{"https://es.example:9200"}, Index: "logs-*", Password: "secret"}, reason: "password requires username"},
	} {
		if _, err := NewClient(test.cfg); !errors.Is(err, ErrInvalidConfig) || !strings.Contains(err.Error(), test.reason) || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "password@") {
			t.Fatalf("unsafe config error = %v, want reason %q without secrets", err, test.reason)
		}
	}
	if _, err := NewClient(config.AgentElasticsearchSourceConfig{Addresses: []string{"http://localhost:9200"}, Index: "logs-*"}); !errors.Is(err, ErrInvalidConfig) || !strings.Contains(err.Error(), "allow_loopback: true") {
		t.Fatalf("loopback error = %v", err)
	}
	client, err := NewClient(config.AgentElasticsearchSourceConfig{Addresses: []string{"http://localhost:9200"}, AllowLoopback: true, Index: "logs-*"})
	if err != nil {
		t.Fatal(err)
	}
	transport, ok := client.httpClient.Transport.(*http.Transport)
	if !ok || transport.Proxy != nil || transport.TLSClientConfig.MinVersion != tls.VersionTLS12 {
		t.Fatalf("transport = %#v", transport)
	}
}

func TestClientEndpointPolicyRejectsSpecialNetworksAndAllowsPrivateServices(t *testing.T) {
	for _, address := range []string{
		"://malformed",
		"http://localhost:9200",
		"http://127.0.0.1:9200",
		"http://[::1]:9200",
		"http://169.254.169.254:9200",
		"http://[fe80::1]:9200",
		"http://metadata.google.internal:80",
	} {
		if _, err := NewClient(config.AgentElasticsearchSourceConfig{Addresses: []string{address}, Index: "logs-*"}); !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("address %q error = %v", address, err)
		}
	}
	for _, address := range []string{"http://10.0.0.10:9200", "http://192.168.1.10:9200", "https://es.internal.example:9200"} {
		if _, err := NewClient(config.AgentElasticsearchSourceConfig{Addresses: []string{address}, Index: "logs-*"}); err != nil {
			t.Errorf("internal address %q error = %v", address, err)
		}
	}
	for _, address := range []string{"0.0.0.0", "224.0.0.1", "169.254.1.1", "::", "ff02::1"} {
		if validResolvedAddress(net.ParseIP(address), false) {
			t.Errorf("resolved address %q allowed", address)
		}
	}
	if validResolvedAddress(net.ParseIP("127.0.0.1"), false) || !validResolvedAddress(net.ParseIP("127.0.0.1"), true) || !validResolvedAddress(net.ParseIP("10.0.0.10"), false) {
		t.Fatal("loopback/private resolved-address policy mismatch")
	}
}

func TestSearchAcceptsOnlySupportedOperatorShapes(t *testing.T) {
	service, err := NewService(config.AgentElasticsearchSourceConfig{Addresses: []string{"http://localhost:9200"}, AllowLoopback: true, Index: "logs-*", MessageField: "message"})
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		`{"query":{"match_all":{}}}`,
		`{"query":{"term":{"message":"error"}}}`,
		`{"query":{"terms":{"message":["error","warning"]}}}`,
		`{"query":{"range":{"@timestamp":{"gte":"now-15m","lt":"now"}}}}`,
		`{"query":{"match_phrase":{"message":"connection refused"}}}`,
		`{"query":{"match":{"message":"error"}}}`,
		`{"query":{"bool":{"must":[{"term":{"message":"error"}}],"must_not":{"match_phrase":{"message":"ignored"}}}}}`,
		`{"sort":[{"@timestamp":"desc"}]}`,
		`{"sort":[{"@timestamp":{"order":"asc"}}]}`,
	} {
		if _, _, _, err := service.searchBody(SearchOptions{QueryBody: json.RawMessage(body)}, 10); err != nil {
			t.Errorf("supported searchBody(%s) error = %v", body, err)
		}
	}
}

func TestClientUsesOneDeadlineAcrossFailoverAndStopsOnCancellation(t *testing.T) {
	blocked := func(writer http.ResponseWriter, request *http.Request) {
		<-request.Context().Done()
	}
	first := httptest.NewServer(http.HandlerFunc(blocked))
	defer first.Close()
	secondCalls := 0
	second := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		secondCalls++
		blocked(writer, request)
	}))
	defer second.Close()

	client, err := NewClient(config.AgentElasticsearchSourceConfig{Addresses: []string{first.URL, second.URL}, AllowLoopback: true, Index: "logs-*"})
	if err != nil {
		t.Fatal(err)
	}
	client.setTestBounds(nil, 20*time.Millisecond, 0)
	started := time.Now()
	err = client.readJSON(context.Background(), client.toolPolicy, http.MethodGet, "/_cat/indices", nil, nil, &[]any{})
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > 200*time.Millisecond || secondCalls != 0 {
		t.Fatalf("shared deadline err=%v elapsed=%v second_calls=%d", err, time.Since(started), secondCalls)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = client.readJSON(ctx, client.toolPolicy, http.MethodGet, "/_cat/indices", nil, nil, &[]any{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("parent cancellation error = %v", err)
	}
}

func TestIngestAllowsStructuredPageOverOneMiBWhileToolsRemainBounded(t *testing.T) {
	payload := `{"hits":{"hits":[{"_source":{"message":"` + strings.Repeat("x", (1<<20)+1024) + `"}}]}}`
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = writer.Write([]byte(payload))
	}))
	defer server.Close()
	client, err := NewClient(config.AgentElasticsearchSourceConfig{Addresses: []string{server.URL}, AllowLoopback: true, Index: "logs-*"})
	if err != nil {
		t.Fatal(err)
	}
	var output any
	if err := client.SearchJSON(context.Background(), []byte(`{}`), &output); !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("tool response error = %v", err)
	}
	if err := client.SearchIngestJSON(context.Background(), []byte(`{}`), &output); err != nil {
		t.Fatalf("ingest response error = %v", err)
	}
	if client.ingestPolicy.timeout != 30*time.Second || client.toolPolicy.timeout != 10*time.Second {
		t.Fatalf("operation policies = tool %v ingest %v", client.toolPolicy, client.ingestPolicy)
	}
}

type replacingScrubber struct{}

func (replacingScrubber) Scrub(value string) string {
	return strings.ReplaceAll(value, "secret", "<redacted>")
}

func TestServiceRecursivelyScrubsSearchValuesAndShardNodes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if strings.Contains(request.URL.Path, "_cat/shards") {
			_, _ = writer.Write([]byte(`[{"index":"logs","shard":"0","prirep":"p","state":"STARTED","node":"secret-node"}]`))
			return
		}
		_, _ = writer.Write([]byte(`{"hits":{"total":{"value":1,"relation":"eq"},"hits":[{"_id":"secret-id","_source":{"message":{"nested":["secret-value"]}}}]}}`))
	}))
	defer server.Close()
	service, err := NewService(config.AgentElasticsearchSourceConfig{Addresses: []string{server.URL}, AllowLoopback: true, Index: "logs-*", MessageField: "message"})
	if err != nil {
		t.Fatal(err)
	}
	service.SetScrubber(replacingScrubber{})
	result, err := service.Search(context.Background(), SearchOptions{Fields: []string{"message"}, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(result)
	if strings.Contains(string(encoded), "secret") {
		t.Fatalf("search result leaked secret: %s", encoded)
	}
	shards, err := service.Shards(context.Background(), 1)
	if err != nil || strings.Contains(shards.Items[0].Node, "secret") {
		t.Fatalf("shards = %#v, err = %v", shards, err)
	}
}

func TestBoundedStringPreservesUTF8(t *testing.T) {
	got := boundedString("  日日日  ", 2)
	if got != "日日" || !json.Valid([]byte(`{"value":"`+got+`"}`)) {
		t.Fatalf("bounded UTF-8 string = %q", got)
	}
}

func TestDiscoveryResultsDiscloseIndexAndFieldTruncation(t *testing.T) {
	properties := make(map[string]map[string]string, MaximumMappingFieldsPerIndex+1)
	extraFields := make([]string, 0, MaximumMappingFieldsPerIndex+1)
	for index := 0; index <= MaximumMappingFieldsPerIndex; index++ {
		field := fmt.Sprintf("field_%02d", index)
		properties[field] = map[string]string{"type": "keyword"}
		extraFields = append(extraFields, field)
	}
	mappingBody, _ := json.Marshal(map[string]any{
		"logs-a": map[string]any{"mappings": map[string]any{"properties": properties}},
		"logs-b": map[string]any{"mappings": map[string]any{"properties": map[string]any{}}},
	})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case strings.Contains(request.URL.Path, "_cat/indices"):
			_, _ = writer.Write([]byte(`[{"index":"a"},{"index":"b"}]`))
		case strings.Contains(request.URL.Path, "_cat/shards"):
			_, _ = writer.Write([]byte(`[{"index":"a","shard":"0"},{"index":"b","shard":"0"}]`))
		default:
			_, _ = writer.Write(mappingBody)
		}
	}))
	defer server.Close()
	service, err := NewService(config.AgentElasticsearchSourceConfig{Addresses: []string{server.URL}, AllowLoopback: true, Index: "logs-*", ExtraFields: extraFields})
	if err != nil {
		t.Fatal(err)
	}
	indices, err := service.ListIndices(context.Background(), 1)
	if err != nil || indices.Total != 2 || !indices.Truncated || len(indices.Items) != 1 {
		t.Fatalf("indices = %#v, err = %v", indices, err)
	}
	shards, err := service.Shards(context.Background(), 1)
	if err != nil || shards.Total != 2 || !shards.Truncated || len(shards.Items) != 1 {
		t.Fatalf("shards = %#v, err = %v", shards, err)
	}
	mappings, err := service.Mappings(context.Background(), 1)
	if err != nil || mappings.TotalIndices != 2 || !mappings.Truncated || len(mappings.Items) != 1 {
		t.Fatalf("mappings = %#v, err = %v", mappings, err)
	}
	if mappings.Items[0].TotalFields != MaximumMappingFieldsPerIndex+1 || !mappings.Items[0].Truncated || len(mappings.Items[0].Fields) != MaximumMappingFieldsPerIndex {
		t.Fatalf("mapping field bounds = %#v", mappings.Items[0])
	}
	if len(mappings.AllowedFields) != MaximumFields || !mappings.AllowedFieldsTruncated || !sort.StringsAreSorted(mappings.AllowedFields) {
		t.Fatalf("mapping allowed field bounds = %#v truncated=%v", mappings.AllowedFields, mappings.AllowedFieldsTruncated)
	}
}

func TestMappingQueryNarrowsConfiguredDottedFields(t *testing.T) {
	service, err := NewService(config.AgentElasticsearchSourceConfig{
		Addresses: []string{"http://localhost:9200"}, AllowLoopback: true, Index: "logs-*",
		TimeField: "@timestamp", MessageField: "message", ExtraFields: []string{"http.response.status_code"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "*.mappings.properties.@timestamp,*.mappings.properties.event.properties.id,*.mappings.properties.http.properties.response.properties.status_code,*.mappings.properties.message"
	if got := service.mappingQuery().Get("filter_path"); got != want {
		t.Fatalf("filter_path = %q, want %q", got, want)
	}
}

func TestSearchTruncatesOversizedDefaultProjectionDeterministically(t *testing.T) {
	extraFields := make([]string, 0, MaximumFields+3)
	for index := 0; index < MaximumFields+3; index++ {
		extraFields = append(extraFields, fmt.Sprintf("field_%02d", index))
	}
	var projected []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var body struct {
			Source []string `json:"_source"`
		}
		_ = json.NewDecoder(request.Body).Decode(&body)
		projected = body.Source
		_, _ = writer.Write([]byte(`{"hits":{"total":{"value":1,"relation":"eq"},"hits":[{"_id":"one","_source":{"message":"ok"}}]}}`))
	}))
	defer server.Close()
	service, err := NewService(config.AgentElasticsearchSourceConfig{
		Addresses: []string{server.URL}, AllowLoopback: true, Index: "logs-*", ExtraFields: extraFields,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Search(context.Background(), SearchOptions{Limit: 1})
	if err != nil || !result.Truncated {
		t.Fatalf("search = %#v, err = %v", result, err)
	}
	allFields := append([]string{"@timestamp", "event.id", "message"}, extraFields...)
	sort.Strings(allFields)
	want := allFields[:MaximumFields]
	if mustJSON(projected) != mustJSON(want) {
		t.Fatalf("default projection = %v, want %v", projected, want)
	}
	if _, _, _, err := service.searchBody(SearchOptions{Fields: allFields[:MaximumFields+1]}, 1); !errors.Is(err, ErrInvalidArguments) {
		t.Fatalf("caller-supplied oversized projection error = %v", err)
	}
	for _, queryBody := range []json.RawMessage{json.RawMessage(`{"_source":[]}`), json.RawMessage(`{"_source":null}`)} {
		_, fields, truncated, err := service.searchBody(SearchOptions{QueryBody: queryBody}, 1)
		if err != nil || !truncated || mustJSON(fields) != mustJSON(want) {
			t.Fatalf("default projection for %s fields=%v truncated=%v err=%v", queryBody, fields, truncated, err)
		}
	}
}

func TestNewServiceRejectsInvalidConfiguredFields(t *testing.T) {
	base := config.AgentElasticsearchSourceConfig{Addresses: []string{"http://localhost:9200"}, AllowLoopback: true, Index: "logs-*"}
	tests := []struct {
		name   string
		mutate func(*config.AgentElasticsearchSourceConfig)
		field  string
	}{
		{name: "time wildcard", field: "time_field", mutate: func(cfg *config.AgentElasticsearchSourceConfig) { cfg.TimeField = "time.*" }},
		{name: "message wildcard", field: "message_field", mutate: func(cfg *config.AgentElasticsearchSourceConfig) { cfg.MessageField = "message*" }},
		{name: "severity metadata", field: "severity_field", mutate: func(cfg *config.AgentElasticsearchSourceConfig) { cfg.SeverityField = "_source" }},
		{name: "extra wildcard", field: "extra_fields[0]", mutate: func(cfg *config.AgentElasticsearchSourceConfig) { cfg.ExtraFields = []string{"labels.*"} }},
		{name: "extra empty segment", field: "extra_fields[0]", mutate: func(cfg *config.AgentElasticsearchSourceConfig) { cfg.ExtraFields = []string{"labels..name"} }},
		{name: "message whitespace", field: "message_field", mutate: func(cfg *config.AgentElasticsearchSourceConfig) { cfg.MessageField = "log body" }},
		{name: "tie breaker metadata", field: "tie_breaker_field", mutate: func(cfg *config.AgentElasticsearchSourceConfig) { cfg.TieBreakerField = "_id" }},
		{name: "same sort fields", field: "tie_breaker_field", mutate: func(cfg *config.AgentElasticsearchSourceConfig) {
			cfg.TimeField = "event.created"
			cfg.TieBreakerField = "event.created"
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := base
			test.mutate(&cfg)
			if _, err := NewService(cfg); !errors.Is(err, ErrInvalidConfig) || !strings.Contains(err.Error(), test.field) {
				t.Fatalf("error = %v, want invalid-config error naming %s", err, test.field)
			}
		})
	}
}

func TestServiceExposesNormalizedProjectedFieldsWithoutAliasing(t *testing.T) {
	service, err := NewService(config.AgentElasticsearchSourceConfig{
		Addresses: []string{"http://localhost:9200"}, AllowLoopback: true, Index: "logs-*",
		TimeField: " @timestamp ", TieBreakerField: " ingest.sequence ", MessageField: " message ", ExtraFields: []string{" service.name ", "message"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := mustJSON(service.ProjectedFields()), `["@timestamp","ingest.sequence","message","service.name"]`; got != want {
		t.Fatalf("projected fields = %s, want %s", got, want)
	}
	configured := service.Config()
	configured.ExtraFields[0] = "mutated"
	if got := mustJSON(service.ProjectedFields()); strings.Contains(got, "mutated") {
		t.Fatalf("service projection aliased returned config: %s", got)
	}
}

func TestDecodeTotalRestrictsRelation(t *testing.T) {
	for _, test := range []struct {
		body string
		want string
	}{
		{body: `7`, want: "eq"},
		{body: `{"value":7,"relation":"eq"}`, want: "eq"},
		{body: `{"value":7,"relation":"gte"}`, want: "gte"},
		{body: `{"value":7,"relation":"backend-extension"}`, want: "gte"},
		{body: `{"value":7}`, want: "gte"},
	} {
		if _, got := decodeTotal(json.RawMessage(test.body)); got != test.want {
			t.Errorf("decodeTotal(%s) relation = %q, want %q", test.body, got, test.want)
		}
	}
}

func TestSearchDisclosesServerSidePartialResults(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = writer.Write([]byte(`{"timed_out":true,"terminated_early":true,"hits":{"total":{"value":1,"relation":"eq"},"hits":[{"_id":"one","_source":{"message":"partial"}}]}}`))
	}))
	defer server.Close()
	service, err := NewService(config.AgentElasticsearchSourceConfig{Addresses: []string{server.URL}, AllowLoopback: true, Index: "logs-*", MessageField: "message"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Search(context.Background(), SearchOptions{Limit: 1})
	if err != nil || !result.TimedOut || !result.Truncated || result.Relation != "eq" {
		t.Fatalf("partial search = %#v, err = %v", result, err)
	}
}

func mustJSON(value any) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}
