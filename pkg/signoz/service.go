// Package signoz provides source-scoped, bounded read operations for SigNoz.
package signoz

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/VersusControl/versus-incident/pkg/core"
)

const (
	QueryRangePath  = "/api/v5/query_range"
	FieldKeysPath   = "/api/v1/fields/keys"
	FieldValuesPath = "/api/v1/fields/values"
	MetricsPath     = "/api/v2/metrics"

	DefaultLimit  = 50
	MaximumLimit  = 100
	MaximumOffset = 10_000
)

var (
	ErrInvalidConfig    = errors.New("invalid SigNoz configuration")
	ErrInvalidArgument  = errors.New("invalid SigNoz arguments")
	ErrResponseTooLarge = errors.New("SigNoz response exceeded its safe bound")
)

// Signal is a SigNoz query signal selected by trusted source configuration.
type Signal string

const (
	SignalLogs    Signal = "logs"
	SignalTraces  Signal = "traces"
	SignalMetrics Signal = "metrics"
)

// Config contains one source's trusted connection and scope settings.
type Config struct {
	Address            string
	APIKey             string
	InsecureSkipVerify bool
	AllowLoopback      bool
	AllowPrivate       bool
	RootCAs            *x509.CertPool
	ScopeFilter        string
}

// Policy contains independent resource budgets for one use of the service.
type Policy struct {
	Timeout                    time.Duration
	MaximumBytes               int64
	MaximumWindow              time.Duration
	MaximumRows                int
	MaximumSeries              int
	MaximumFieldKeys           int
	MaximumDatapointsPerSeries int
	MaximumDatapoints          int
}

// ToolPolicy is the bounded interactive-read policy used by model tools.
func ToolPolicy() Policy {
	return Policy{Timeout: 15 * time.Second, MaximumBytes: 2 << 20, MaximumWindow: 6 * time.Hour, MaximumRows: MaximumLimit, MaximumSeries: 50, MaximumFieldKeys: MaximumLimit, MaximumDatapointsPerSeries: 1000, MaximumDatapoints: 5000}
}

// IngestPolicy preserves the larger response and timeout budgets used by source ingestion.
func IngestPolicy() Policy {
	return Policy{Timeout: 30 * time.Second, MaximumBytes: 16 << 20, MaximumWindow: 24 * time.Hour, MaximumRows: 1000, MaximumSeries: 1000, MaximumFieldKeys: 1000, MaximumDatapointsPerSeries: 10_000, MaximumDatapoints: 100_000}
}

// SearchRequest is a bounded raw logs or spans read. Filter is always composed
// with the configured source scope and cannot replace it.
type SearchRequest struct {
	Start      time.Time
	End        time.Time
	Service    string
	SearchText string
	Severity   string
	Operation  string
	ErrorOnly  *bool
	Offset     int
	Limit      int
}

// MetricRequest is a bounded time-series read for one named metric.
type MetricRequest struct {
	Start            time.Time
	End              time.Time
	Service          string
	MetricName       string
	Temporality      string
	TimeAggregation  string
	SpaceAggregation string
	Step             time.Duration
	Limit            int
}

// FieldRequest scopes field discovery to a trusted signal and optional metric.
type FieldRequest struct {
	MetricName string
	SearchText string
	Context    string
	DataType   string
	Name       string
}

// Result is a model-neutral bounded response from SigNoz.
type Result struct {
	Data       map[string]any `json:"data"`
	Count      int            `json:"count"`
	Truncated  bool           `json:"truncated"`
	Truncation []string       `json:"truncation,omitempty"`
	Offset     int            `json:"offset,omitempty"`
	Limit      int            `json:"limit"`
}

// Service owns transport, validation, source scope, budgets, and redaction for one source.
type Service struct {
	scope    string
	apiKey   string
	policy   Policy
	client   *Client
	scrubber core.Scrubber
}

