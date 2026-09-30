package tempo

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
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
)

const traceID = "abcdef0123456789abcdef0123456789"

func fixture(serviceName string, count int) string {
	spans := make([]map[string]any, count)
	for index := range spans {
		spanID := fmt.Sprintf("%016x", index+1)
		parent := ""
		if index > 0 {
			parent = fmt.Sprintf("%016x", index)
		}
		spans[index] = map[string]any{"traceId": traceID, "spanId": spanID, "parentSpanId": parent, "name": "GET /secret", "startTimeUnixNano": "1000000", "endTimeUnixNano": "2000000", "status": map[string]any{"code": 2}}
	}
	encoded, _ := json.Marshal(map[string]any{"batches": []any{map[string]any{"resource": map[string]any{"attributes": []any{map[string]any{"key": "service.name", "value": map[string]any{"stringValue": serviceName}}}}, "scopeSpans": []any{map[string]any{"spans": spans}}}}})
	return string(encoded)
}

func TestReadBuildsFixedSelectorAndKeepsExplicitParentIDs(t *testing.T) {
	var query string
	var traceWindow url.Values
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/api/search" {
			query = request.URL.Query().Get("q")
			fmt.Fprintf(writer, `{"traces":[{"traceID":"%s","startTimeUnixNano":"1000000"}]}`, traceID)
			return
		}
		traceWindow = request.URL.Query()
		fmt.Fprint(writer, fixture(`api"} || {service="admin`, 2))
	}))
	defer server.Close()
	service, err := NewService(Config{Address: server.URL, AllowLoopback: true, Now: func() time.Time { return time.Unix(100, 0) }})
	if err != nil {
		t.Fatal(err)
	}
	name := `api"} || {service="admin`
	result, err := service.Read(context.Background(), ReadRequest{Service: name, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if query != `{ resource.service.name = "api\"} || {service=\"admin" }` || result.Count != 1 || !result.Truncated || result.NextOffset != 0 || result.Spans[0].SpanID != "0000000000000001" {
		t.Fatalf("query=%q result=%+v", query, result)
	}
	if _, err := service.Read(context.Background(), ReadRequest{Service: name, Offset: 1}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("unstable search page accepted: %v", err)
	}
	result, err = service.Read(context.Background(), ReadRequest{Service: name, TraceID: traceID, Offset: 1, Limit: 1})
	if err != nil || result.Count != 1 || result.Spans[0].ParentSpanID != "0000000000000001" {
		t.Fatalf("trace page two=%+v err=%v", result, err)
	}
	first, err := service.Read(context.Background(), ReadRequest{Service: name, TraceID: traceID, Limit: 1})
	if err != nil || first.NextOffset != 1 || strings.Join(first.Truncation, ",") != "page_limit" {
		t.Fatalf("trace page one=%+v err=%v", first, err)
	}
	if traceWindow.Get("start") != "-3500" || traceWindow.Get("end") != "100" {
		t.Fatalf("unbounded trace fetch: %v", traceWindow)
	}
	if _, err := service.ProjectTrace(context.Background(), traceID); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("tree error=%v", err)
	}
}

func TestScopedDiscoveryWithheldAndReadCannotEscapeScope(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) { calls++; fmt.Fprint(writer, `{}`) }))
	defer server.Close()
	service, err := NewService(Config{Address: server.URL, AllowLoopback: true, ScopeService: "api"})
	if err != nil {
		t.Fatal(err)
	}
	if service.DiscoveryAvailable() {
		t.Fatal("scoped discovery exposed")
	}
	if _, err := service.Discover(context.Background(), FieldRequest{}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("discover err=%v", err)
	}
	if _, err := service.Read(context.Background(), ReadRequest{Service: "other"}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("read err=%v", err)
	}
	if _, err := service.CandidateTraceIDs(context.Background(), CandidateRequest{}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("empty candidate scope err=%v", err)
	}
	if _, err := service.CandidateTraceIDs(context.Background(), CandidateRequest{Service: "other"}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("cross-scope candidate err=%v", err)
	}
	if calls != 0 {
		t.Fatalf("unexpected network calls: %d", calls)
	}
}

