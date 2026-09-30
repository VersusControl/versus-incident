// Package loki provides bounded, scoped read-only Loki queries for interactive tools.
package loki

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/providerhttp"
)

var ErrInvalidArgument = errors.New("loki: invalid argument")
var ErrInvalidConfig = errors.New("loki: invalid configuration")
var ErrResponseTooLarge = errors.New("loki: response too large")

const MaximumLimit = 100
const MaximumOffset = 1000
const maximumResponseBytes = 512 << 10

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
	config   config.AgentLokiSourceConfig
	scope    []matcher
	policy   Policy
	scrubber core.Scrubber
	now      func() time.Time
}

// NewService constructs an isolated read transport; it does not change the ingestion client.
func NewService(cfg config.AgentLokiSourceConfig, scrubber core.Scrubber) (*Service, error) {
	address, err := providerhttp.NormalizeOrigin(cfg.Address)
	if err != nil || cfg.Query == "" {
		return nil, ErrInvalidConfig
	}
	// The endpoint is operator-authored. Explicit private/loopback destinations are
	// permitted for an internal Loki installation, but DNS and redirects stay guarded.
	transportPolicy := providerhttp.Policy{AllowLoopback: true, AllowPrivate: true, InsecureSkipVerify: cfg.InsecureSkipVerify}
	parsed, _ := url.Parse(address)
	if !providerhttp.HostAllowed(parsed.Hostname(), transportPolicy) || strings.ContainsAny(cfg.TenantID, "\r\n") || strings.ContainsAny(cfg.BearerToken, "\r\n") || strings.ContainsAny(cfg.Username, "\r\n") || strings.ContainsAny(cfg.Password, "\r\n") {
		return nil, ErrInvalidConfig
	}
	if (cfg.BearerToken != "" || cfg.Username != "" || cfg.Password != "") && parsed.Scheme != "https" && !strings.EqualFold(parsed.Hostname(), "localhost") {
		if ip := parsed.Hostname(); ip != "127.0.0.1" && ip != "[::1]" && ip != "::1" {
			return nil, ErrInvalidConfig
		}
	}
	if parsed.Scheme == "http" && (cfg.BearerToken != "" || cfg.Username != "" || cfg.Password != "") {
		transportPolicy.RequireLoopback = true
	}
	scope, scopeErr := parseSelector(cfg.Query)
	if cfg.ServiceLabel != "" && !labelName.MatchString(cfg.ServiceLabel) {
		return nil, ErrInvalidConfig
	}
	if scopeErr == nil && len(scope) > 0 && !selectorValid(scope) {
		scopeErr = ErrUnsupportedScope
	}
	policy := ToolPolicy()
	return &Service{address: address, config: cfg, scope: scope, policy: policy, scrubber: scrubber, now: time.Now,
		client: &http.Client{Transport: providerhttp.NewTransport(transportPolicy), Timeout: policy.Timeout, CheckRedirect: providerhttp.RefuseRedirects}}, scopeErr
}

func selectorValid(scope []matcher) bool {
	if len(scope) == 0 {
		return false
	}
	for _, match := range scope {
		value, _ := strconv.Unquote(match.literal)
		switch match.operator {
		case "=":
			if value != "" {
				return true
			}
		case "=~":
			expression, err := regexp.Compile("^(?:" + value + ")$")
			if err == nil && !expression.MatchString("") {
				return true
			}
		}
	}
	return false
}

// DiscoveryAvailable is true when scope can be enforced by the series endpoint.
func (service *Service) DiscoveryAvailable() bool { return service != nil }

type DiscoveryRequest struct {
	Label, Search string
	Offset, Limit int
}

type DiscoveryResult struct {
	Labels     []string `json:"labels"`
	Count      int      `json:"count"`
	Offset     int      `json:"offset"`
	Limit      int      `json:"limit"`
	NextOffset int      `json:"next_offset,omitempty"`
	Truncated  bool     `json:"truncated"`
	Truncation []string `json:"truncation,omitempty"`
}

