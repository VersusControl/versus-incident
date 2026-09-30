// Package prometheus provides bounded, normalized reads over Prometheus.
package prometheus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/signalsources"
)

var (
	ErrInvalidArgument  = errors.New("prometheus: invalid argument")
	ErrInvalidConfig    = errors.New("prometheus: invalid configuration")
	ErrResponseTooLarge = errors.New("prometheus: response too large")
)

const (
	MaximumLimit  = 100
	MaximumOffset = 10_000
)

type Reader interface {
	MetadataFor(context.Context, string, time.Time, time.Time) (map[string]signalsources.MetricMeta, error)
	LabelValuesLimited(context.Context, string, time.Time, time.Time, int, ...string) ([]string, error)
	QueryRange(context.Context, string, time.Time, time.Time, time.Duration) ([]signalsources.MetricSeries, error)
}

type Policy struct {
	MaxLookback   time.Duration
	MinStep       time.Duration
	MaxStep       time.Duration
	MaxSeries     int
	MaxDatapoints int
	MaxBytes      int
	Timeout       time.Duration
}

func ToolPolicy() Policy {
	return Policy{MaxLookback: 6 * time.Hour, MinStep: 15 * time.Second, MaxStep: time.Hour, MaxSeries: 50, MaxDatapoints: 2_000, MaxBytes: 256 << 10, Timeout: 10 * time.Second}
}

type Config struct {
	ScopeFilter string
	Policy      Policy
	Scrubber    core.Scrubber
	Secrets     []string
	Now         func() time.Time
}

type Service struct {
	reader   Reader
	selector selector
	policy   Policy
	scrubber core.Scrubber
	secrets  []string
	now      func() time.Time
}

func NewService(reader Reader, config Config) (*Service, error) {
	if reader == nil {
		return nil, ErrInvalidConfig
	}
	scope, err := parseSelector(config.ScopeFilter)
	if err != nil {
		return nil, fmt.Errorf("%w: selector", ErrInvalidConfig)
	}
	policy := config.Policy
	defaults := ToolPolicy()
	if policy.MaxLookback <= 0 {
		policy.MaxLookback = defaults.MaxLookback
	}
	if policy.MinStep <= 0 {
		policy.MinStep = defaults.MinStep
	}
	if policy.MaxStep <= 0 {
		policy.MaxStep = defaults.MaxStep
	}
	if policy.MaxStep < policy.MinStep {
		policy.MaxStep = policy.MinStep
	}
	if policy.MaxSeries <= 0 {
		policy.MaxSeries = defaults.MaxSeries
	}
	if policy.MaxDatapoints <= 0 {
		policy.MaxDatapoints = defaults.MaxDatapoints
	}
	if policy.MaxBytes <= 0 {
		policy.MaxBytes = defaults.MaxBytes
	}
	if policy.Timeout <= 0 {
		policy.Timeout = defaults.Timeout
	}
	now := config.Now
	if now == nil {
		now = time.Now
	}
	secrets := make([]string, 0, len(config.Secrets))
	for _, secret := range config.Secrets {
		if secret != "" {
			secrets = append(secrets, secret)
		}
	}
	return &Service{reader: reader, selector: scope, policy: policy, scrubber: config.Scrubber, secrets: secrets, now: now}, nil
}

type DiscoveryRequest struct {
	Search string
	Offset int
	Limit  int
}

type Metric struct {
	Name              string   `json:"name"`
	Type              string   `json:"type,omitempty"`
	Help              string   `json:"help,omitempty"`
	Unit              string   `json:"unit,omitempty"`
	ServiceLabel      string   `json:"service_label,omitempty"`
	Services          []string `json:"services,omitempty"`
	ServicesTruncated bool     `json:"services_truncated,omitempty"`
}