func TestCandidateTraceIDsScopedSelectorAndBounds(t *testing.T) {
	name := `api"} || {service="admin`
	var query url.Values
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/search" {
			t.Errorf("unexpected path: %s", request.URL.Path)
		}
		query = request.URL.Query()
		fmt.Fprint(writer, `{"traces":[{"traceID":"b"},{"traceID":"a"},{"traceID":"0000000000000000000000000000000a"}]}`)
	}))
	defer server.Close()
	policy := ToolPolicy()
	policy.MaxTraces = 2
	service, _ := NewService(Config{Address: server.URL, AllowLoopback: true, ScopeService: name, Policy: policy, Now: func() time.Time { return time.Unix(100, 0) }})
	result, err := service.CandidateTraceIDs(context.Background(), CandidateRequest{Service: name, Lookback: 24 * time.Hour})
	if err != nil || !result.Truncated || !slices.Equal(result.TraceIDs, []string{strings.Repeat("0", 31) + "a", strings.Repeat("0", 31) + "b"}) {
		t.Fatalf("candidates=%+v err=%v", result, err)
	}
	if query.Get("q") != `{ resource.service.name = "api\"} || {service=\"admin" }` || query.Get("limit") != "3" || query.Get("start") != "-21500" || query.Get("end") != "100" {
		t.Fatalf("unbounded or unsafe selector: %v", query)
	}
	if _, err := service.CandidateTraceIDs(context.Background(), CandidateRequest{Service: ""}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("empty scope err=%v", err)
	}
	unscoped, _ := NewService(Config{Address: server.URL, AllowLoopback: true})
	if _, err := unscoped.CandidateTraceIDs(context.Background(), CandidateRequest{Service: name}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("unconfigured scope err=%v", err)
	}
}

func TestCandidateTraceIDsRejectsBadSourcesAndTransportFailures(t *testing.T) {
	for _, response := range []string{`{"traces":[{"traceID":"bad-id"}]}`, `{"traces":[{"traceID":"a"},{"traceID":"b"},{"traceID":"c"}]}`, `{"traces":"wrong"}`, `{"traces":null`, `null`, strings.Repeat("x", 1000)} {
		t.Run(response[:min(len(response), 20)], func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) { fmt.Fprint(writer, response) }))
			defer server.Close()
			policy := ToolPolicy()
			policy.MaxTraces = 1
			policy.MaxBytes = 256
			service, _ := NewService(Config{Address: server.URL, AllowLoopback: true, ScopeService: "api", Policy: policy})
			if _, err := service.CandidateTraceIDs(context.Background(), CandidateRequest{Service: "api"}); err == nil {
				t.Fatal("accepted unsafe candidate response")
			}
		})
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) { <-request.Context().Done() }))
	defer server.Close()
	policy := ToolPolicy()
	policy.Timeout = 5 * time.Millisecond
	service, _ := NewService(Config{Address: server.URL, AllowLoopback: true, ScopeService: "api", Policy: policy})
	if _, err := service.CandidateTraceIDs(context.Background(), CandidateRequest{Service: "api"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline err=%v", err)
	}
}

func TestCandidateTraceIDsEmptySearchResults(t *testing.T) {
	for _, response := range []string{`{"metrics":{"completedJobs":1}}`, `{"traces":null}`, `{"traces":[]}`} {
		t.Run(response, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) { fmt.Fprint(writer, response) }))
			defer server.Close()
			service, _ := NewService(Config{Address: server.URL, AllowLoopback: true, ScopeService: "api"})
			result, err := service.CandidateTraceIDs(context.Background(), CandidateRequest{Service: "api"})
			if err != nil || len(result.TraceIDs) != 0 || result.Truncated {
				t.Fatalf("candidates=%+v err=%v", result, err)
			}
			read, err := service.Read(context.Background(), ReadRequest{Service: "api"})
			if err != nil || read.Count != 0 || read.Traces != 0 || read.Truncated {
				t.Fatalf("read=%+v err=%v", read, err)
			}
		})
	}
}