// NewService validates one source and constructs a bounded read-only service.
func NewService(config Config, policy Policy) (*Service, error) {
	policy = normalizePolicy(policy)
	client, err := NewClient(config, policy)
	if err != nil {
		return nil, err
	}
	return &Service{scope: strings.TrimSpace(config.ScopeFilter), apiKey: config.APIKey, policy: policy, client: client}, nil
}

// SetScrubber applies the shared redaction policy before data leaves the service.
func (service *Service) SetScrubber(scrubber core.Scrubber) {
	if service != nil {
		service.scrubber = scrubber
	}
}

// DiscoveryAvailable reports whether source-wide metadata discovery is safe.
// SigNoz discovery endpoints do not accept the configured query scope.
func (service *Service) DiscoveryAvailable() bool {
	return service != nil && service.scope == ""
}

// SearchLogs reads bounded log rows from the configured source.
func (service *Service) SearchLogs(ctx context.Context, request SearchRequest) (Result, error) {
	return service.search(ctx, SignalLogs, request)
}

// SearchTraces reads bounded span rows from the configured source.
func (service *Service) SearchTraces(ctx context.Context, request SearchRequest) (Result, error) {
	return service.search(ctx, SignalTraces, request)
}

// QueryMetrics reads bounded time series for one metric from the configured source.
func (service *Service) QueryMetrics(ctx context.Context, request MetricRequest) (Result, error) {
	start, end, err := service.window(request.Start, request.End)
	if err != nil || strings.TrimSpace(request.MetricName) == "" || request.Step < 0 || !safeRequestLiterals(request.Service, request.MetricName, request.Temporality, request.TimeAggregation, request.SpaceAggregation) {
		return Result{}, fmt.Errorf("%w: invalid metric query", ErrInvalidArgument)
	}
	limit := boundedLimit(request.Limit, service.policy.MaximumSeries)
	filter := combineFilters(service.scope, equalityFilter("service.name", request.Service))
	aggregation := map[string]any{"metricName": request.MetricName}
	for key, value := range map[string]string{"temporality": request.Temporality, "timeAggregation": request.TimeAggregation, "spaceAggregation": request.SpaceAggregation} {
		if value = strings.TrimSpace(value); value != "" {
			aggregation[key] = value
		}
	}
	spec := map[string]any{"name": "A", "signal": string(SignalMetrics), "limit": limit, "aggregations": []any{aggregation}, "order": []any{orderTerm("__result", "desc")}}
	if filter != "" {
		spec["filter"] = map[string]any{"expression": filter}
	}
	if seconds := int64(request.Step / time.Second); seconds > 0 {
		spec["stepInterval"] = seconds
	}
	payload := queryPayload(start, end, "time_series", spec)
	return service.execute(ctx, QueryRangePath, nil, payload, responseSeries, service.policy.MaximumSeries, 0, limit)
}

// FieldKeys discovers bounded fields for one trusted signal.
func (service *Service) FieldKeys(ctx context.Context, signal Signal, request FieldRequest) (Result, error) {
	if !validSignal(signal) || !validFieldRequest(request, false) {
		return Result{}, fmt.Errorf("%w: invalid signal", ErrInvalidArgument)
	}
	query := url.Values{"signal": {string(signal)}}
	setOptional(query, "metricName", request.MetricName)
	setOptional(query, "searchText", request.SearchText)
	setOptional(query, "fieldContext", request.Context)
	setOptional(query, "fieldDataType", request.DataType)
	query.Set("limit", strconv.Itoa(service.policy.MaximumFieldKeys))
	return service.execute(ctx, FieldKeysPath, query, nil, responseDiscovery, service.policy.MaximumFieldKeys, 0, service.policy.MaximumFieldKeys)
}