type DiscoveryResult struct {
	Metrics    []Metric `json:"metrics"`
	Count      int      `json:"count"`
	Offset     int      `json:"offset"`
	Limit      int      `json:"limit"`
	NextOffset int      `json:"next_offset,omitempty"`
	Truncated  bool     `json:"truncated"`
	Truncation []string `json:"truncation,omitempty"`
}

func (service *Service) Discover(ctx context.Context, request DiscoveryRequest) (DiscoveryResult, error) {
	if request.Offset < 0 || request.Offset > MaximumOffset || request.Limit < 0 || request.Limit > MaximumLimit || len(request.Search) > 256 {
		return DiscoveryResult{}, ErrInvalidArgument
	}
	if request.Limit == 0 {
		request.Limit = 25
	}
	ctx, cancel := context.WithTimeout(ctx, service.policy.Timeout)
	defer cancel()
	end := service.now().UTC()
	start := end.Add(-service.policy.MaxLookback)
	search := strings.TrimSpace(request.Search)
	discoverySelector := service.selector
	if search != "" {
		discoverySelector = discoverySelector.with("__name__", "=~", strconv.Quote("(?i:.*"+regexp.QuoteMeta(search)+".*)"))
	}
	matchers := []string(nil)
	if rendered := discoverySelector.render(); rendered != "" {
		matchers = []string{rendered}
	}
	names, err := service.reader.LabelValuesLimited(ctx, "__name__", start, end, request.Offset+request.Limit+1, matchers...)
	if err != nil {
		return DiscoveryResult{}, safeError(err)
	}
	unique := make(map[string]struct{}, len(names))
	lowerSearch := strings.ToLower(search)
	for _, name := range names {
		if validMetricName(name) && (lowerSearch == "" || strings.Contains(strings.ToLower(name), lowerSearch)) {
			unique[name] = struct{}{}
		}
	}
	names = names[:0]
	for name := range unique {
		names = append(names, name)
	}
	sort.Strings(names)
	if request.Offset > len(names) {
		request.Offset = len(names)
	}
	endIndex := min(request.Offset+request.Limit, len(names))
	result := DiscoveryResult{Offset: request.Offset, Limit: request.Limit, Truncated: endIndex < len(names)}
	if result.Truncated {
		result.NextOffset = endIndex
		result.Truncation = append(result.Truncation, "page_limit")
	}
	for _, name := range names[request.Offset:endIndex] {
		metadata, readErr := service.reader.MetadataFor(ctx, name, start, end)
		if readErr != nil {
			return DiscoveryResult{}, safeError(readErr)
		}
		item := Metric{Name: name}
		if meta, ok := metadata[name]; ok {
			item.Type = service.scrub(meta.Type, 32)
			item.Help = service.scrub(meta.Help, 512)
			item.Unit = service.scrub(meta.Unit, 64)
		}
		item.ServiceLabel, item.Services, item.ServicesTruncated, err = service.serviceEvidence(ctx, name, start, end)
		if err != nil {
			return DiscoveryResult{}, err
		}
		if item.ServicesTruncated {
			result.Truncated = true
			result.Truncation = appendUnique(result.Truncation, "service_limit")
		}
		candidate := append(result.Metrics, item)
		if encodedSize(candidate) > service.policy.MaxBytes {
			result.Truncated = true
			result.Truncation = appendUnique(result.Truncation, "byte_limit")
			break
		}
		result.Metrics = candidate
	}
	result.Count = len(result.Metrics)
	if slices.Contains(result.Truncation, "byte_limit") && result.Count > 0 {
		result.NextOffset = result.Offset + result.Count
	}
	return result, nil
}

type ReadRequest struct {
	MetricName string
	Service    string
	Lookback   time.Duration
	Step       time.Duration
}

type Sample struct {
	Timestamp time.Time `json:"timestamp"`
	Value     float64   `json:"value"`
}

type Series struct {
	Labels  map[string]string `json:"labels"`
	Samples []Sample          `json:"samples"`
}