func TestCandidateTraceIDsHardCapAndCanonicalOutputBytes(t *testing.T) {
	traces := make([]map[string]string, 51)
	for index := range traces {
		traces[index] = map[string]string{"traceID": fmt.Sprintf("%x", index+1)}
	}
	body, _ := json.Marshal(map[string]any{"traces": traces})
	var limit string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		limit = request.URL.Query().Get("limit")
		writer.Write(body)
	}))
	defer server.Close()
	policy := ToolPolicy()
	policy.MaxTraces = 100
	service, _ := NewService(Config{Address: server.URL, AllowLoopback: true, ScopeService: "api", Policy: policy})
	result, err := service.CandidateTraceIDs(context.Background(), CandidateRequest{Service: "api"})
	if err != nil || limit != "51" || len(result.TraceIDs) != MaximumLimit || !result.Truncated || result.TraceIDs[0] != strings.Repeat("0", 30)+"01" {
		t.Fatalf("candidate cap=%+v limit=%s err=%v", result, limit, err)
	}
	policy.MaxTraces = 10
	policy.MaxBytes = 220
	service, _ = NewService(Config{Address: server.URL, AllowLoopback: true, ScopeService: "api", Policy: policy})
	if _, err := service.CandidateTraceIDs(context.Background(), CandidateRequest{Service: "api"}); !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("byte bound err=%v", err)
	}
}

func TestScopedProjectTraceKeepsCrossServiceTreeAndRejectsNonmembers(t *testing.T) {
	var value map[string]any
	if err := json.Unmarshal([]byte(fixture("api", 1)), &value); err != nil {
		t.Fatal(err)
	}
	var other map[string]any
	if err := json.Unmarshal([]byte(fixture("worker", 1)), &other); err != nil {
		t.Fatal(err)
	}
	child := other["batches"].([]any)[0].(map[string]any)["scopeSpans"].([]any)[0].(map[string]any)["spans"].([]any)[0].(map[string]any)
	child["spanId"] = "0000000000000002"
	child["parentSpanId"] = "0000000000000001"
	value["batches"] = append(value["batches"].([]any), other["batches"].([]any)...)
	body, _ := json.Marshal(value)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) { writer.Write(body) }))
	defer server.Close()
	service, _ := NewService(Config{Address: server.URL, AllowLoopback: true, ScopeService: "api"})
	tree, err := service.ProjectTrace(context.Background(), traceID)
	if err != nil || len(tree.Spans) != 2 || tree.Spans[0].Service != "api" || tree.Spans[1].Service != "worker" || tree.Spans[1].ParentSpanID != tree.Spans[0].SpanID || tree.Spans[0].TraceID != traceID {
		t.Fatalf("cross-service tree=%+v err=%v", tree, err)
	}
	service, _ = NewService(Config{Address: server.URL, AllowLoopback: true, ScopeService: "unrelated"})
	if tree, err := service.ProjectTrace(context.Background(), traceID); !errors.Is(err, ErrUnsupported) || len(tree.Spans) != 0 {
		t.Fatalf("nonmember tree=%+v err=%v", tree, err)
	}
	policy := ToolPolicy()
	policy.MaxSpans = 1
	service, _ = NewService(Config{Address: server.URL, AllowLoopback: true, ScopeService: "api", Policy: policy})
	if _, err := service.ProjectTrace(context.Background(), traceID); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("oversized tree err=%v", err)
	}
}

func TestDiscoverySortsAndPaginatesAndHonorsFieldCap(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Query().Get("scope") == "span" {
			fmt.Fprint(writer, `{"scopes":[{"name":"span","tags":["z","a","z"]}]}`)
		} else {
			fmt.Fprint(writer, `{"scopes":[{"name":"resource","tags":["service.name"]}]}`)
		}
	}))
	defer server.Close()
	policy := ToolPolicy()
	policy.MaxFields = 2
	service, _ := NewService(Config{Address: server.URL, AllowLoopback: true, Policy: policy})
	result, err := service.Discover(context.Background(), FieldRequest{Limit: 1})
	if err != nil || result.Count != 1 || result.Fields[0] != "resource.service.name" || result.NextOffset != 1 || strings.Join(result.Truncation, ",") != "page_limit,field_limit" {
		t.Fatalf("fields=%+v err=%v", result, err)
	}
}

