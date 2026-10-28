// Package tempo provides bounded read-only Tempo access for interactive tools.
package tempo

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/providerhttp"
)

var (
	ErrInvalidArgument  = errors.New("tempo: invalid argument")
	ErrInvalidConfig    = errors.New("tempo: invalid configuration")
	ErrResponseTooLarge = errors.New("tempo: response too large")
	ErrUnsupported      = errors.New("tempo: unsupported")
	ErrBackend          = errors.New("tempo: read failed")
)

type Policy struct {
	MaxBytes    int64
	Timeout     time.Duration
	MaxTraces   int
	MaxSpans    int
	MaxFields   int
	MaxLookback time.Duration
}

func ToolPolicy() Policy {
	return Policy{MaxBytes: 256 << 10, Timeout: 10 * time.Second, MaxTraces: 20, MaxSpans: 100, MaxFields: 100, MaxLookback: 6 * time.Hour}
}

const MaximumLimit = 50
const MaximumOffset = 100

var traceIDPattern = regexp.MustCompile(`^[a-fA-F0-9]{1,32}$`)
var spanIDPattern = regexp.MustCompile(`^[a-fA-F0-9]{16}$`)
var serviceNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:/-]{0,255}$`)

type Config struct {
	Address            string
	BearerToken        string
	Username           string
	Password           string
	AllowLoopback      bool
	AllowPrivate       bool
	InsecureSkipVerify bool
	RootCAs            *x509.CertPool
	ScopeService       string
	Policy             Policy
	Scrubber           core.Scrubber
	Now                func() time.Time
}

type Service struct {
	origin string
	config Config
	client *http.Client
}

func NewService(config Config) (*Service, error) {
	origin, err := providerhttp.NormalizeOrigin(config.Address)
	if err != nil {
		return nil, ErrInvalidConfig
	}
	parsed, _ := url.Parse(origin)
	policy := providerhttp.Policy{AllowLoopback: config.AllowLoopback, AllowPrivate: config.AllowPrivate, InsecureSkipVerify: config.InsecureSkipVerify, RootCAs: config.RootCAs}
	if !providerhttp.HostAllowed(parsed.Hostname(), policy) {
		return nil, ErrInvalidConfig
	}
	if parsed.Scheme == "http" && (config.BearerToken != "" || config.Username != "" || config.Password != "") {
		host := parsed.Hostname()
		if !config.AllowLoopback || !(host == "localhost" || strings.HasSuffix(host, ".localhost") || net.ParseIP(host).IsLoopback()) {
			return nil, ErrInvalidConfig
		}
		policy.RequireLoopback = true
	}
	if !validLiteral(config.ScopeService) || config.Policy.MaxBytes < 0 || config.Policy.Timeout < 0 || config.Policy.MaxTraces < 0 || config.Policy.MaxSpans < 0 || config.Policy.MaxFields < 0 || config.Policy.MaxLookback < 0 {
		return nil, ErrInvalidConfig
	}
	defaults := ToolPolicy()
	if config.Policy.MaxBytes == 0 {
		config.Policy.MaxBytes = defaults.MaxBytes
	}
	if config.Policy.Timeout == 0 {
		config.Policy.Timeout = defaults.Timeout
	}
	if config.Policy.MaxTraces == 0 {
		config.Policy.MaxTraces = defaults.MaxTraces
	}
	if config.Policy.MaxSpans == 0 {
		config.Policy.MaxSpans = defaults.MaxSpans
	}
	if config.Policy.MaxFields == 0 {
		config.Policy.MaxFields = defaults.MaxFields
	}
	if config.Policy.MaxLookback == 0 {
		config.Policy.MaxLookback = defaults.MaxLookback
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.BearerToken != "" && config.Username != "" {
		return nil, ErrInvalidConfig
	}
	return &Service{origin: origin, config: config, client: &http.Client{Transport: providerhttp.NewTransport(policy), Timeout: config.Policy.Timeout, CheckRedirect: providerhttp.RefuseRedirects}}, nil
}

func validLiteral(value string) bool {
	return len(value) <= 256 && strings.TrimSpace(value) == value && !strings.ContainsAny(value, "\x00\r\n")
}

func (service *Service) DiscoveryAvailable() bool {
	return service != nil && service.config.ScopeService == ""
}

type FieldRequest struct {
	Search string
	Offset int
	Limit  int
}

type FieldResult struct {
	Fields     []string `json:"fields"`
	Count      int      `json:"count"`
	Offset     int      `json:"offset"`
	Limit      int      `json:"limit"`
	NextOffset int      `json:"next_offset,omitempty"`
	Truncated  bool     `json:"truncated"`
	Truncation []string `json:"truncation,omitempty"`
}

func (service *Service) Discover(ctx context.Context, request FieldRequest) (FieldResult, error) {
	if !service.DiscoveryAvailable() {
		return FieldResult{}, ErrUnsupported
	}
	if request.Offset < 0 || request.Offset > MaximumOffset || request.Limit < 0 || request.Limit > MaximumLimit || len(request.Search) > 256 || !validLiteral(request.Search) {
		return FieldResult{}, ErrInvalidArgument
	}
	if request.Limit == 0 {
		request.Limit = 25
	}
	ctx, cancel := context.WithTimeout(ctx, service.config.Policy.Timeout)
	defer cancel()
	ctx = withByteBudget(ctx, service.config.Policy.MaxBytes)
	end := service.config.Now().UTC()
	values := map[string]bool{}
	for _, scope := range []string{"resource", "span"} {
		params := url.Values{"scope": {scope}, "start": {strconv.FormatInt(end.Add(-service.config.Policy.MaxLookback).Unix(), 10)}, "end": {strconv.FormatInt(end.Unix(), 10)}}
		body, err := service.get(ctx, "/api/v2/search/tags", params)
		if err != nil {
			return FieldResult{}, err
		}
		var response struct {
			Scopes []struct {
				Name string   `json:"name"`
				Tags []string `json:"tags"`
			} `json:"scopes"`
		}
		if json.Unmarshal(body, &response) != nil {
			return FieldResult{}, ErrBackend
		}
		for _, group := range response.Scopes {
			if group.Name != scope {
				continue
			}
			for _, tag := range group.Tags {
				if len(tag) <= 256 && strings.Contains(strings.ToLower(tag), strings.ToLower(request.Search)) {
					values[scope+"."+tag] = true
				}
			}
		}
	}
	fields := make([]string, 0, len(values))
	for field := range values {
		fields = append(fields, service.scrub(field, 256))
	}
	sort.Strings(fields)
	if len(fields) > service.config.Policy.MaxFields {
		fields = fields[:service.config.Policy.MaxFields]
	}
	result := FieldResult{Offset: request.Offset, Limit: request.Limit}
	if result.Offset > len(fields) {
		result.Offset = len(fields)
	}
	endIndex := min(result.Offset+result.Limit, len(fields))
	result.Fields = fields[result.Offset:endIndex]
	result.Count = len(result.Fields)
	for result.Count > 0 {
		encoded, _ := json.Marshal(result.Fields)
		if int64(len(encoded)) <= service.config.Policy.MaxBytes {
			break
		}
		result.Fields = result.Fields[:result.Count-1]
		result.Count--
		result.Truncated = true
		result.Truncation = appendUnique(result.Truncation, "byte_limit")
	}
	endIndex = result.Offset + result.Count
	if endIndex < len(fields) {
		result.Truncated = true
		result.NextOffset = endIndex
		result.Truncation = append(result.Truncation, "page_limit")
	}
	if len(values) > service.config.Policy.MaxFields {
		result.Truncated = true
		result.Truncation = append(result.Truncation, "field_limit")
	}
	return result, nil
}

type ReadRequest struct {
	Service   string
	TraceID   string
	Operation string
	ErrorOnly *bool
	Lookback  time.Duration
	Offset    int
	Limit     int
}

type Span struct {
	TraceID      string  `json:"trace_id"`
	SpanID       string  `json:"span_id"`
	ParentSpanID string  `json:"parent_span_id,omitempty"`
	Service      string  `json:"service"`
	Operation    string  `json:"operation"`
	DurationMs   float64 `json:"duration_ms"`
	Error        bool    `json:"error"`
	rawOperation string
}

type ReadResult struct {
	Spans      []Span   `json:"spans"`
	Count      int      `json:"count"`
	Traces     int      `json:"traces"`
	Offset     int      `json:"offset"`
	Limit      int      `json:"limit"`
	NextOffset int      `json:"next_offset,omitempty"`
	Truncated  bool     `json:"truncated"`
	Truncation []string `json:"truncation,omitempty"`
}

type CandidateRequest struct {
	Service  string
	Lookback time.Duration
}

type CandidateResult struct {
	TraceIDs  []string `json:"trace_ids"`
	Truncated bool     `json:"truncated"`
}

func (service *Service) CandidateTraceIDs(ctx context.Context, request CandidateRequest) (CandidateResult, error) {
	if service == nil || service.config.ScopeService == "" || request.Service == "" || request.Service != service.config.ScopeService || !validLiteral(request.Service) || service.scrub(request.Service, 256) != request.Service || request.Lookback < 0 {
		return CandidateResult{}, ErrInvalidArgument
	}
	if request.Lookback == 0 {
		request.Lookback = time.Hour
	}
	if request.Lookback > service.config.Policy.MaxLookback {
		request.Lookback = service.config.Policy.MaxLookback
	}
	ctx, cancel := context.WithTimeout(ctx, service.config.Policy.Timeout)
	defer cancel()
	ctx = withByteBudget(ctx, service.config.Policy.MaxBytes)
	end := service.config.Now().UTC()
	limit := min(service.config.Policy.MaxTraces, MaximumLimit)
	params := url.Values{"q": {`{ resource.service.name = ` + strconv.Quote(request.Service) + ` }`}, "start": {strconv.FormatInt(end.Add(-request.Lookback).Unix(), 10)}, "end": {strconv.FormatInt(end.Unix(), 10)}, "limit": {strconv.Itoa(limit + 1)}}
	body, err := service.get(ctx, "/api/search", params)
	if err != nil {
		return CandidateResult{}, err
	}
	var found *struct {
		Traces []struct {
			TraceID string `json:"traceID"`
		} `json:"traces"`
	}
	if json.Unmarshal(body, &found) != nil || found == nil || len(found.Traces) > limit+1 {
		return CandidateResult{}, ErrBackend
	}
	result := CandidateResult{Truncated: len(found.Traces) > limit}
	seen := map[string]bool{}
	for _, trace := range found.Traces {
		canonicalID, valid := wireID(trace.TraceID, 16)
		if !valid || canonicalID == strings.Repeat("0", 32) {
			return CandidateResult{}, ErrBackend
		}
		if !seen[canonicalID] && len(result.TraceIDs) < limit {
			seen[canonicalID] = true
			result.TraceIDs = append(result.TraceIDs, canonicalID)
		} else if !seen[canonicalID] {
			result.Truncated = true
		}
	}
	sort.Strings(result.TraceIDs)
	encoded, _ := json.Marshal(result)
	if int64(len(encoded)) > service.config.Policy.MaxBytes {
		return CandidateResult{}, ErrResponseTooLarge
	}
	return result, nil
}

func (service *Service) Read(ctx context.Context, request ReadRequest) (ReadResult, error) {
	if request.Service == "" || !validLiteral(request.Service) || !validLiteral(request.Operation) || (request.TraceID != "" && !traceIDPattern.MatchString(request.TraceID)) || request.Offset < 0 || request.Offset > MaximumOffset || (request.TraceID == "" && request.Offset != 0) || request.Limit < 0 || request.Limit > MaximumLimit || request.Lookback < 0 {
		return ReadResult{}, ErrInvalidArgument
	}
	if request.Limit == 0 {
		request.Limit = 25
	}
	if request.Lookback == 0 {
		request.Lookback = time.Hour
	}
	if request.Lookback > service.config.Policy.MaxLookback {
		request.Lookback = service.config.Policy.MaxLookback
	}
	if service.config.ScopeService != "" && request.Service != service.config.ScopeService {
		return ReadResult{}, ErrInvalidArgument
	}
	ctx, cancel := context.WithTimeout(ctx, service.config.Policy.Timeout)
	defer cancel()
	ctx = withByteBudget(ctx, service.config.Policy.MaxBytes)
	end := service.config.Now().UTC()
	selector := `{ resource.service.name = ` + strconv.Quote(request.Service)
	if request.Operation != "" {
		selector += ` && name = ` + strconv.Quote(request.Operation)
	}
	if request.ErrorOnly != nil && *request.ErrorOnly {
		selector += ` && status = error`
	}
	selector += ` }`
	var found struct {
		Traces []struct {
			TraceID string `json:"traceID"`
			Start   string `json:"startTimeUnixNano"`
		} `json:"traces"`
	}
	if request.TraceID != "" {
		found.Traces = append(found.Traces, struct {
			TraceID string `json:"traceID"`
			Start   string `json:"startTimeUnixNano"`
		}{TraceID: request.TraceID})
	} else {
		params := url.Values{"q": {selector}, "start": {strconv.FormatInt(end.Add(-request.Lookback).Unix(), 10)}, "end": {strconv.FormatInt(end.Unix(), 10)}, "limit": {strconv.Itoa(service.config.Policy.MaxTraces + 1)}}
		body, err := service.get(ctx, "/api/search", params)
		if err != nil {
			return ReadResult{}, err
		}
		if json.Unmarshal(body, &found) != nil {
			return ReadResult{}, ErrBackend
		}
	}
	sort.Slice(found.Traces, func(i, j int) bool {
		if found.Traces[i].Start == found.Traces[j].Start {
			return found.Traces[i].TraceID < found.Traces[j].TraceID
		}
		return found.Traces[i].Start > found.Traces[j].Start
	})
	result := ReadResult{Offset: request.Offset, Limit: request.Limit}
	if len(found.Traces) > service.config.Policy.MaxTraces {
		result.Truncated = true
		result.Truncation = append(result.Truncation, "trace_limit")
		found.Traces = found.Traces[:service.config.Policy.MaxTraces]
	}
	seen := map[string]bool{}
	for _, trace := range found.Traces {
		if !traceIDPattern.MatchString(trace.TraceID) {
			return ReadResult{}, ErrBackend
		}
		if seen[trace.TraceID] {
			continue
		}
		seen[trace.TraceID] = true
		spans, capped, readErr := service.trace(ctx, trace.TraceID, request.Service, end.Add(-request.Lookback), end, false)
		if readErr != nil {
			if errors.Is(readErr, ErrResponseTooLarge) && len(result.Spans) > 0 {
				result.Truncated = true
				result.Truncation = appendUnique(result.Truncation, "byte_limit")
				break
			}
			return ReadResult{}, readErr
		}
		result.Traces++
		if capped {
			result.Truncated = true
			result.Truncation = appendUnique(result.Truncation, "span_limit")
		}
		for _, span := range spans {
			if (request.Operation != "" && span.rawOperation != request.Operation) || (request.ErrorOnly != nil && span.Error != *request.ErrorOnly) {
				continue
			}
			if result.Offset > 0 {
				result.Offset--
				continue
			}
			if len(result.Spans) >= min(request.Limit, service.config.Policy.MaxSpans) {
				result.Truncated = true
				if request.Limit <= service.config.Policy.MaxSpans {
					result.Truncation = appendUnique(result.Truncation, "page_limit")
				} else {
					result.Truncation = appendUnique(result.Truncation, "span_limit")
				}
				break
			}
			candidate := append(result.Spans, span)
			encoded, _ := json.Marshal(candidate)
			if int64(len(encoded)) > service.config.Policy.MaxBytes {
				result.Truncated = true
				result.Truncation = appendUnique(result.Truncation, "byte_limit")
				break
			}
			result.Spans = candidate
		}
		if capped || slices.Contains(result.Truncation, "page_limit") || slices.Contains(result.Truncation, "span_limit") || slices.Contains(result.Truncation, "byte_limit") {
			break
		}
	}
	result.Offset = request.Offset
	result.Count = len(result.Spans)
	if request.TraceID != "" && slices.Contains(result.Truncation, "page_limit") && !slices.Contains(result.Truncation, "byte_limit") && result.Count > 0 && request.Offset+result.Count < service.config.Policy.MaxSpans {
		result.NextOffset = request.Offset + result.Count
	}
	return result, nil
}

// TraceTree is reserved for a complete trace-local projection.
type TraceTree struct {
	TraceID string `json:"trace_id"`
	Spans   []Span `json:"spans"`
}

func (service *Service) ProjectTrace(ctx context.Context, traceID string) (TraceTree, error) {
	if !traceIDPattern.MatchString(traceID) {
		return TraceTree{}, ErrInvalidArgument
	}
	if service.config.ScopeService != "" && service.scrub(service.config.ScopeService, 256) != service.config.ScopeService {
		return TraceTree{}, ErrInvalidArgument
	}
	ctx, cancel := context.WithTimeout(ctx, service.config.Policy.Timeout)
	defer cancel()
	ctx = withByteBudget(ctx, service.config.Policy.MaxBytes)
	end := service.config.Now().UTC()
	spans, capped, err := service.trace(ctx, traceID, "", end.Add(-service.config.Policy.MaxLookback), end, true)
	if err != nil {
		return TraceTree{}, err
	}
	if capped || len(spans) == 0 {
		return TraceTree{}, ErrUnsupported
	}
	canonicalID, _ := wireID(traceID, 16)
	parents := make(map[string]string, len(spans))
	member := service.config.ScopeService == ""
	roots := 0
	for index := range spans {
		spans[index].TraceID = canonicalID
		parents[spans[index].SpanID] = spans[index].ParentSpanID
		if spans[index].Service == service.config.ScopeService {
			member = true
		}
		if spans[index].ParentSpanID == "" {
			roots++
		}
	}
	if !member || roots != 1 {
		return TraceTree{}, ErrUnsupported
	}
	for _, span := range spans {
		visited := map[string]bool{}
		for parent := span.ParentSpanID; parent != ""; parent = parents[parent] {
			if parent == span.SpanID || visited[parent] {
				return TraceTree{}, ErrUnsupported
			}
			visited[parent] = true
		}
	}
	tree := TraceTree{TraceID: canonicalID, Spans: spans}
	encoded, _ := json.Marshal(tree)
	if int64(len(encoded)) > service.config.Policy.MaxBytes {
		return TraceTree{}, ErrResponseTooLarge
	}
	return tree, nil
}

func (service *Service) trace(ctx context.Context, traceID, serviceFilter string, start, end time.Time, complete bool) ([]Span, bool, error) {
	params := url.Values{"start": {strconv.FormatInt(start.Unix(), 10)}, "end": {strconv.FormatInt(end.Unix(), 10)}}
	body, err := service.get(ctx, "/api/traces/"+traceID, params)
	if err != nil {
		return nil, false, err
	}
	var trace struct {
		Batches []struct {
			Resource struct {
				Attributes []attribute `json:"attributes"`
			} `json:"resource"`
			ScopeSpans []struct {
				Attributes []json.RawMessage `json:"attributes"`
				Spans      []struct {
					TraceID      string            `json:"traceId"`
					SpanID       string            `json:"spanId"`
					ParentSpanID string            `json:"parentSpanId"`
					Attributes   []json.RawMessage `json:"attributes"`
					Name         string            `json:"name"`
					Start        string            `json:"startTimeUnixNano"`
					End          string            `json:"endTimeUnixNano"`
					Status       struct {
						Code json.RawMessage `json:"code"`
					} `json:"status"`
				} `json:"spans"`
			} `json:"scopeSpans"`
		} `json:"batches"`
	}
	if json.Unmarshal(body, &trace) != nil || len(trace.Batches) == 0 {
		return nil, false, ErrUnsupported
	}
	spans := []Span{}
	ids := map[string]bool{}
	capped := false
	for _, batch := range trace.Batches {
		resource := ""
		serviceNames := 0
		if len(batch.Resource.Attributes) > service.config.Policy.MaxFields || (complete && len(batch.ScopeSpans) == 0) {
			return nil, false, ErrUnsupported
		}
		for _, attr := range batch.Resource.Attributes {
			if attr.Key == "service.name" {
				resource = attr.Value.StringValue
				serviceNames++
			}
		}
		if complete && (serviceNames != 1 || !serviceNamePattern.MatchString(resource) || service.scrub(resource, 256) != resource) {
			return nil, false, ErrUnsupported
		}
		if serviceFilter != "" && resource != serviceFilter {
			continue
		}
		for _, scope := range batch.ScopeSpans {
			if complete && (len(scope.Spans) == 0 || len(scope.Attributes) > service.config.Policy.MaxFields) {
				return nil, false, ErrUnsupported
			}
			for _, span := range scope.Spans {
				if len(spans) >= service.config.Policy.MaxSpans {
					capped = true
					break
				}
				if complete && (len(span.Attributes) > service.config.Policy.MaxFields || span.TraceID == "") {
					return nil, false, ErrUnsupported
				}
				spanID, validSpan := wireID(span.SpanID, 8)
				parentID, validParent := "", true
				if span.ParentSpanID != "" {
					parentID, validParent = wireID(span.ParentSpanID, 8)
				}
				actualTrace, validTrace := "", true
				if span.TraceID != "" {
					actualTrace, validTrace = wireID(span.TraceID, 16)
				}
				isError, validStatus := wireStatus(span.Status.Code)
				if !validSpan || !validParent || !validTrace || !validStatus || (span.TraceID != "" && strings.TrimLeft(actualTrace, "0") != strings.TrimLeft(strings.ToLower(traceID), "0")) || ids[spanID] || (complete && (spanID == "0000000000000000" || parentID == "0000000000000000" || actualTrace == strings.Repeat("0", 32))) {
					return nil, false, ErrUnsupported
				}
				ids[spanID] = true
				start, startErr := strconv.ParseInt(span.Start, 10, 64)
				end, endErr := strconv.ParseInt(span.End, 10, 64)
				if startErr != nil || endErr != nil || end < start {
					return nil, false, ErrUnsupported
				}
				spans = append(spans, Span{TraceID: traceID, SpanID: spanID, ParentSpanID: parentID, Service: service.scrub(resource, 256), Operation: service.scrub(span.Name, 256), DurationMs: float64(end-start) / 1e6, Error: isError, rawOperation: span.Name})
			}
		}
	}
	if serviceFilter == "" {
		for _, span := range spans {
			if span.ParentSpanID != "" && !ids[span.ParentSpanID] {
				return nil, false, ErrUnsupported
			}
		}
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].SpanID < spans[j].SpanID })
	encoded, _ := json.Marshal(spans)
	if int64(len(encoded)) > service.config.Policy.MaxBytes {
		return nil, false, ErrResponseTooLarge
	}
	return spans, capped, nil
}

func wireID(value string, size int) (string, bool) {
	if (size == 8 && spanIDPattern.MatchString(value)) || (size == 16 && traceIDPattern.MatchString(value)) {
		if size == 16 {
			return strings.Repeat("0", 32-len(value)) + strings.ToLower(value), true
		}
		return strings.ToLower(value), true
	}
	decoded, err := base64.StdEncoding.DecodeString(value)
	if err != nil || len(decoded) != size {
		return "", false
	}
	return hex.EncodeToString(decoded), true
}

func wireStatus(raw json.RawMessage) (bool, bool) {
	if len(raw) == 0 {
		return false, true
	}
	var code int
	if json.Unmarshal(raw, &code) == nil {
		return code == 2, code >= 0 && code <= 2
	}
	var name string
	if json.Unmarshal(raw, &name) != nil {
		return false, false
	}
	switch name {
	case "STATUS_CODE_UNSET", "STATUS_CODE_OK":
		return false, true
	case "STATUS_CODE_ERROR":
		return true, true
	default:
		return false, false
	}
}

type attribute struct {
	Key   string `json:"key"`
	Value struct {
		StringValue string `json:"stringValue"`
	} `json:"value"`
}

func (service *Service) scrub(value string, limit int) string {
	if service.config.Scrubber != nil {
		value = service.config.Scrubber.Scrub(value)
	}
	for _, secret := range []string{service.config.BearerToken, service.config.Username, service.config.Password} {
		if secret != "" {
			value = strings.ReplaceAll(value, secret, "[redacted]")
		}
	}
	if len(value) > limit {
		value = value[:limit]
	}
	return value
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func (service *Service) get(ctx context.Context, path string, params url.Values) ([]byte, error) {
	maximum := service.config.Policy.MaxBytes
	if budget, ok := ctx.Value(byteBudgetKey{}).(*byteBudget); ok {
		maximum = budget.remaining
	}
	if maximum <= 0 {
		return nil, ErrResponseTooLarge
	}
	endpoint := service.origin + path
	if len(params) > 0 {
		endpoint += "?" + params.Encode()
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, ErrInvalidArgument
	}
	request.Header.Set("Accept", "application/json")
	if service.config.BearerToken != "" {
		request.Header.Set("Authorization", "Bearer "+service.config.BearerToken)
	} else if service.config.Username != "" {
		request.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(service.config.Username+":"+service.config.Password)))
	}
	response, err := service.client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, context.DeadlineExceeded
		}
		return nil, ErrBackend
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, ErrBackend
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maximum+1))
	if err != nil {
		return nil, ErrBackend
	}
	if int64(len(body)) > maximum {
		return nil, ErrResponseTooLarge
	}
	if budget, ok := ctx.Value(byteBudgetKey{}).(*byteBudget); ok {
		budget.remaining -= int64(len(body))
	}
	return body, nil
}

type byteBudgetKey struct{}
type byteBudget struct{ remaining int64 }

func withByteBudget(ctx context.Context, maximum int64) context.Context {
	return context.WithValue(ctx, byteBudgetKey{}, &byteBudget{remaining: maximum})
}
