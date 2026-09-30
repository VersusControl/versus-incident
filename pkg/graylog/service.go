// Package graylog provides bounded, scoped, read-only Graylog queries for native tools.
package graylog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/providerhttp"
)

var ErrInvalidArgument = errors.New("graylog: invalid argument")
var ErrInvalidConfig = errors.New("graylog: invalid configuration")
var ErrResponseTooLarge = errors.New("graylog: response too large")

const MaximumLimit = 100
const MaximumOffset = 99
const maximumResponseBytes = 512 << 10
const maximumSearchRequests = 16

type Policy struct {
	MaxRows, MaxFields, MaxBytes int
	MaxLookback, Timeout         time.Duration
}

func ToolPolicy() Policy {
	return Policy{MaxRows: 100, MaxFields: 40, MaxBytes: 128 << 10, MaxLookback: 6 * time.Hour, Timeout: 10 * time.Second}
}

type Service struct {
	client   *http.Client
	address  string
	config   config.AgentGraylogSourceConfig
	scope    []clause
	policy   Policy
	scrubber core.Scrubber
	now      func() time.Time
}

func NewService(cfg config.AgentGraylogSourceConfig, scrubber core.Scrubber) (*Service, error) {
	address, err := providerhttp.NormalizeOrigin(cfg.Address)
	if err != nil {
		return nil, ErrInvalidConfig
	}
	scope, err := parseScope(cfg.Query)
	if err != nil {
		return nil, err
	}
	transportPolicy := providerhttp.Policy{AllowLoopback: true, AllowPrivate: true, InsecureSkipVerify: cfg.InsecureSkipVerify}
	parsed, _ := url.Parse(address)
	if !providerhttp.HostAllowed(parsed.Hostname(), transportPolicy) ||
		(cfg.StreamID != "" && !exactValue.MatchString(cfg.StreamID)) ||
		strings.ContainsAny(cfg.APIToken+cfg.Username+cfg.Password, "\r\n") ||
		(cfg.Password != "" && cfg.Username == "" && cfg.APIToken == "") {
		return nil, ErrInvalidConfig
	}
	if (cfg.APIToken != "" || cfg.Username != "" || cfg.Password != "") && parsed.Scheme == "http" {
		if parsed.Hostname() != "localhost" && parsed.Hostname() != "127.0.0.1" && parsed.Hostname() != "::1" {
			return nil, ErrInvalidConfig
		}
		transportPolicy.RequireLoopback = true
	}
	policy := ToolPolicy()
	return &Service{address: address, config: cfg, scope: scope, policy: policy, scrubber: scrubber, now: time.Now,
		client: &http.Client{Transport: providerhttp.NewTransport(transportPolicy), CheckRedirect: providerhttp.RefuseRedirects}}, nil
}

type DiscoveryRequest struct {
	Search        string
	Offset, Limit int
}

type DiscoveryResult struct {
	Fields     []string `json:"fields"`
	Count      int      `json:"count"`
	Offset     int      `json:"offset"`
	Limit      int      `json:"limit"`
	NextOffset int      `json:"next_offset,omitempty"`
	Truncated  bool     `json:"truncated"`
	Truncation []string `json:"truncation"`
	Coverage   string   `json:"coverage"`
}

