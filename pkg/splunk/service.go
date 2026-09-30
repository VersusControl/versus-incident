// Package splunk provides bounded, scoped Splunk search-job reads for native tools.
package splunk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/providerhttp"
)

var ErrInvalidConfig = errors.New("splunk: invalid configuration")
var ErrInvalidArgument = errors.New("splunk: invalid argument")
var ErrResponseTooLarge = errors.New("splunk: response too large")
var ErrCleanup = errors.New("splunk: job cleanup unconfirmed")
var ErrRead = errors.New("splunk: read failed")

const MaximumLimit = 50
const maximumResponseBytes = 256 << 10
const maximumTotalBytes = 768 << 10
const maximumOutputBytes = 128 << 10
const maximumResponseHeaderBytes = 16 << 10
const maximumRequests = 6

var resultFields = []string{"index", "_time", "_raw", "source", "sourcetype", "host", "service", "severity", "level", "environment"}

var sidPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
var namespacePattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,63}$`)
var outputFieldPattern = regexp.MustCompile(`^[_A-Za-z][_A-Za-z0-9.]{0,63}$`)

type Service struct {
	client   *http.Client
	address  string
	path     string
	config   config.AgentSplunkSourceConfig
	scope    []clause
	index    string
	scrubber core.Scrubber
	now      func() time.Time
}

func NewService(cfg config.AgentSplunkSourceConfig, scrubber core.Scrubber) (*Service, error) {
	address, err := providerhttp.NormalizeOrigin(cfg.Address)
	if err != nil {
		return nil, ErrInvalidConfig
	}
	scope, err := parseScope(cfg.Search)
	if err != nil {
		return nil, err
	}
	policy := providerhttp.Policy{AllowLoopback: true, AllowPrivate: true, InsecureSkipVerify: cfg.InsecureSkipVerify}
	parsed, _ := url.Parse(address)
	if !providerhttp.HostAllowed(parsed.Hostname(), policy) ||
		strings.ContainsAny(cfg.Token+cfg.Username+cfg.Password, "\r\n") ||
		(cfg.Password != "" && cfg.Username == "" && cfg.Token == "") ||
		(cfg.Owner == "") != (cfg.App == "") ||
		(cfg.Owner != "" && (!namespacePattern.MatchString(cfg.Owner) || !namespacePattern.MatchString(cfg.App) || cfg.Owner == ".." || cfg.App == "..")) {
		return nil, ErrInvalidConfig
	}
	if (cfg.Token != "" || cfg.Username != "" || cfg.Password != "") && parsed.Scheme == "http" {
		if parsed.Hostname() != "localhost" && parsed.Hostname() != "127.0.0.1" && parsed.Hostname() != "::1" {
			return nil, ErrInvalidConfig
		}
		policy.RequireLoopback = true
	}
	path := "/services/search/jobs"
	if cfg.Owner != "" {
		path = "/servicesNS/" + url.PathEscape(cfg.Owner) + "/" + url.PathEscape(cfg.App) + "/search/jobs"
	}
	transport := providerhttp.NewTransport(policy)
	transport.MaxResponseHeaderBytes = maximumResponseHeaderBytes
	service := &Service{address: address, path: path, config: cfg, scope: scope, scrubber: scrubber, now: time.Now,
		client: &http.Client{Transport: transport, CheckRedirect: providerhttp.RefuseRedirects}}
	for _, item := range scope {
		if item.field == "index" {
			service.index = item.value
		}
	}
	return service, nil
}

type DiscoveryRequest struct {
	Search string
	Limit  int
}

type DiscoveryResult struct {
	Fields     []string `json:"fields"`
	Count      int      `json:"count"`
	Coverage   string   `json:"coverage"`
	Truncated  bool     `json:"truncated"`
	Truncation []string `json:"truncation"`
}

func (service *Service) Discover(ctx context.Context, request DiscoveryRequest) (DiscoveryResult, error) {
	if request.Limit < 0 || request.Limit > MaximumLimit || (request.Search != "" && !literal.MatchString(request.Search)) {
		return DiscoveryResult{}, ErrInvalidArgument
	}
	if request.Limit == 0 {
		request.Limit = 25
	}
	rows, err := service.query(ctx, nil, time.Hour, MaximumLimit)
	if err != nil {
		return DiscoveryResult{}, err
	}
	fields := map[string]bool{}
	limited := false
	for _, row := range rows {
		for key := range row {
			if (strings.HasPrefix(key, "_") && key != "_raw" && key != "_time") || !outputFieldPattern.MatchString(key) || !strings.Contains(strings.ToLower(key), strings.ToLower(request.Search)) {
				continue
			}
			if len(fields) >= 40 && !fields[key] {
				limited = true
				continue
			}
			fields[key] = true
		}
	}
	items := make([]string, 0, len(fields))
	for key := range fields {
		items = append(items, service.scrub(key, 64))
	}
	sort.Strings(items)
	result := DiscoveryResult{Coverage: "recent_sample", Truncated: true, Truncation: []string{"sample_only"}}
	for _, field := range items {
		if len(result.Fields) >= request.Limit {
			limited = true
			break
		}
		if size(result.Fields)+len(field)+4 > maximumOutputBytes {
			limited = true
			break
		}
		result.Fields = append(result.Fields, field)
	}
	result.Count = len(result.Fields)
	if limited || len(rows) == MaximumLimit {
		result.Truncation = append(result.Truncation, "sample_limit")
	}
	return result, nil
}

type ReadRequest struct {
	Filters  map[string]string
	Lookback time.Duration
	Limit    int
}

type Record struct {
	Timestamp string            `json:"timestamp"`
	Message   string            `json:"message"`
	Fields    map[string]string `json:"fields"`
}

type ReadResult struct {
	Records    []Record `json:"records"`
	Count      int      `json:"count"`
	Partial    bool     `json:"partial"`
	Truncated  bool     `json:"truncated"`
	Truncation []string `json:"truncation,omitempty"`
}

func (service *Service) Read(ctx context.Context, request ReadRequest) (ReadResult, error) {
	if request.Limit < 0 || request.Limit > MaximumLimit || request.Lookback < 0 || len(request.Filters) > 12 {
		return ReadResult{}, ErrInvalidArgument
	}
	if request.Limit == 0 {
		request.Limit = 25
	}
	if request.Lookback == 0 {
		request.Lookback = time.Hour
	}
	clamped := request.Lookback > 6*time.Hour
	if clamped {
		request.Lookback = 6 * time.Hour
	}
	filters := make([]clause, 0, len(request.Filters))
	for key, value := range request.Filters {
		if key == "index" || !allowedFields[key] || !literal.MatchString(value) {
			return ReadResult{}, ErrInvalidArgument
		}
		filters = append(filters, clause{key, value})
	}
	sort.Slice(filters, func(left, right int) bool { return filters[left].field < filters[right].field })
	rows, err := service.query(ctx, filters, request.Lookback, request.Limit)
	if err != nil {
		return ReadResult{}, err
	}
	result := ReadResult{}
	for _, row := range rows {
		record := Record{Fields: map[string]string{}}
		keys := make([]string, 0, len(row))
		for key := range row {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if strings.HasPrefix(key, "_") {
				continue
			}
			if !outputFieldPattern.MatchString(key) {
				result.Truncation = appendUnique(result.Truncation, "unsupported_field")
				continue
			}
			value, ok := row[key].(string)
			if !ok {
				result.Truncation = appendUnique(result.Truncation, "unsupported_field")
				continue
			}
			if len(value) > 512 {
				result.Truncation = appendUnique(result.Truncation, "value_limit")
			}
			if len(record.Fields) == 40 {
				result.Truncation = appendUnique(result.Truncation, "field_limit")
				break
			}
			safeKey := service.scrub(key, 64)
			if _, exists := record.Fields[safeKey]; exists {
				result.Truncation = appendUnique(result.Truncation, "field_collision")
				continue
			}
			record.Fields[safeKey] = service.scrub(value, 512)
		}
		record.Timestamp = service.scrub(stringValue(row["_time"]), 64)
		record.Message = service.scrub(stringValue(row["_raw"]), 4096)
		if size(append(result.Records, record)) > maximumOutputBytes {
			result.Truncation = appendUnique(result.Truncation, "byte_limit")
			break
		}
		result.Records = append(result.Records, record)
	}
	result.Count = len(result.Records)
	if clamped {
		result.Truncation = appendUnique(result.Truncation, "lookback_limit")
	}
	if len(rows) == request.Limit {
		result.Truncation = appendUnique(result.Truncation, "row_limit")
	}
	result.Truncated = len(result.Truncation) > 0
	result.Partial = result.Truncated
	return result, nil
}

func (service *Service) query(ctx context.Context, filters []clause, lookback time.Duration, limit int) (rows []map[string]any, err error) {
	ctx, cancel := context.WithTimeout(ctx, 9*time.Second)
	defer cancel()
	parts := make([]string, 0, len(service.scope)+len(filters))
	for _, item := range append(append([]clause(nil), service.scope...), filters...) {
		parts = append(parts, item.field+"="+item.value)
	}
	end := service.now().UTC()
	form := url.Values{"search": {"search " + strings.Join(parts, " AND ")}, "earliest_time": {strconv.FormatInt(end.Add(-lookback).Unix(), 10)}, "latest_time": {strconv.FormatInt(end.Unix(), 10)}, "output_mode": {"json"}, "exec_mode": {"normal"}, "max_count": {strconv.Itoa(limit)}, "max_time": {"8"}, "ttl": {"10"}, "auto_cancel": {"10"}}
	total := 0
	data, err := service.request(ctx, http.MethodPost, service.path, form, &total)
	if err != nil {
		return nil, err
	}
	var created struct {
		SID string `json:"sid"`
	}
	if json.Unmarshal(data, &created) != nil || !sidPattern.MatchString(created.SID) || strings.Contains(created.SID, "..") {
		return nil, ErrRead
	}
	jobPath := service.path + "/" + url.PathEscape(created.SID)
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Second)
		defer cleanupCancel()
		if _, cleanupErr := service.request(cleanupCtx, http.MethodDelete, jobPath, nil, &total); cleanupErr != nil {
			rows, err = nil, ErrCleanup
		}
	}()
	requests := 1
	for {
		if requests >= maximumRequests-1 {
			return nil, ErrRead
		}
		requests++
		data, err = service.request(ctx, http.MethodGet, jobPath+"?output_mode=json", nil, &total)
		if err != nil {
			return nil, err
		}
		var status struct {
			Entry []struct {
				Content struct {
					IsDone        *bool  `json:"isDone"`
					IsFailed      bool   `json:"isFailed"`
					IsCanceled    bool   `json:"isCanceled"`
					IsPreview     bool   `json:"isPreview"`
					IsFinalized   bool   `json:"isFinalized"`
					DispatchState string `json:"dispatchState"`
				} `json:"content"`
			} `json:"entry"`
		}
		if json.Unmarshal(data, &status) != nil || len(status.Entry) != 1 || status.Entry[0].Content.IsDone == nil {
			return nil, ErrRead
		}
		state := status.Entry[0].Content
		if state.IsFailed || state.IsCanceled || state.IsFinalized || strings.EqualFold(state.DispatchState, "FAILED") || strings.EqualFold(state.DispatchState, "CANCELED") || strings.EqualFold(state.DispatchState, "NO_DATA") || (*state.IsDone && state.IsPreview) {
			return nil, ErrRead
		}
		if *state.IsDone {
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(400 * time.Millisecond):
		}
	}
	if requests >= maximumRequests-1 {
		return nil, ErrRead
	}
	resultsPath := strings.TrimSuffix(service.path, "/jobs") + "/v2/jobs/" + url.PathEscape(created.SID) + "/results"
	query := url.Values{"output_mode": {"json"}, "count": {strconv.Itoa(limit)}, "f": resultFields}
	data, err = service.request(ctx, http.MethodGet, resultsPath+"?"+query.Encode(), nil, &total)
	if err != nil {
		return nil, err
	}
	var response struct {
		Results []map[string]any `json:"results"`
	}
	if json.Unmarshal(data, &response) != nil || response.Results == nil || len(response.Results) > limit {
		return nil, ErrRead
	}
	for _, row := range response.Results {
		index, ok := row["index"].(string)
		if !ok || strings.TrimSpace(index) == "" || !strings.EqualFold(index, service.index) {
			return nil, ErrRead
		}
	}
	return response.Results, nil
}

func (service *Service) request(ctx context.Context, method, path string, form url.Values, total *int) ([]byte, error) {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, service.address+path, body)
	if err != nil {
		return nil, ErrRead
	}
	req.Header.Set("Accept", "application/json")
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if service.config.Token != "" {
		req.Header.Set("Authorization", "Bearer "+service.config.Token)
	} else if service.config.Username != "" {
		req.SetBasicAuth(service.config.Username, service.config.Password)
	}
	response, err := service.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ErrRead
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("%w: status %d", ErrRead, response.StatusCode)
	}
	if method == http.MethodDelete {
		return nil, nil
	}
	remaining := min(maximumResponseBytes, maximumTotalBytes-*total)
	if remaining <= 0 {
		return nil, ErrResponseTooLarge
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, int64(remaining)))
	if err != nil {
		return nil, ErrRead
	}
	*total += len(data)
	if len(data) == remaining {
		return nil, ErrResponseTooLarge
	}
	return data, nil
}

func (service *Service) scrub(value string, maximum int) string {
	for _, secret := range []string{service.config.Token, service.config.Username, service.config.Password} {
		if secret != "" {
			value = strings.ReplaceAll(value, secret, "[redacted]")
		}
	}
	if service.scrubber != nil {
		value = service.scrubber.Scrub(value)
	}
	if len(value) > maximum {
		value = value[:maximum]
	}
	return value
}

func stringValue(value any) string { text, _ := value.(string); return text }
func size(value any) int           { encoded, _ := json.Marshal(value); return len(encoded) }
func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}