func TestTransportBoundsAndSanitizesErrors(t *testing.T) {
	const secret = "TEMPO_SECRET_CANARY"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/api/search" {
			writer.WriteHeader(500)
			fmt.Fprint(writer, secret)
			return
		}
		fmt.Fprint(writer, strings.Repeat("x", 200))
	}))
	defer server.Close()
	policy := ToolPolicy()
	policy.MaxBytes = 32
	service, _ := NewService(Config{Address: server.URL, BearerToken: secret, AllowLoopback: true, Policy: policy})
	_, err := service.Read(context.Background(), ReadRequest{Service: "api"})
	if !errors.Is(err, ErrBackend) || strings.Contains(err.Error(), secret) {
		t.Fatalf("unsafe backend error=%v", err)
	}
	_, err = service.get(context.Background(), "/api/traces/"+traceID, nil)
	if !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("byte limit error=%v", err)
	}
	for _, address := range []string{"http://169.254.169.254", "http://user:pass@example.com", "http://127.0.0.1"} {
		if _, err := NewService(Config{Address: address}); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("allowed %s: %v", address, err)
		}
	}
}

func TestServiceCredentialedHTTPRequiresLoopbackOptIn(t *testing.T) {
	for _, auth := range []struct {
		name   string
		config Config
	}{
		{name: "bearer", config: Config{BearerToken: "secret"}},
		{name: "basic", config: Config{Username: "user", Password: "secret"}},
		{name: "password only", config: Config{Password: "secret"}},
	} {
		t.Run(auth.name, func(t *testing.T) {
			for _, test := range []struct {
				name          string
				address       string
				allowLoopback bool
				allowPrivate  bool
				wantAllowed   bool
			}{
				{name: "public HTTP", address: "http://example.com"},
				{name: "private HTTP", address: "http://10.0.0.2", allowPrivate: true},
				{name: "private HTTP with loopback opt-in", address: "http://10.0.0.2", allowPrivate: true, allowLoopback: true},
				{name: "localhost without opt-in", address: "http://localhost:3200"},
				{name: "localhost with opt-in", address: "http://localhost:3200", allowLoopback: true, wantAllowed: true},
				{name: "localhost with private opt-in", address: "http://localhost:3200", allowLoopback: true, allowPrivate: true, wantAllowed: true},
				{name: "loopback IP with opt-in", address: "http://127.0.0.1:3200", allowLoopback: true, wantAllowed: true},
				{name: "HTTPS", address: "https://example.com", wantAllowed: true},
			} {
				t.Run(test.name, func(t *testing.T) {
					config := auth.config
					config.Address = test.address
					config.AllowLoopback = test.allowLoopback
					config.AllowPrivate = test.allowPrivate
					service, err := NewService(config)
					if test.wantAllowed && (err != nil || service == nil) || !test.wantAllowed && !errors.Is(err, ErrInvalidConfig) {
						t.Fatalf("NewService(%q): service=%v err=%v", test.address, service, err)
					}
				})
			}
		})
	}
	for _, address := range []string{"http://example.com", "http://10.0.0.2"} {
		service, err := NewService(Config{Address: address, AllowPrivate: true})
		if err != nil || service == nil {
			t.Fatalf("uncredentialed HTTP %q: service=%v err=%v", address, service, err)
		}
	}
}