// Discover samples a bounded recent search; the absolute search API does not
// promise a complete field inventory for the configured query and stream.
func (service *Service) Discover(ctx context.Context, request DiscoveryRequest) (DiscoveryResult, error) {
	if request.Offset < 0 || request.Offset > MaximumOffset || request.Limit < 0 || request.Limit > MaximumLimit || len(request.Search) > 128 || strings.ContainsAny(request.Search, "\r\n\x00") {
		return DiscoveryResult{}, ErrInvalidArgument
	}
	if request.Limit == 0 {
		request.Limit = 25
	}
	ctx, cancel := context.WithTimeout(ctx, service.policy.Timeout)
	defer cancel()
	response, err := service.search(ctx, nil, time.Hour, 0, service.policy.MaxRows)
	if err != nil {
		return DiscoveryResult{}, err
	}
	fields := map[string]bool{}
	fieldLimited := false
	for _, hit := range response.Messages {
		names := make([]string, 0, len(hit.Message))
		for name := range hit.Message {
			names = append(names, name)
		}
		sort.Strings(names)
		visible := map[string]bool{}
		for _, name := range names {
			value := hit.Message[name]
			if !service.visibleField(name) || !supportedValue(value) {
				continue
			}
			name = service.scrub(name, 128)
			if visible[name] {
				continue
			}
			if len(visible) >= service.policy.MaxFields {
				fieldLimited = true
				break
			}
			visible[name] = true
			if strings.Contains(strings.ToLower(name), strings.ToLower(request.Search)) {
				fields[name] = true
			}
		}
	}
	items := make([]string, 0, len(fields))
	for name := range fields {
		items = append(items, name)
	}
	sort.Strings(items)
	start := min(request.Offset, len(items))
	result := DiscoveryResult{Offset: request.Offset, Limit: request.Limit, Coverage: "recent_sample", Truncated: true, Truncation: []string{"sample_only"}}
	if fieldLimited {
		result.Truncation = append(result.Truncation, "field_limit")
	}
	for index := start; index < len(items) && result.Count < request.Limit; index++ {
		candidate := append(result.Fields, items[index])
		if len(candidate) > service.policy.MaxFields {
			if !fieldLimited {
				result.Truncation = append(result.Truncation, "field_limit")
			}
			break
		}
		if encodedSize(candidate) > service.policy.MaxBytes {
			result.Truncation = append(result.Truncation, "byte_limit")
			break
		}
		result.Fields = candidate
		result.Count++
	}
	if start+result.Count < len(items) {
		result.Truncation = append(result.Truncation, "page_limit")
		if request.Offset+result.Count <= MaximumOffset {
			result.NextOffset = request.Offset + result.Count
		} else {
			result.Truncation = append(result.Truncation, "offset_limit")
		}
	}
	if response.TotalResults >= service.policy.MaxRows {
		result.Truncation = append(result.Truncation, "row_limit")
	}
	return result, nil
}

type ReadRequest struct {
	Filters       map[string]string
	Search        string
	Lookback      time.Duration
	Offset, Limit int
}

type Record struct {
	Timestamp string            `json:"timestamp"`
	Message   string            `json:"message"`
	Fields    map[string]string `json:"fields"`
}

type ReadResult struct {
	Records    []Record `json:"records"`
	Count      int      `json:"count"`
	Offset     int      `json:"offset"`
	Limit      int      `json:"limit"`
	NextOffset int      `json:"next_offset,omitempty"`
	Partial    bool     `json:"partial,omitempty"`
	Truncated  bool     `json:"truncated"`
	Truncation []string `json:"truncation,omitempty"`
}