// FieldValues discovers bounded observed values for one trusted signal field.
func (service *Service) FieldValues(ctx context.Context, signal Signal, request FieldRequest) (Result, error) {
	if !validSignal(signal) || strings.TrimSpace(request.Name) == "" || !validFieldRequest(request, true) {
		return Result{}, fmt.Errorf("%w: signal and field name are required", ErrInvalidArgument)
	}
	query := url.Values{"signal": {string(signal)}, "name": {request.Name}}
	setOptional(query, "metricName", request.MetricName)
	setOptional(query, "searchText", request.SearchText)
	setOptional(query, "fieldContext", request.Context)
	query.Set("limit", strconv.Itoa(service.policy.MaximumFieldKeys))
	return service.execute(ctx, FieldValuesPath, query, nil, responseDiscovery, service.policy.MaximumFieldKeys, 0, service.policy.MaximumFieldKeys)
}

// ListMetrics discovers bounded metric metadata from the configured source.
func (service *Service) ListMetrics(ctx context.Context, search string, limit int) (Result, error) {
	if !safeFilterLiteral(search) {
		return Result{}, fmt.Errorf("%w: invalid metric search", ErrInvalidArgument)
	}
	limit = boundedLimit(limit, service.policy.MaximumSeries)
	query := url.Values{"limit": {strconv.Itoa(limit)}}
	setOptional(query, "searchText", search)
	return service.execute(ctx, MetricsPath, query, nil, responseMetricList, service.policy.MaximumSeries, 0, limit)
}

func (service *Service) search(ctx context.Context, signal Signal, request SearchRequest) (Result, error) {
	start, end, err := service.window(request.Start, request.End)
	if err != nil || request.Offset < 0 || request.Offset > MaximumOffset || !safeFilterLiteral(request.Service) || !safeFilterLiteral(request.SearchText) || !safeFilterLiteral(request.Severity) || !safeFilterLiteral(request.Operation) {
		return Result{}, fmt.Errorf("%w: invalid search bounds", ErrInvalidArgument)
	}
	limit := boundedLimit(request.Limit, service.policy.MaximumRows)
	filters := []string{service.scope, equalityFilter("service.name", request.Service)}
	if signal == SignalLogs {
		filters = append(filters, equalityFilter("severity_text", request.Severity), containsFilter("body", request.SearchText))
	} else {
		filters = append(filters, equalityFilter("name", request.Operation))
		if request.ErrorOnly != nil {
			filters = append(filters, fmt.Sprintf("has_error = %t", *request.ErrorOnly))
		}
	}
	order := []any{orderTerm("timestamp", "desc")}
	if signal == SignalLogs {
		order = append(order, orderTerm("id", "desc"))
	}
	spec := map[string]any{"name": "A", "signal": string(signal), "offset": request.Offset, "limit": limit, "order": order}
	if signal == SignalTraces {
		spec["selectFields"] = traceSelectFields()
	}
	if filter := combineFilters(filters...); filter != "" {
		spec["filter"] = map[string]any{"expression": filter}
	}
	payload := queryPayload(start, end, "raw", spec)
	return service.execute(ctx, QueryRangePath, nil, payload, responseRows, service.policy.MaximumRows, request.Offset, limit)
}

func (service *Service) window(start, end time.Time) (time.Time, time.Time, error) {
	if end.IsZero() {
		end = time.Now().UTC()
	}
	if start.IsZero() {
		start = end.Add(-time.Hour)
	}
	start, end = start.UTC(), end.UTC()
	if !start.Before(end) || end.Sub(start) > service.policy.MaximumWindow || end.After(time.Now().UTC().Add(time.Minute)) {
		return time.Time{}, time.Time{}, ErrInvalidArgument
	}
	return start, end, nil
}

type responseShape int

const (
	responseRows responseShape = iota
	responseSeries
	responseMetricList
	responseDiscovery
)

func (service *Service) execute(ctx context.Context, path string, query url.Values, payload any, shape responseShape, itemLimit, offset, limit int) (Result, error) {
	method := http.MethodGet
	if payload != nil {
		method = http.MethodPost
	}
	data, err := service.client.Do(ctx, method, path, query, payload)
	if err != nil {
		return Result{}, err
	}
	var decoded any
	if err := json.Unmarshal(data, &decoded); err != nil {
		return Result{}, fmt.Errorf("decode SigNoz response: %w", err)
	}
	bounded, count, truncation := service.boundValue(decoded, shape, itemLimit)
	object, ok := bounded.(map[string]any)
	if !ok {
		object = map[string]any{"items": bounded}
	}
	return Result{Data: object, Count: count, Truncated: len(truncation) > 0, Truncation: truncation, Offset: offset, Limit: limit}, nil
}