func TestServiceCredentialsStayOnOriginAndHTTPSUsesTLS12(t *testing.T) {
	for _, auth := range []struct {
		name   string
		config Config
		want   string
	}{
		{name: "bearer", config: Config{BearerToken: "secret"}, want: "Bearer secret"},
		{name: "basic", config: Config{Username: "user", Password: "secret"}, want: "Basic " + base64.StdEncoding.EncodeToString([]byte("user:secret"))},
	} {
		t.Run(auth.name, func(t *testing.T) {
			forwarded := 0
			target := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) { forwarded++ }))
			defer target.Close()
			seen := ""
			origin := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				seen = request.Header.Get("Authorization")
				http.Redirect(writer, request, target.URL, http.StatusFound)
			}))
			defer origin.Close()
			config := auth.config
			config.Address = strings.Replace(origin.URL, "127.0.0.1", "localhost", 1)
			config.AllowLoopback = true
			config.AllowPrivate = true
			service, err := NewService(config)
			if err != nil {
				t.Fatal(err)
			}
			connection, dialErr := service.client.Transport.(*http.Transport).DialContext(context.Background(), "tcp", "10.0.0.2:3200")
			if connection != nil {
				connection.Close()
			}
			if dialErr == nil || !strings.Contains(dialErr.Error(), "not permitted") {
				t.Fatalf("credentialed HTTP dial to private address: %v", dialErr)
			}
			if _, err := service.get(context.Background(), "/api/search", nil); !errors.Is(err, ErrBackend) || seen != auth.want || forwarded != 0 {
				t.Fatalf("redirect err=%v authorization=%q forwarded=%d", err, seen, forwarded)
			}

			secure := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				seen = request.Header.Get("Authorization")
				fmt.Fprint(writer, `{}`)
			}))
			defer secure.Close()
			roots := x509.NewCertPool()
			roots.AddCert(secure.Certificate())
			config.Address = secure.URL
			config.AllowPrivate = false
			config.RootCAs = roots
			service, err = NewService(config)
			if err != nil {
				t.Fatal(err)
			}
			if service.client.Transport.(*http.Transport).TLSClientConfig.MinVersion != tls.VersionTLS12 {
				t.Fatal("HTTPS transport permits TLS below 1.2")
			}
			if _, err := service.get(context.Background(), "/api/search", nil); err != nil || seen != auth.want {
				t.Fatalf("HTTPS err=%v authorization=%q", err, seen)
			}
		})
	}
}

func TestProjectTraceRejectsIncompleteSpanTree(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) { fmt.Fprint(writer, fixture("api", 2)) }))
	defer server.Close()
	policy := ToolPolicy()
	policy.MaxSpans = 1
	service, _ := NewService(Config{Address: server.URL, AllowLoopback: true, Policy: policy})
	if _, err := service.ProjectTrace(context.Background(), traceID); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("partial tree error=%v", err)
	}
}

func TestProjectTraceCompleteTree(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) { fmt.Fprint(writer, fixture("api", 2)) }))
	defer server.Close()
	service, _ := NewService(Config{Address: server.URL, AllowLoopback: true})
	tree, err := service.ProjectTrace(context.Background(), traceID)
	if err != nil || tree.TraceID != traceID || len(tree.Spans) != 2 || tree.Spans[1].ParentSpanID != tree.Spans[0].SpanID {
		t.Fatalf("tree=%+v err=%v", tree, err)
	}
}