func (service *Service) Read(ctx context.Context, request ReadRequest) (ReadResult, error) {
	if request.Offset < 0 || request.Offset > MaximumOffset || request.Limit < 0 || request.Limit > MaximumLimit || request.Lookback < 0 || len(request.Filters) > 12 || (request.Search != "" && !exactValue.MatchString(request.Search)) {
		return ReadResult{}, ErrInvalidArgument
	}
	if request.Limit == 0 {
		request.Limit = 25
	}
	if request.Lookback == 0 {
		request.Lookback = time.Hour
	}
	ctx, cancel := context.WithTimeout(ctx, service.policy.Timeout)
	defer cancel()
	clamped := request.Lookback > service.policy.MaxLookback
	if clamped {
		request.Lookback = service.policy.MaxLookback
	}
	filters := make([]clause, 0, len(request.Filters)+1)
	for field, value := range request.Filters {
		if !fieldName.MatchString(field) || !exactValue.MatchString(value) {
			return ReadResult{}, ErrInvalidArgument
		}
		filters = append(filters, clause{field: field, value: value})
	}
	if request.Search != "" {
		filters = append(filters, clause{field: "message", value: request.Search})
	}
	result := ReadResult{Offset: request.Offset, Limit: request.Limit}
	unsupportedField := false
	fieldLimited := false
	fieldCollision := false
	valueTruncated := false
	byteLimited := false
	serialByteLimited := false
	inconsistentPage := false
	more := false
	batch := min(service.policy.MaxRows, request.Limit+1)
	lookaheadChecked := false
	searchRequests := 0
	for result.Count < request.Limit || (more && batch == 1 && !lookaheadChecked) {
		if err := ctx.Err(); err != nil {
			if result.Count == 0 {
				return ReadResult{}, err
			}
			result.Truncation = append(result.Truncation, "timeout")
			result.Partial = true
			break
		}
		if searchRequests >= maximumSearchRequests {
			result.Truncation = append(result.Truncation, "request_limit")
			result.Partial = true
			break
		}
		searchRequests++
		response, err := service.search(ctx, filters, request.Lookback, request.Offset+result.Count, batch)
		if errors.Is(err, ErrResponseTooLarge) {
			if batch > 1 {
				batch = max(1, batch/2)
				continue
			}
			byteLimited = true
			break
		}
		if err != nil {
			if ctx.Err() != nil && result.Count > 0 {
				result.Truncation = append(result.Truncation, "timeout")
				result.Partial = true
				break
			}
			return ReadResult{}, err
		}
		more = request.Offset+result.Count+len(response.Messages) < response.TotalResults
		if len(response.Messages) == 0 {
			inconsistentPage = more
			more = false
			break
		}
		if result.Count == request.Limit {
			lookaheadChecked = true
			more = len(response.Messages) > 0
			break
		}
		for _, hit := range response.Messages {
			if result.Count == request.Limit {
				more = true
				break
			}
			record := Record{Fields: map[string]string{}}
			names := make([]string, 0, len(hit.Message))
			for name := range hit.Message {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				if !service.visibleField(name) {
					unsupportedField = true
					continue
				}
				var text string
				switch value := hit.Message[name].(type) {
				case string:
					text = value
				case float64, bool:
					text = fmt.Sprint(value)
				default:
					unsupportedField = true
					continue
				}
				key := service.scrub(name, 128)
				if _, exists := record.Fields[key]; exists {
					fieldCollision = true
					continue
				}
				if len(record.Fields) >= service.policy.MaxFields {
					fieldLimited = true
					break
				}
				if len(text) > 512 {
					valueTruncated = true
				}
				record.Fields[key] = service.scrub(text, 512)
			}
			record.Timestamp, _ = hit.Message["timestamp"].(string)
			if len(record.Timestamp) > 64 {
				valueTruncated = true
			}
			record.Timestamp = service.scrub(record.Timestamp, 64)
			message, _ := hit.Message["message"].(string)
			if len(message) > 4096 {
				valueTruncated = true
			}
			record.Message = service.scrub(message, 4096)
			candidate := append(result.Records, record)
			if encodedSize(candidate) > service.policy.MaxBytes {
				byteLimited = true
				serialByteLimited = true
				more = true
				break
			}
			result.Records = candidate
			result.Count++
		}
		if byteLimited || (result.Count == request.Limit && !(more && batch == 1)) || len(response.Messages) < batch {
			break
		}
	}
	if byteLimited {
		result.Truncation = append(result.Truncation, "byte_limit")
	}
	if inconsistentPage {
		result.Truncation = append(result.Truncation, "inconsistent_results")
	}
	if more && result.Count > 0 && (!byteLimited || serialByteLimited) {
		result.Truncation = append(result.Truncation, "page_limit")
		if request.Offset+result.Count <= MaximumOffset {
			result.NextOffset = request.Offset + result.Count
		} else {
			result.Truncation = append(result.Truncation, "row_limit")
		}
	}
	if fieldLimited {
		result.Truncation = append(result.Truncation, "field_limit")
	}
	if unsupportedField {
		result.Truncation = append(result.Truncation, "unsupported_field")
	}
	if fieldCollision {
		result.Truncation = append(result.Truncation, "field_collision")
	}
	if valueTruncated {
		result.Truncation = append(result.Truncation, "value_limit")
	}
	if clamped {
		result.Truncation = append(result.Truncation, "lookback_limit")
	}
	result.Truncated = len(result.Truncation) > 0
	return result, nil
}