type ReadResult struct {
	Series     []Series `json:"series"`
	Count      int      `json:"count"`
	Datapoints int      `json:"datapoints"`
	Step       string   `json:"step"`
	Truncated  bool     `json:"truncated"`
	Truncation []string `json:"truncation,omitempty"`
}

func (service *Service) Read(ctx context.Context, request ReadRequest) (ReadResult, error) {
	if !validMetricName(request.MetricName) || !validService(request.Service) {
		return ReadResult{}, ErrInvalidArgument
	}
	if request.Lookback <= 0 {
		request.Lookback = time.Hour
	}
	if request.Lookback > service.policy.MaxLookback {
		request.Lookback = service.policy.MaxLookback
	}
	if request.Step <= 0 {
		request.Step = time.Minute
	}
	if request.Step < service.policy.MinStep {
		request.Step = service.policy.MinStep
	}
	if request.Step > service.policy.MaxStep {
		request.Step = service.policy.MaxStep
	}
	ctx, cancel := context.WithTimeout(ctx, service.policy.Timeout)
	defer cancel()
	end := service.now().UTC()
	start := end.Add(-request.Lookback)
	label, err := service.resolveServiceLabel(ctx, request.MetricName, request.Service, start, end)
	if err != nil {
		return ReadResult{}, err
	}
	query := request.MetricName + service.selector.intersect(label, "=", strconv.Quote(request.Service)).render()
	upstream, err := service.reader.QueryRange(ctx, query, start, end, request.Step)
	if err != nil {
		return ReadResult{}, safeError(err)
	}
	result := ReadResult{Step: request.Step.String()}
	for _, source := range upstream {
		if len(result.Series) >= service.policy.MaxSeries {
			result.Truncated = true
			result.Truncation = appendUnique(result.Truncation, "series_limit")
			break
		}
		item := Series{Labels: make(map[string]string, len(source.Metric))}
		for key, value := range source.Metric {
			item.Labels[service.scrub(key, 128)] = service.scrub(value, 512)
		}
		for _, point := range source.Samples {
			if result.Datapoints >= service.policy.MaxDatapoints {
				result.Truncated = true
				result.Truncation = appendUnique(result.Truncation, "datapoint_limit")
				break
			}
			item.Samples = append(item.Samples, Sample{Timestamp: point.Timestamp.UTC(), Value: point.Value})
			result.Datapoints++
		}
		candidate := append(result.Series, item)
		if encodedSize(candidate) > service.policy.MaxBytes {
			result.Datapoints -= len(item.Samples)
			result.Truncated = true
			result.Truncation = appendUnique(result.Truncation, "byte_limit")
			break
		}
		result.Series = candidate
		if result.Datapoints >= service.policy.MaxDatapoints {
			break
		}
	}
	result.Count = len(result.Series)
	return result, nil
}

var serviceLabelCandidates = []string{"service", "service_name", "app_kubernetes_io_name", "app", "job"}

func (service *Service) serviceEvidence(ctx context.Context, metric string, start, end time.Time) (string, []string, bool, error) {
	for _, label := range serviceLabelCandidates {
		values, err := service.reader.LabelValuesLimited(ctx, label, start, end, MaximumLimit+1, metric+service.selector.render())
		if err != nil {
			return "", nil, false, safeError(err)
		}
		values, truncated := boundedStrings(values, MaximumLimit, service.scrub)
		if len(values) > 0 {
			return label, values, truncated, nil
		}
	}
	return "", nil, false, nil
}

func (service *Service) resolveServiceLabel(ctx context.Context, metric, requested string, start, end time.Time) (string, error) {
	for _, label := range serviceLabelCandidates {
		values, err := service.reader.LabelValuesLimited(ctx, label, start, end, MaximumLimit+1, metric+service.selector.render())
		if err != nil {
			return "", safeError(err)
		}
		for _, value := range values {
			if value == requested {
				return label, nil
			}
		}
	}
	return "", ErrInvalidArgument
}

type matcher struct{ label, operator, value string }
type selector []matcher