func TestProjectTraceCompleteness(t *testing.T) {
	decode := func(t *testing.T, body string) map[string]any {
		t.Helper()
		var value map[string]any
		if err := json.Unmarshal([]byte(body), &value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	firstBatch := func(value map[string]any) map[string]any { return value["batches"].([]any)[0].(map[string]any) }
	firstSpan := func(value map[string]any) map[string]any {
		return firstBatch(value)["scopeSpans"].([]any)[0].(map[string]any)["spans"].([]any)[0].(map[string]any)
	}
	tests := []struct {
		name       string
		count      int
		limit      int
		change     func(map[string]any)
		ok         bool
		wantParent string
	}{
		{name: "exact cap", count: 2, limit: 2, ok: true, wantParent: "0000000000000001"},
		{name: "cap plus one", count: 3, limit: 2},
		{name: "multiple roots across batches", count: 1, change: func(value map[string]any) {
			other := decode(t, fixture("worker", 1))
			firstSpan(other)["spanId"] = "0000000000000002"
			value["batches"] = append(value["batches"].([]any), other["batches"].([]any)...)
		}},
		{name: "same service child across batches", count: 1, ok: true, change: func(value map[string]any) {
			other := decode(t, fixture("api", 1))
			firstSpan(other)["spanId"] = "0000000000000002"
			firstSpan(other)["parentSpanId"] = "0000000000000001"
			value["batches"] = append(value["batches"].([]any), other["batches"].([]any)...)
		}, wantParent: "0000000000000001"},
		{name: "missing sampled parent", count: 1, change: func(value map[string]any) { firstSpan(value)["parentSpanId"] = "0000000000000002" }},
		{name: "cycle", count: 2, change: func(value map[string]any) { firstSpan(value)["parentSpanId"] = "0000000000000002" }},
		{name: "duplicate ID", count: 2, change: func(value map[string]any) {
			firstBatch(value)["scopeSpans"].([]any)[0].(map[string]any)["spans"].([]any)[1].(map[string]any)["spanId"] = "0000000000000001"
		}},
		{name: "invalid ID", count: 1, change: func(value map[string]any) { firstSpan(value)["spanId"] = "invalid" }},
		{name: "missing trace ID", count: 1, change: func(value map[string]any) { delete(firstSpan(value), "traceId") }},
		{name: "wrong trace ID", count: 1, change: func(value map[string]any) { firstSpan(value)["traceId"] = "abcdef0123456789abcdef0123456788" }},
		{name: "zero span ID", count: 1, change: func(value map[string]any) { firstSpan(value)["spanId"] = "0000000000000000" }},
		{name: "unsafe service", count: 1, change: func(value map[string]any) {
			firstBatch(value)["resource"].(map[string]any)["attributes"].([]any)[0].(map[string]any)["value"].(map[string]any)["stringValue"] = "api\nforged"
		}},
		{name: "duplicate service", count: 1, change: func(value map[string]any) {
			attrs := firstBatch(value)["resource"].(map[string]any)["attributes"].([]any)
			firstBatch(value)["resource"].(map[string]any)["attributes"] = append(attrs, attrs[0])
		}},
		{name: "empty batch", count: 1, change: func(value map[string]any) {
			value["batches"] = append(value["batches"].([]any), map[string]any{"resource": map[string]any{}, "scopeSpans": []any{}})
		}},
		{name: "empty scope", count: 1, change: func(value map[string]any) {
			firstBatch(value)["scopeSpans"] = append(firstBatch(value)["scopeSpans"].([]any), map[string]any{"spans": []any{}})
		}},
		{name: "malformed batch", count: 1, change: func(value map[string]any) { firstBatch(value)["scopeSpans"] = "invalid" }},
		{name: "span field cap", count: 1, limit: 1, change: func(value map[string]any) { firstSpan(value)["attributes"] = []any{map[string]any{}, map[string]any{}} }},
		{name: "scope field cap", count: 1, limit: 1, change: func(value map[string]any) {
			firstBatch(value)["scopeSpans"].([]any)[0].(map[string]any)["attributes"] = []any{map[string]any{}, map[string]any{}}
		}},
		{name: "resource field cap", count: 1, limit: 1, change: func(value map[string]any) {
			attrs := firstBatch(value)["resource"].(map[string]any)["attributes"].([]any)
			firstBatch(value)["resource"].(map[string]any)["attributes"] = append(attrs, map[string]any{"key": "extra"})
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value := decode(t, fixture("api", test.count))
			if test.change != nil {
				test.change(value)
			}
			body, _ := json.Marshal(value)
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) { writer.Write(body) }))
			defer server.Close()
			policy := ToolPolicy()
			if test.limit > 0 {
				policy.MaxSpans = test.limit
				policy.MaxFields = test.limit
			}
			service, _ := NewService(Config{Address: server.URL, AllowLoopback: true, Policy: policy})
			tree, err := service.ProjectTrace(context.Background(), traceID)
			if test.ok {
				if err != nil || len(tree.Spans) != len(value["batches"].([]any))+test.count-1 {
					t.Fatalf("tree=%+v err=%v", tree, err)
				}
				if len(tree.Spans) == 2 && (tree.Spans[0].ParentSpanID != "" || tree.Spans[1].ParentSpanID != test.wantParent) {
					t.Fatalf("unexpected parent links: %+v", tree.Spans)
				}
			} else if !errors.Is(err, ErrUnsupported) || len(tree.Spans) != 0 {
				t.Fatalf("unsafe tree=%+v err=%v", tree, err)
			}
		})
	}
}