type searchResponse struct {
	Messages []struct {
		Message map[string]any `json:"message"`
	} `json:"messages"`
	TotalResults int `json:"total_results"`
}

func (service *Service) search(ctx context.Context, filters []clause, lookback time.Duration, offset, limit int) (searchResponse, error) {
	end := service.now().UTC()
	clauses := append([]clause(nil), service.scope...)
	clauses = append(clauses, filters...)
	query := "*"
	if len(clauses) > 0 {
		parts := make([]string, 0, len(clauses))
		for _, item := range clauses {
			parts = append(parts, item.field+":"+strconv.Quote(item.value))
		}
		query = strings.Join(parts, " AND ")
	}
	values := url.Values{"query": {query}, "from": {end.Add(-lookback).Format("2006-01-02T15:04:05.000Z")}, "to": {end.Format("2006-01-02T15:04:05.000Z")}, "offset": {strconv.Itoa(offset)}, "limit": {strconv.Itoa(limit)}, "sort": {"timestamp:desc"}}
	if len(service.config.Fields) > 0 {
		fields := []string{"message", "timestamp"}
		for _, field := range service.config.Fields {
			if fieldName.MatchString(field) && field != "message" && field != "timestamp" {
				fields = append(fields, field)
			}
		}
		values.Set("fields", strings.Join(fields, ","))
	}
	if service.config.StreamID != "" {
		values.Set("filter", "streams:"+service.config.StreamID)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, service.address+"/api/search/universal/absolute?"+values.Encode(), nil)
	if err != nil {
		return searchResponse{}, ErrInvalidConfig
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Requested-By", "versus-incident")
	if service.config.APIToken != "" {
		req.SetBasicAuth(service.config.APIToken, "token")
	} else if service.config.Username != "" {
		req.SetBasicAuth(service.config.Username, service.config.Password)
	}
	resp, err := service.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return searchResponse{}, ctx.Err()
		}
		return searchResponse{}, errors.New("graylog: read failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return searchResponse{}, fmt.Errorf("graylog: status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maximumResponseBytes+1))
	if err != nil {
		return searchResponse{}, errors.New("graylog: read failed")
	}
	if len(data) > maximumResponseBytes {
		return searchResponse{}, ErrResponseTooLarge
	}
	var result searchResponse
	if json.Unmarshal(data, &result) != nil || result.TotalResults < 0 || len(result.Messages) > limit {
		return searchResponse{}, errors.New("graylog: invalid response")
	}
	return result, nil
}

func (service *Service) scrub(value string, maximum int) string {
	for _, secret := range []string{service.config.APIToken, service.config.Username, service.config.Password} {
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

func (service *Service) visibleField(name string) bool {
	if !fieldName.MatchString(name) {
		return false
	}
	if len(service.config.Fields) == 0 || name == "message" || name == "timestamp" {
		return true
	}
	for _, field := range service.config.Fields {
		if field == name {
			return true
		}
	}
	return false
}

func supportedValue(value any) bool {
	switch value.(type) {
	case string, float64, bool:
		return true
	default:
		return false
	}
}

func encodedSize(value any) int { data, _ := json.Marshal(value); return len(data) }