func (service *Service) Discover(ctx context.Context, request DiscoveryRequest) (DiscoveryResult, error) {
	if !service.DiscoveryAvailable() {
		return DiscoveryResult{}, ErrUnsupportedScope
	}
	if request.Offset < 0 || request.Offset > MaximumOffset || request.Limit < 0 || request.Limit > MaximumLimit || len(request.Search) > 128 || (request.Label != "" && !labelName.MatchString(request.Label)) {
		return DiscoveryResult{}, ErrInvalidArgument
	}
	if request.Limit == 0 {
		request.Limit = 25
	}
	path := "/loki/api/v1/labels"
	if len(service.scope) != 0 {
		path = "/loki/api/v1/series"
	} else if request.Label != "" {
		path = "/loki/api/v1/label/" + request.Label + "/values"
	}
	ctx, cancel := context.WithTimeout(ctx, service.policy.Timeout)
	defer cancel()
	end := service.now().UTC()
	values := url.Values{"start": {strconv.FormatInt(end.Add(-service.policy.MaxLookback).UnixNano(), 10)}, "end": {strconv.FormatInt(end.UnixNano(), 10)}}
	if len(service.scope) != 0 {
		values.Set("match[]", renderSelector(service.scope))
	}
	data, err := service.get(ctx, path, values)
	if err != nil {
		return DiscoveryResult{}, err
	}
	var envelope struct {
		Status string          `json:"status"`
		Data   json.RawMessage `json:"data"`
	}
	if json.Unmarshal(data, &envelope) != nil || envelope.Status != "success" {
		return DiscoveryResult{}, errors.New("loki: invalid response")
	}
	var candidates []string
	if len(service.scope) == 0 {
		if json.Unmarshal(envelope.Data, &candidates) != nil {
			return DiscoveryResult{}, errors.New("loki: invalid response")
		}
	} else {
		var series []map[string]string
		if json.Unmarshal(envelope.Data, &series) != nil {
			return DiscoveryResult{}, errors.New("loki: invalid response")
		}
		for _, labels := range series {
			if request.Label != "" {
				if value, ok := labels[request.Label]; ok {
					candidates = append(candidates, value)
				}
			} else {
				for name := range labels {
					candidates = append(candidates, name)
				}
			}
		}
	}
	unique := map[string]bool{}
	for _, item := range candidates {
		if strings.Contains(strings.ToLower(item), strings.ToLower(request.Search)) {
			unique[service.scrub(item, 256)] = true
		}
	}
	items := make([]string, 0, len(unique))
	for item := range unique {
		items = append(items, item)
	}
	sort.Strings(items)
	start := min(request.Offset, len(items))
	stop := min(start+request.Limit, len(items))
	result := DiscoveryResult{Offset: start, Limit: request.Limit}
	for _, item := range items[start:stop] {
		candidate := append(result.Labels, item)
		if encodedSize(candidate) > service.policy.MaxBytes || len(candidate) > service.policy.MaxFields {
			result.Truncated = true
			if len(candidate) > service.policy.MaxFields {
				result.Truncation = append(result.Truncation, "field_limit")
			} else {
				result.Truncation = append(result.Truncation, "byte_limit")
			}
			break
		}
		result.Labels = candidate
	}
	result.Count = len(result.Labels)
	if start+result.Count < len(items) {
		result.Truncated = true
		result.NextOffset = start + result.Count
		if len(result.Truncation) == 0 {
			result.Truncation = append(result.Truncation, "page_limit")
		}
		if result.NextOffset > MaximumOffset {
			result.NextOffset = 0
			result.Truncation = append(result.Truncation, "offset_limit")
		}
	}
	return result, nil
}

type ReadRequest struct {
	Labels          map[string]string
	Service, Search string
	Lookback        time.Duration
	Offset, Limit   int
}

type Record struct {
	Timestamp time.Time         `json:"timestamp"`
	Message   string            `json:"message"`
	Labels    map[string]string `json:"labels"`
}

type ReadResult struct {
	Records    []Record `json:"records"`
	Count      int      `json:"count"`
	Offset     int      `json:"offset"`
	Limit      int      `json:"limit"`
	NextOffset int      `json:"next_offset,omitempty"`
	Truncated  bool     `json:"truncated"`
	Truncation []string `json:"truncation,omitempty"`
}

