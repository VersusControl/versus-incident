package graylog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/config"
)

type collisionScrubber struct{}

func (collisionScrubber) Scrub(value string) string {
	return strings.ReplaceAll(value, "[redacted]", "redacted")
}

func TestScopedSampleAndRead(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		if request.URL.Path != "/api/search/universal/absolute" || request.URL.Query().Get("filter") != "streams:stream-1" || request.URL.Query().Get("offset") != "0" || request.Header.Get("X-Requested-By") == "" {
			t.Errorf("missing Graylog request scope or header")
		}
		if user, password, ok := request.BasicAuth(); !ok || user != "secret" || password != "token" {
			t.Errorf("missing token auth")
		}
		query := request.URL.Query().Get("query")
		if requests == 1 && query != `service:"api"` {
			t.Errorf("discovery query = %q", query)
		}
		if requests == 2 && query != `service:"api" AND level:"3" AND message:"error"` && query != `service:"api" AND message:"error" AND level:"3"` {
			t.Errorf("read query = %q", query)
		}
		fmt.Fprint(writer, `{"total_results":4,"messages":[{"message":{"timestamp":"2026-01-01T00:00:00Z","message":"secret error","service":"api","level":3}}]}`)
	}))
	defer server.Close()
	service, err := NewService(config.AgentGraylogSourceConfig{Address: server.URL, Query: "service:api", StreamID: "stream-1", APIToken: "secret"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	discovery, err := service.Discover(context.Background(), DiscoveryRequest{Limit: 2})
	if err != nil || !discovery.Truncated || discovery.Coverage != "recent_sample" || discovery.NextOffset != 2 {
		t.Fatalf("discovery = %+v, %v", discovery, err)
	}
	read, err := service.Read(context.Background(), ReadRequest{Filters: map[string]string{"level": "3"}, Search: "error", Limit: 1})
	if err != nil || len(read.Records) != 1 || strings.Contains(fmt.Sprint(read), "secret") || !read.Truncated || read.NextOffset != 1 {
		t.Fatalf("read = %+v, %v", read, err)
	}
	if requests != 2 {
		t.Fatalf("requests = %d", requests)
	}
}

func TestRejectUnsafeInputsBeforeNetwork(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
	defer server.Close()
	for _, query := range []string{"service:api OR level:3", "service:api AND message:*"} {
		if _, err := NewService(config.AgentGraylogSourceConfig{Address: server.URL, Query: query}, nil); !errors.Is(err, ErrUnsupportedScope) {
			t.Fatalf("query %q: %v", query, err)
		}
	}
	for _, cfg := range []config.AgentGraylogSourceConfig{
		{Address: "http://example.com", APIToken: "secret"},
		{Address: server.URL, StreamID: "one OR two"},
		{Address: server.URL + "/api", Query: "*"},
	} {
		if _, err := NewService(cfg, nil); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("config accepted: %v", err)
		}
	}
	service, err := NewService(config.AgentGraylogSourceConfig{Address: server.URL, Query: "*"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, request := range []ReadRequest{{Filters: map[string]string{"service": `api" OR *`}}, {Filters: map[string]string{"service|x": "api"}}, {Search: "foo*"}, {Offset: 100}, {Limit: 101}} {
		if _, err := service.Read(context.Background(), request); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("read accepted: %+v: %v", request, err)
		}
	}
	if _, err := service.Discover(context.Background(), DiscoveryRequest{Offset: 100}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatal("discovery offset accepted")
	}
	if requests != 0 {
		t.Fatalf("unsafe inputs sent %d requests", requests)
	}
}

func TestSafeErrorsAndCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Error(writer, "secret upstream body", http.StatusForbidden)
	}))
	defer server.Close()
	service, err := NewService(config.AgentGraylogSourceConfig{Address: server.URL, APIToken: "secret"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Read(context.Background(), ReadRequest{})
	if err == nil || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), server.URL) || !strings.Contains(err.Error(), "403") {
		t.Fatalf("unsafe error: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := service.Discover(ctx, DiscoveryRequest{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel = %v", err)
	}
}