func TestProjectTraceBoundsAndScope(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Query().Get("start") == "" || request.URL.Query().Get("end") == "" {
			t.Error("missing bounded time window")
		}
		fmt.Fprint(writer, fixture("api", 1))
	}))
	defer server.Close()
	policy := ToolPolicy()
	policy.MaxBytes = 32
	service, _ := NewService(Config{Address: server.URL, AllowLoopback: true, Policy: policy})
	if _, err := service.ProjectTrace(context.Background(), traceID); !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("byte limit error=%v", err)
	}
	service, _ = NewService(Config{Address: server.URL, AllowLoopback: true, ScopeService: "api"})
	if tree, err := service.ProjectTrace(context.Background(), traceID); err != nil || len(tree.Spans) != 1 {
		t.Fatalf("scoped projection tree=%+v error=%v", tree, err)
	}
	service, _ = NewService(Config{Address: server.URL, AllowLoopback: true})
	if _, err := service.ProjectTrace(context.Background(), "invalid"); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("invalid trace ID error=%v", err)
	}
	if _, err := service.get(withByteBudget(context.Background(), 0), "/api/traces/"+traceID, nil); !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("exhausted byte budget error=%v", err)
	}
}

func TestReadTruncatesSpanCapAndBoundsTotalFetchedBytes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/api/search" {
			fmt.Fprintf(writer, `{"traces":[{"traceID":"%s"}]}`, traceID)
			return
		}
		fmt.Fprint(writer, fixture("api", 3))
	}))
	defer server.Close()
	policy := ToolPolicy()
	policy.MaxSpans = 2
	service, _ := NewService(Config{Address: server.URL, AllowLoopback: true, Policy: policy})
	result, err := service.Read(context.Background(), ReadRequest{Service: "api", TraceID: traceID})
	if err != nil || result.Count != 2 || !result.Truncated || result.NextOffset != 0 || strings.Join(result.Truncation, ",") != "span_limit" {
		t.Fatalf("capped=%+v err=%v", result, err)
	}
	policy.MaxBytes = int64(len(fixture("api", 3)) + 4)
	service, _ = NewService(Config{Address: server.URL, AllowLoopback: true, Policy: policy})
	_, err = service.Read(context.Background(), ReadRequest{Service: "api"})
	if !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("total byte limit error=%v", err)
	}
}

func TestReadCappedTracePageKeepsNextOffset(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		fmt.Fprint(writer, strings.Replace(fixture("api", 101), `"name":"GET /secret"`, `"name":"POST"`, 50))
	}))
	defer server.Close()
	service, _ := NewService(Config{Address: server.URL, AllowLoopback: true})
	result, err := service.Read(context.Background(), ReadRequest{Service: "api", TraceID: traceID, Limit: 50})
	if err != nil || result.Count != 50 || result.NextOffset != 50 || !slices.Contains(result.Truncation, "span_limit") {
		t.Fatalf("capped page=%+v err=%v", result, err)
	}
	last, err := service.Read(context.Background(), ReadRequest{Service: "api", TraceID: traceID, Offset: 50, Limit: 50})
	if err != nil || last.Count != 50 || last.NextOffset != 0 || !slices.Contains(last.Truncation, "span_limit") {
		t.Fatalf("last accessible page=%+v err=%v", last, err)
	}
	filtered, err := service.Read(context.Background(), ReadRequest{Service: "api", TraceID: traceID, Operation: "POST", Limit: 50})
	if err != nil || filtered.Count != 50 || filtered.NextOffset != 0 || !slices.Contains(filtered.Truncation, "span_limit") {
		t.Fatalf("filtered cap page=%+v err=%v", filtered, err)
	}
}