func (service *Service) Read(ctx context.Context, request ReadRequest) (ReadResult, error) {
	if request.Offset < 0 || request.Offset >= service.policy.MaxRows || request.Limit < 0 || request.Limit > MaximumLimit || request.Lookback < 0 || len(request.Labels) > 12 || !validLiteral(request.Service) || !validLiteral(request.Search) {
		return ReadResult{}, ErrInvalidArgument
	}
	if request.Limit == 0 {
		request.Limit = 25
	}
	if request.Lookback <= 0 {
		request.Lookback = time.Hour
	}
	lookbackClamped := request.Lookback > service.policy.MaxLookback
	if lookbackClamped {
		request.Lookback = service.policy.MaxLookback
	}
	matchers := slices.Clone(service.scope)
	for label, value := range request.Labels {
		if !labelName.MatchString(label) || !validLiteral(value) || value == "" {
			return ReadResult{}, ErrInvalidArgument
		}
		matchers = append(matchers, matcher{label, "=", strconv.Quote(value)})
	}
	if request.Service != "" {
		label := service.config.ServiceLabel
		if label == "" {
			label = "service"
			if len(service.scope) > 0 {
				found := false
				for _, match := range service.scope {
					found = found || match.name == label
				}
				if !found {
					return ReadResult{}, ErrInvalidArgument
				}
			}
		}
		matchers = append(matchers, matcher{label, "=", strconv.Quote(request.Service)})
	}
	if !selectorValid(matchers) {
		return ReadResult{}, ErrInvalidArgument
	}
	query := renderSelector(matchers)
	if request.Search != "" {
		query += " |= " + strconv.Quote(request.Search)
	}
	ctx, cancel := context.WithTimeout(ctx, service.policy.Timeout)
	defer cancel()
	end := service.now().UTC()
	values := url.Values{"query": {query}, "start": {strconv.FormatInt(end.Add(-request.Lookback).UnixNano(), 10)}, "end": {strconv.FormatInt(end.UnixNano(), 10)}, "direction": {"backward"}, "limit": {strconv.Itoa(min(service.policy.MaxRows, request.Offset+request.Limit+1))}}
	data, err := service.get(ctx, "/loki/api/v1/query_range", values)
	if err != nil {
		return ReadResult{}, err
	}
	var response struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Stream map[string]string `json:"stream"`
				Values [][]string        `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if json.Unmarshal(data, &response) != nil || response.Status != "success" || response.Data.ResultType != "streams" {
		return ReadResult{}, errors.New("loki: invalid response")
	}
	all := make([]Record, 0)
	fieldsTruncated := false
	for _, stream := range response.Data.Result {
		for _, entry := range stream.Values {
			if len(entry) != 2 {
				continue
			}
			nanos, parseErr := strconv.ParseInt(entry[0], 10, 64)
			if parseErr != nil {
				continue
			}
			labels := make(map[string]string)
			names := make([]string, 0, len(stream.Stream))
			for name := range stream.Stream {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				if len(labels) >= service.policy.MaxFields {
					fieldsTruncated = true
					break
				}
				labels[service.scrub(name, 128)] = service.scrub(stream.Stream[name], 256)
			}
			all = append(all, Record{Timestamp: time.Unix(0, nanos).UTC(), Message: service.scrub(entry[1], 4096), Labels: labels})
		}
	}
	sort.SliceStable(all, func(left, right int) bool { return all[left].Timestamp.After(all[right].Timestamp) })
	result := ReadResult{Offset: request.Offset, Limit: request.Limit}
	for index := request.Offset; index < len(all) && result.Count < request.Limit; index++ {
		candidate := append(result.Records, all[index])
		if encodedSize(candidate) > service.policy.MaxBytes {
			result.Truncation = append(result.Truncation, "byte_limit")
			break
		}
		result.Records = candidate
		result.Count++
	}
	if request.Offset+result.Count < len(all) {
		result.Truncation = append(result.Truncation, "page_limit")
		result.NextOffset = request.Offset + result.Count
	}
	if len(all) >= service.policy.MaxRows {
		result.Truncation = append(result.Truncation, "row_limit")
		if request.Offset+result.Count >= service.policy.MaxRows {
			result.NextOffset = 0
		}
	}
	if fieldsTruncated {
		result.Truncation = append(result.Truncation, "field_limit")
	}
	if lookbackClamped {
		result.Truncation = append(result.Truncation, "lookback_limit")
	}
	result.Truncated = len(result.Truncation) > 0
	return result, nil
}

func validLiteral(value string) bool {
	return len(value) <= 256 && strings.TrimSpace(value) == value && !strings.ContainsAny(value, "\r\n\x00")
}
func encodedSize(value any) int { data, _ := json.Marshal(value); return len(data) }
func (service *Service) scrub(value string, maximum int) string {
	for _, secret := range []string{service.config.BearerToken, service.config.Username, service.config.Password, service.config.TenantID} {
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

func (service *Service) get(ctx context.Context, path string, values url.Values) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, service.address+path+"?"+values.Encode(), nil)
	if err != nil {
		return nil, ErrInvalidArgument
	}
	req.Header.Set("Accept", "application/json")
	if service.config.TenantID != "" {
		req.Header.Set("X-Scope-OrgID", service.config.TenantID)
	}
	if service.config.BearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+service.config.BearerToken)
	} else if service.config.Username != "" {
		req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(service.config.Username+":"+service.config.Password)))
	}
	resp, err := service.client.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, context.DeadlineExceeded
		}
		return nil, errors.New("loki: read failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("loki: status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maximumResponseBytes+1))
	if err != nil {
		return nil, errors.New("loki: read failed")
	}
	if len(data) > maximumResponseBytes {
		return nil, ErrResponseTooLarge
	}
	return data, nil
}