func TestRedactionPaginationAndResponseBound(t *testing.T) {
	secret := `quote"secret`
	var large atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if large.Load() {
			writer.Write(make([]byte, maximumResponseBytes+1))
			return
		}
		if request.URL.Query().Get("limit") != "2" || request.URL.Query().Get("offset") != "1" {
			t.Errorf("server page = %s", request.URL.RawQuery)
		}
		fmt.Fprint(writer, `{"total_results":3,"messages":[{"message":{"message":"quote\"secret"}},{"message":{"message":"next"}}]}`)
	}))
	defer server.Close()
	service, err := NewService(config.AgentGraylogSourceConfig{Address: server.URL, Password: secret, Username: "user"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Read(context.Background(), ReadRequest{Offset: 1, Limit: 1})
	if err != nil || result.Count != 1 || result.NextOffset != 2 || !result.Truncated || strings.Contains(fmt.Sprint(result), secret) {
		t.Fatalf("pagination/redaction = %+v, %v", result, err)
	}
	large.Store(true)
	partial, err := service.Read(context.Background(), ReadRequest{})
	if err != nil || partial.Count != 0 || !strings.Contains(fmt.Sprint(partial.Truncation), "byte_limit") {
		t.Fatalf("large response = %+v, %v", partial, err)
	}
}

func TestClippedValuesReportTruncation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		fmt.Fprintf(writer, `{"total_results":1,"messages":[{"message":{"message":%q}}]}`, strings.Repeat("x", 5000))
	}))
	defer server.Close()
	service, err := NewService(config.AgentGraylogSourceConfig{Address: server.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Read(context.Background(), ReadRequest{})
	if err != nil || result.Count != 1 || !result.Truncated || !strings.Contains(fmt.Sprint(result.Truncation), "value_limit") || len(result.Records[0].Message) != 4096 {
		t.Fatalf("clipped result = %+v, %v", result, err)
	}
}

func TestOversizedLookaheadPreservesPageAndScope(t *testing.T) {
	var pages []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		query := request.URL.Query()
		if query.Get("query") != `service:"api" AND level:"ERROR"` || query.Get("filter") != "streams:stream-1" || query.Get("fields") != "message,timestamp,service" {
			t.Errorf("scope/projection = %s", request.URL.RawQuery)
		}
		pages = append(pages, query.Get("offset")+":"+query.Get("limit"))
		if query.Get("limit") != "1" {
			writer.Write(make([]byte, maximumResponseBytes+1))
			return
		}
		if query.Get("offset") == "0" {
			fmt.Fprint(writer, `{"total_results":2,"messages":[{"message":{"message":"good","service":"api"}}]}`)
			return
		}
		writer.Write(make([]byte, maximumResponseBytes+1))
	}))
	defer server.Close()
	service, err := NewService(config.AgentGraylogSourceConfig{Address: server.URL, Query: "service:api", StreamID: "stream-1", Fields: []string{"service"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Read(context.Background(), ReadRequest{Filters: map[string]string{"level": "ERROR"}, Limit: 1})
	if err != nil || result.Count != 1 || result.Records[0].Message != "good" || !strings.Contains(fmt.Sprint(result.Truncation), "byte_limit") || result.NextOffset != 0 {
		t.Fatalf("partial page = %+v, %v", result, err)
	}
	if !strings.Contains(fmt.Sprint(pages), "1:1") {
		t.Fatalf("lookahead not fetched: %v", pages)
	}
}

func TestWideRowsStopAtRequestBudgetWithoutSkipping(t *testing.T) {
	const total = 25
	wide := strings.Repeat("x", 260<<10)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		offset, _ := strconv.Atoi(request.URL.Query().Get("offset"))
		limit, _ := strconv.Atoi(request.URL.Query().Get("limit"))
		messages := make([]any, 0, limit)
		for index := offset; index < min(total, offset+limit); index++ {
			messages = append(messages, map[string]any{"message": map[string]any{"message": strconv.Itoa(index), "wide": wide}})
		}
		json.NewEncoder(writer).Encode(map[string]any{"total_results": total, "messages": messages})
	}))
	defer server.Close()
	service, err := NewService(config.AgentGraylogSourceConfig{Address: server.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	service.policy.Timeout = time.Second
	start := time.Now()
	page, err := service.Read(context.Background(), ReadRequest{Limit: total})
	if err != nil || calls.Load() > maximumSearchRequests || time.Since(start) > 2*time.Second || page.Count == 0 || page.Count >= total || !page.Partial || !slices.Contains(page.Truncation, "request_limit") || page.NextOffset != page.Count {
		t.Fatalf("bounded page = %+v, calls=%d, elapsed=%s, err=%v", page, calls.Load(), time.Since(start), err)
	}
	for index, record := range page.Records {
		if record.Message != strconv.Itoa(index) {
			t.Fatalf("row %d = %q", index, record.Message)
		}
	}
	calls.Store(0)
	next, err := service.Read(context.Background(), ReadRequest{Offset: page.NextOffset, Limit: total - page.Count})
	if err != nil || calls.Load() > maximumSearchRequests || next.Count != total-page.Count || next.NextOffset != 0 {
		t.Fatalf("continuation = %+v, calls=%d, err=%v", next, calls.Load(), err)
	}
	for index, record := range next.Records {
		if record.Message != strconv.Itoa(page.Count+index) {
			t.Fatalf("continuation row %d = %q", page.Count+index, record.Message)
		}
	}
}

func TestInvocationDeadlineAndCanceledBeforeEgress(t *testing.T) {
	var calls atomic.Int32
	var discovery atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		if !discovery.Load() && request.URL.Query().Get("limit") != "1" {
			writer.Write(make([]byte, maximumResponseBytes+1))
			return
		}
		select {
		case <-time.After(30 * time.Millisecond):
		case <-request.Context().Done():
			return
		}
		offset, _ := strconv.Atoi(request.URL.Query().Get("offset"))
		fmt.Fprintf(writer, `{"total_results":25,"messages":[{"message":{"message":%q}}]}`, strconv.Itoa(offset))
	}))
	defer server.Close()
	service, err := NewService(config.AgentGraylogSourceConfig{Address: server.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	service.policy.Timeout = 75 * time.Millisecond
	start := time.Now()
	page, err := service.Read(context.Background(), ReadRequest{Limit: 25})
	if err != nil || page.Count == 0 || !page.Partial || !slices.Contains(page.Truncation, "timeout") || page.NextOffset != page.Count || time.Since(start) > 250*time.Millisecond {
		t.Fatalf("deadline page = %+v, elapsed=%s, err=%v", page, time.Since(start), err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	before := calls.Load()
	if _, err := service.Read(ctx, ReadRequest{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled read = %v", err)
	}
	if _, err := service.Discover(ctx, DiscoveryRequest{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled discovery = %v", err)
	}
	if calls.Load() != before {
		t.Fatalf("canceled invocations made %d requests", calls.Load()-before)
	}
	start = time.Now()
	service.policy.Timeout = 10 * time.Millisecond
	discovery.Store(true)
	if _, err := service.Discover(context.Background(), DiscoveryRequest{}); !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 250*time.Millisecond {
		t.Fatalf("discovery deadline = %v, elapsed=%s", err, time.Since(start))
	}
}

func TestFirstOversizedMessageReturnsPartial(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Write(make([]byte, maximumResponseBytes+1))
	}))
	defer server.Close()
	service, err := NewService(config.AgentGraylogSourceConfig{Address: server.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Read(context.Background(), ReadRequest{Limit: 1})
	if err != nil || result.Count != 0 || !result.Truncated || !strings.Contains(fmt.Sprint(result.Truncation), "byte_limit") {
		t.Fatalf("first oversized message = %+v, %v", result, err)
	}
}

func TestFirstSerializedMessageTooLargeDoesNotAdvance(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		fmt.Fprint(writer, `{"total_results":2,"messages":[{"message":{"message":"too large"}},{"message":{"message":"next"}}]}`)
	}))
	defer server.Close()
	service, err := NewService(config.AgentGraylogSourceConfig{Address: server.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	service.policy.MaxBytes = 1
	result, err := service.Read(context.Background(), ReadRequest{Limit: 2})
	if err != nil || result.Count != 0 || result.NextOffset != 0 || !slices.Contains(result.Truncation, "byte_limit") || slices.Contains(result.Truncation, "page_limit") {
		t.Fatalf("first serialized message = %+v, %v", result, err)
	}
}

func TestEmptyPageWithRemainingTotalDoesNotAdvance(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		fmt.Fprint(writer, `{"total_results":50,"messages":[]}`)
	}))
	defer server.Close()
	service, err := NewService(config.AgentGraylogSourceConfig{Address: server.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, offset := range []int{5, 0} {
		t.Run(strconv.Itoa(offset), func(t *testing.T) {
			page, err := service.Read(context.Background(), ReadRequest{Offset: offset, Limit: 5})
			if err != nil || page.Count != 0 || len(page.Records) != 0 || page.NextOffset != 0 || !page.Truncated || !slices.Contains(page.Truncation, "inconsistent_results") || slices.Contains(page.Truncation, "page_limit") {
				t.Fatalf("empty page at %d = %+v, %v", offset, page, err)
			}
		})
	}
}

func TestSerializedByteLimitPagination(t *testing.T) {
	const total = 20
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		offset, err := strconv.Atoi(request.URL.Query().Get("offset"))
		if err != nil {
			t.Errorf("offset: %v", err)
			return
		}
		messages := make([]any, 0, 12)
		for index := offset; index < min(total, offset+12); index++ {
			fields := map[string]any{"message": strconv.Itoa(index)}
			for field := 0; field < 39; field++ {
				fields[fmt.Sprintf("field_%02d", field)] = strings.Repeat("x", 400)
			}
			messages = append(messages, map[string]any{"message": fields})
		}
		json.NewEncoder(writer).Encode(map[string]any{"total_results": total, "messages": messages})
	}))
	defer server.Close()
	service, err := NewService(config.AgentGraylogSourceConfig{Address: server.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var seen []string
	for offset := 0; ; {
		page, err := service.Read(context.Background(), ReadRequest{Offset: offset, Limit: total})
		if err != nil || page.Count == 0 {
			t.Fatalf("page at %d = %+v, %v", offset, page, err)
		}
		for _, record := range page.Records {
			seen = append(seen, record.Message)
		}
		if page.NextOffset == 0 {
			if len(seen) != total {
				t.Fatalf("stopped early at %d: %v", offset, seen)
			}
			break
		}
		if !slices.Contains(page.Truncation, "byte_limit") || !slices.Contains(page.Truncation, "page_limit") || page.NextOffset != offset+page.Count {
			t.Fatalf("byte-limited page at %d = %+v", offset, page)
		}
		offset = page.NextOffset
	}
	for index, message := range seen {
		if message != strconv.Itoa(index) {
			t.Fatalf("row %d = %q, all rows = %v", index, message, seen)
		}
	}
}

func TestFieldProjectionScrubbingAndReasons(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Query().Get("fields") != "message,timestamp,secret,redacted,service" {
			t.Errorf("projection = %s", request.URL.RawQuery)
		}
		payload := map[string]any{"total_results": 1, "messages": []any{map[string]any{"message": map[string]any{
			"message": "ok", "secret": "first", "redacted": "second", "service": "api", "nested": map[string]any{"key": "value"},
		}}}}
		json.NewEncoder(writer).Encode(payload)
	}))
	defer server.Close()
	service, err := NewService(config.AgentGraylogSourceConfig{Address: server.URL, Fields: []string{"secret", "redacted", "service"}, APIToken: "secret"}, collisionScrubber{})
	if err != nil {
		t.Fatal(err)
	}
	read, err := service.Read(context.Background(), ReadRequest{})
	if err != nil || read.Count != 1 || read.Records[0].Fields["redacted"] != "second" || strings.Contains(fmt.Sprint(read), "secret") || !strings.Contains(fmt.Sprint(read.Truncation), "field_collision") || !strings.Contains(fmt.Sprint(read.Truncation), "unsupported_field") || strings.Contains(fmt.Sprint(read.Truncation), "field_limit") {
		t.Fatalf("read = %+v, %v", read, err)
	}
	fields, err := service.Discover(context.Background(), DiscoveryRequest{})
	if err != nil || !strings.Contains(fmt.Sprint(fields.Fields), "redacted") || strings.Contains(fmt.Sprint(fields.Fields), "secret") || strings.Contains(fmt.Sprint(fields.Fields), "nested") {
		t.Fatalf("discovery = %+v, %v", fields, err)
	}
	service.policy.MaxFields = 1
	read, err = service.Read(context.Background(), ReadRequest{})
	if err != nil || !strings.Contains(fmt.Sprint(read.Truncation), "field_limit") {
		t.Fatalf("field cap = %+v, %v", read, err)
	}
	fields, err = service.Discover(context.Background(), DiscoveryRequest{})
	if err != nil || !strings.Contains(fmt.Sprint(fields.Truncation), "field_limit") || !strings.EqualFold(fmt.Sprint(fields.Fields), "[message]") {
		t.Fatalf("discovery field cap = %+v, %v", fields, err)
	}
}

func TestDiscoveryReportsFieldLimitOnce(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		json.NewEncoder(writer).Encode(map[string]any{"total_results": 2, "messages": []any{
			map[string]any{"message": map[string]any{"a": "1", "b": "2", "c": "3"}},
			map[string]any{"message": map[string]any{"d": "4", "e": "5", "f": "6"}},
		}})
	}))
	defer server.Close()
	service, err := NewService(config.AgentGraylogSourceConfig{Address: server.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	service.policy.MaxFields = 2
	result, err := service.Discover(context.Background(), DiscoveryRequest{Limit: 10})
	if err != nil || result.Count != 2 {
		t.Fatalf("discovery = %+v, %v", result, err)
	}
	count := 0
	for _, reason := range result.Truncation {
		if reason == "field_limit" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("field_limit occurred %d times: %+v", count, result)
	}
}