func TestReadTempoV1WireIDsAndStatuses(t *testing.T) {
	shortID := "abc123"
	fullID := strings.Repeat("0", 26) + shortID
	traceBytes, _ := hex.DecodeString(fullID)
	spanBytes, _ := hex.DecodeString("0000000000000001")
	spans := []map[string]any{
		{"traceId": base64.StdEncoding.EncodeToString(traceBytes), "spanId": base64.StdEncoding.EncodeToString(spanBytes), "name": "GET", "startTimeUnixNano": "1000000", "endTimeUnixNano": "2000000", "status": map[string]any{"code": "STATUS_CODE_ERROR"}},
		{"traceId": fullID, "spanId": "0000000000000002", "parentSpanId": base64.StdEncoding.EncodeToString(spanBytes), "name": "POST", "startTimeUnixNano": "1000000", "endTimeUnixNano": "2000000", "status": map[string]any{"code": 1}},
	}
	response, _ := json.Marshal(map[string]any{"batches": []any{map[string]any{"resource": map[string]any{"attributes": []any{map[string]any{"key": "service.name", "value": map[string]any{"stringValue": "api"}}}}, "scopeSpans": []any{map[string]any{"spans": spans}}}}})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/api/search" {
			fmt.Fprintf(writer, `{"traces":[{"traceID":%q}]}`, shortID)
			return
		}
		if request.URL.Path != "/api/traces/"+shortID {
			t.Errorf("unexpected trace URL: %s", request.URL.Path)
		}
		writer.Write(response)
	}))
	defer server.Close()
	service, _ := NewService(Config{Address: server.URL, AllowLoopback: true})
	for _, request := range []ReadRequest{{Service: "api"}, {Service: "api", TraceID: shortID}} {
		result, err := service.Read(context.Background(), request)
		if err != nil || result.Count != 2 || !result.Spans[0].Error || result.Spans[1].Error || result.Spans[1].ParentSpanID != "0000000000000001" {
			t.Fatalf("wire trace=%+v err=%v", result, err)
		}
	}
	tree, err := service.ProjectTrace(context.Background(), shortID)
	if err != nil || tree.TraceID != fullID || len(tree.Spans) != 2 || tree.Spans[0].TraceID != fullID || tree.Spans[1].ParentSpanID != tree.Spans[0].SpanID {
		t.Fatalf("wire tree=%+v err=%v", tree, err)
	}
}

func TestReadKeepsCompletedTracesWhenLaterTraceExhaustsBytes(t *testing.T) {
	secondID := "abcdef0123456789abcdef0123456788"
	search := fmt.Sprintf(`{"traces":[{"traceID":%q,"startTimeUnixNano":"2"},{"traceID":%q,"startTimeUnixNano":"1"}]}`, traceID, secondID)
	first := fixture("api", 1)
	second := strings.ReplaceAll(fixture("api", 1), traceID, secondID)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/search":
			fmt.Fprint(writer, search)
		case "/api/traces/" + traceID:
			fmt.Fprint(writer, first)
		default:
			fmt.Fprint(writer, second)
		}
	}))
	defer server.Close()
	policy := ToolPolicy()
	policy.MaxBytes = int64(len(search) + len(first) + len(second) - 1)
	service, _ := NewService(Config{Address: server.URL, AllowLoopback: true, Policy: policy})
	result, err := service.Read(context.Background(), ReadRequest{Service: "api"})
	if err != nil || result.Count != 1 || result.Traces != 1 || result.Spans[0].TraceID != traceID || !result.Truncated || strings.Join(result.Truncation, ",") != "byte_limit" || result.NextOffset != 0 {
		t.Fatalf("partial traces=%+v err=%v", result, err)
	}
}

func TestDiscoverEnforcesExecutionTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		<-request.Context().Done()
	}))
	defer server.Close()
	policy := ToolPolicy()
	policy.Timeout = 5 * time.Millisecond
	service, err := NewService(Config{Address: server.URL, AllowLoopback: true, Policy: policy})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Discover(context.Background(), FieldRequest{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout error = %v", err)
	}
}

func TestProjectTraceEnforcesExecutionTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		<-request.Context().Done()
	}))
	defer server.Close()
	policy := ToolPolicy()
	policy.Timeout = 5 * time.Millisecond
	service, _ := NewService(Config{Address: server.URL, AllowLoopback: true, Policy: policy})
	if _, err := service.ProjectTrace(context.Background(), traceID); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout error=%v", err)
	}
}