func (service *Service) boundValue(value any, shape responseShape, limit int) (any, int, []string) {
	count, datapoints := 0, 0
	reasons := map[string]bool{}
	var walk func(any) any
	walk = func(current any) any {
		switch typed := current.(type) {
		case map[string]any:
			out := make(map[string]any, len(typed))
			keys := make([]string, 0, len(typed))
			for key := range typed {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				child := typed[key]
				if shape == responseDiscovery && (key == "keys" || key == "values") {
					bounded, discovered, truncated := service.boundDiscovery(child, limit, walk)
					count += discovered
					if truncated {
						reasons["discovery_field_"+key] = true
						out["complete"] = false
					}
					out[service.scrub(key, 256)] = bounded
					continue
				}
				if items, ok := child.([]any); ok && boundedItemKey(shape, key) {
					count += len(items)
					if limit > 0 && len(items) > limit {
						items = items[:limit]
						reasons[itemTruncationReason(shape)] = true
					}
					out[service.scrub(key, 256)] = walkItems(items, walk)
					continue
				}
				if shape == responseSeries && key == "values" {
					if values, ok := child.([]any); ok {
						available := max(0, service.policy.MaximumDatapoints-datapoints)
						keep := min(len(values), service.policy.MaximumDatapointsPerSeries, available)
						if keep < len(values) {
							values = values[len(values)-keep:]
							reasons["datapoints"] = true
						}
						datapoints += keep
						out[service.scrub(key, 256)] = walkItems(values, walk)
						continue
					}
				}
				out[service.scrub(key, 256)] = walk(child)
			}
			return out
		case []any:
			out := make([]any, len(typed))
			for index, child := range typed {
				out[index] = walk(child)
			}
			return out
		case string:
			return service.scrub(typed, 4096)
		default:
			return current
		}
	}
	bounded := walk(value)
	truncation := make([]string, 0, len(reasons))
	for reason := range reasons {
		truncation = append(truncation, reason)
	}
	sort.Strings(truncation)
	return bounded, count, truncation
}

func boundedItemKey(shape responseShape, key string) bool {
	switch shape {
	case responseRows:
		return key == "rows"
	case responseSeries:
		return key == "series"
	case responseMetricList:
		return key == "metrics" || key == "items"
	default:
		return false
	}
}

func itemTruncationReason(shape responseShape) string {
	switch shape {
	case responseRows:
		return "rows"
	case responseSeries:
		return "series"
	case responseMetricList:
		return "metrics"
	default:
		return "items"
	}
}

func (service *Service) boundDiscovery(value any, limit int, walk func(any) any) (any, int, bool) {
	switch typed := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		truncated := limit > 0 && len(keys) > limit
		if truncated {
			keys = keys[:limit]
		}
		out := make(map[string]any, len(keys))
		for _, key := range keys {
			out[service.scrub(key, 256)] = walk(typed[key])
		}
		return out, len(typed), truncated
	case []any:
		count := len(typed)
		truncated := limit > 0 && count > limit
		if truncated {
			typed = typed[:limit]
		}
		return walkItems(typed, walk), count, truncated
	default:
		return walk(value), 0, false
	}
}

func walkItems(items []any, walk func(any) any) []any {
	out := make([]any, len(items))
	for index, item := range items {
		out[index] = walk(item)
	}
	return out
}