var matcherPattern = regexp.MustCompile(`^([a-zA-Z_][a-zA-Z0-9_]*)(=~|!~|=|!=)("(?:[^"\\]|\\.)*")$`)
var metricPattern = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)

func parseSelector(raw string) (selector, error) {
	value := strings.TrimSpace(raw)
	if value == "" || value == "*" || value == "{}" {
		return nil, nil
	}
	open := strings.IndexByte(value, '{')
	if open < 0 || !strings.HasSuffix(value, "}") {
		return nil, ErrInvalidConfig
	}
	body := strings.TrimSpace(value[open+1 : len(value)-1])
	if body == "" {
		return nil, nil
	}
	parts, err := splitMatchers(body)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	result := make(selector, 0, len(parts))
	for _, part := range parts {
		match := matcherPattern.FindStringSubmatch(strings.TrimSpace(part))
		if match == nil || match[1] == "__name__" || seen[match[1]] {
			return nil, ErrInvalidConfig
		}
		seen[match[1]] = true
		result = append(result, matcher{label: match[1], operator: match[2], value: match[3]})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].label < result[j].label })
	return result, nil
}

func splitMatchers(body string) ([]string, error) {
	var result []string
	start, quoted, escaped := 0, false, false
	for index, char := range body {
		switch {
		case escaped:
			escaped = false
		case quoted && char == '\\':
			escaped = true
		case char == '"':
			quoted = !quoted
		case char == ',' && !quoted:
			result = append(result, body[start:index])
			start = index + 1
		}
	}
	if quoted {
		return nil, ErrInvalidConfig
	}
	return append(result, body[start:]), nil
}

func (value selector) with(label, operator, operand string) selector {
	result := append(selector(nil), value...)
	for _, item := range result {
		if item.label == label {
			return result
		}
	}
	result = append(result, matcher{label: label, operator: operator, value: operand})
	sort.Slice(result, func(i, j int) bool { return result[i].label < result[j].label })
	return result
}

func (value selector) intersect(label, operator, operand string) selector {
	result := append(selector(nil), value...)
	result = append(result, matcher{label: label, operator: operator, value: operand})
	sort.SliceStable(result, func(i, j int) bool { return result[i].label < result[j].label })
	return result
}

func (value selector) render() string {
	if len(value) == 0 {
		return ""
	}
	parts := make([]string, len(value))
	for index, item := range value {
		parts[index] = item.label + item.operator + item.value
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func validMetricName(value string) bool { return metricPattern.MatchString(value) && len(value) <= 256 }
func validService(value string) bool {
	return strings.TrimSpace(value) == value && value != "" && len(value) <= 256 && !strings.ContainsAny(value, "\r\n\x00")
}
func encodedSize(value any) int { encoded, _ := json.Marshal(value); return len(encoded) }
func appendUnique(values []string, value string) []string {
	for _, item := range values {
		if item == value {
			return values
		}
	}
	return append(values, value)
}
func boundedStrings(values []string, limit int, scrub func(string, int) string) ([]string, bool) {
	unique := map[string]struct{}{}
	for _, value := range values {
		if value != "" {
			unique[scrub(value, 256)] = struct{}{}
		}
	}
	result := make([]string, 0, len(unique))
	for value := range unique {
		result = append(result, value)
	}
	sort.Strings(result)
	truncated := len(result) > limit
	if len(result) > limit {
		result = result[:limit]
	}
	return result, truncated
}
func (service *Service) scrub(value string, limit int) string {
	for _, secret := range service.secrets {
		value = strings.ReplaceAll(value, secret, "[redacted]")
	}
	if service.scrubber != nil {
		value = service.scrubber.Scrub(value)
	}
	if len(value) > limit {
		value = value[:limit]
	}
	return value
}
func safeError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, signalsources.ErrPrometheusResponseTooLarge) {
		return ErrResponseTooLarge
	}
	return fmt.Errorf("prometheus read failed")
}