func (service *Service) scrub(value string, maximum int) string {
	value = ScrubExactSecret(value, service.apiKey)
	if service.scrubber != nil {
		value = service.scrubber.Scrub(value)
	}
	if len(value) <= maximum {
		return value
	}
	value = value[:maximum]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

func normalizePolicy(policy Policy) Policy {
	defaults := ToolPolicy()
	if policy.Timeout <= 0 {
		policy.Timeout = defaults.Timeout
	}
	if policy.MaximumBytes <= 0 {
		policy.MaximumBytes = defaults.MaximumBytes
	}
	if policy.MaximumWindow <= 0 {
		policy.MaximumWindow = defaults.MaximumWindow
	}
	if policy.MaximumRows <= 0 {
		policy.MaximumRows = defaults.MaximumRows
	}
	if policy.MaximumSeries <= 0 {
		policy.MaximumSeries = defaults.MaximumSeries
	}
	if policy.MaximumFieldKeys <= 0 {
		policy.MaximumFieldKeys = defaults.MaximumFieldKeys
	}
	if policy.MaximumDatapointsPerSeries <= 0 {
		policy.MaximumDatapointsPerSeries = defaults.MaximumDatapointsPerSeries
	}
	if policy.MaximumDatapoints <= 0 {
		policy.MaximumDatapoints = defaults.MaximumDatapoints
	}
	return policy
}

func boundedLimit(limit, maximum int) int {
	if maximum <= 0 || maximum > MaximumLimit {
		maximum = MaximumLimit
	}
	if limit <= 0 {
		return min(DefaultLimit, maximum)
	}
	return min(limit, maximum)
}

func queryPayload(start, end time.Time, requestType string, spec map[string]any) map[string]any {
	return map[string]any{"schemaVersion": "v1", "start": start.UnixMilli(), "end": end.UnixMilli(), "requestType": requestType, "compositeQuery": map[string]any{"queries": []any{map[string]any{"type": "builder_query", "spec": spec}}}}
}

func orderTerm(name, direction string) map[string]any {
	return map[string]any{"key": map[string]any{"name": name}, "direction": direction}
}
func traceSelectFields() []any {
	return []any{
		map[string]any{"name": "timestamp"},
		map[string]any{"name": "trace_id"},
		map[string]any{"name": "span_id"},
		map[string]any{"name": "parent_span_id"},
		map[string]any{"name": "name"},
		map[string]any{"name": "duration_nano"},
		map[string]any{"name": "has_error"},
		map[string]any{"name": "service.name", "fieldContext": "resource", "fieldDataType": "string"},
	}
}
func validSignal(signal Signal) bool {
	return signal == SignalLogs || signal == SignalTraces || signal == SignalMetrics
}
func setOptional(values url.Values, key, value string) {
	if value = strings.TrimSpace(value); value != "" {
		values.Set(key, value)
	}
}
func equalityFilter(field, value string) string {
	if strings.TrimSpace(value) == "" {
		return ""
	}
	return field + " = '" + escapeFilter(value) + "'"
}
func containsFilter(field, value string) string {
	if strings.TrimSpace(value) == "" {
		return ""
	}
	return field + " CONTAINS '" + escapeFilter(value) + "'"
}
func safeFilterLiteral(value string) bool {
	return !strings.ContainsAny(value, "'\"\\\r\n\x00") && !strings.ContainsFunc(value, func(char rune) bool { return char < 0x20 || char == 0x7f })
}
func safeRequestLiterals(values ...string) bool {
	for _, value := range values {
		if !safeFilterLiteral(value) {
			return false
		}
	}
	return true
}
func validFieldRequest(request FieldRequest, includeName bool) bool {
	values := []string{request.MetricName, request.SearchText, request.Context, request.DataType}
	if includeName {
		values = append(values, request.Name)
	}
	return safeRequestLiterals(values...)
}
func escapeFilter(value string) string {
	return strings.TrimSpace(value)
}
func combineFilters(filters ...string) string {
	var out []string
	for _, filter := range filters {
		if filter = strings.TrimSpace(filter); filter != "" {
			out = append(out, "("+filter+")")
		}
	}
	return strings.Join(out, " AND ")
}
