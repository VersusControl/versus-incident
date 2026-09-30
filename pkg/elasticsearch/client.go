// Package elasticsearch provides bounded, read-only access to one configured
// Elasticsearch log source.
package elasticsearch

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"time"

	"github.com/VersusControl/versus-incident/pkg/config"
)

const (
	toolTimeout          = 10 * time.Second
	toolMaxResponse      = int64(1 << 20)
	ingestTimeout        = 30 * time.Second
	ingestMaxResponse    = int64(8 << 20)
	maximumRequestBytes  = 32 << 10
	maximumResponseBytes = ingestMaxResponse
)

type operationPolicy struct {
	timeout  time.Duration
	maxBytes int64
}

var (
	ErrInvalidConfig    = errors.New("elasticsearch: invalid source configuration")
	ErrInvalidArguments = errors.New("elasticsearch: invalid arguments")
	ErrUnauthorized     = errors.New("elasticsearch: unauthorized")
	ErrForbidden        = errors.New("elasticsearch: forbidden")
	ErrNotFound         = errors.New("elasticsearch: not found")
	ErrResponseTooLarge = errors.New("elasticsearch: response too large")
	ErrBackend          = errors.New("elasticsearch: backend unavailable")
	ErrShardFailure     = errors.New("elasticsearch: search failed on one or more shards")
)

// ShardStats is the `_shards` header of a search response. Elasticsearch
// reports per-shard query failures (for example sorting on an unmapped field)
// with HTTP 200 and zero hits, so callers must check it before trusting hits.
type ShardStats struct {
	Total    int `json:"total"`
	Failed   int `json:"failed"`
	Failures []struct {
		Reason struct {
			Type   string `json:"type"`
			Reason string `json:"reason"`
		} `json:"reason"`
	} `json:"failures"`
}

// Err returns ErrShardFailure with the failure count and a bounded, printable
// first reason when any shard failed, and nil otherwise.
func (stats ShardStats) Err() error {
	if stats.Failed <= 0 {
		return nil
	}
	if len(stats.Failures) == 0 {
		return fmt.Errorf("%w: %d of %d shards failed", ErrShardFailure, stats.Failed, stats.Total)
	}
	first := stats.Failures[0].Reason
	return fmt.Errorf("%w: %d of %d shards failed; first failure %s: %s", ErrShardFailure, stats.Failed, stats.Total,
		printableBounded(first.Type, 64), printableBounded(first.Reason, 256))
}

func printableBounded(value string, limit int) string {
	value = strings.Map(func(character rune) rune {
		if character < 0x20 || character == 0x7f {
			return ' '
		}
		return character
	}, value)
	runes := []rune(strings.TrimSpace(value))
	if len(runes) > limit {
		return string(runes[:limit]) + "…"
	}
	return string(runes)
}

// Client owns transport, authentication, failover, and source index scoping.
type Client struct {
	addresses    []string
	index        string
	username     string
	password     string
	apiKey       string
	toolPolicy   operationPolicy
	ingestPolicy operationPolicy
	httpClient   *http.Client
}

// NewClient constructs a client from the same source configuration used by
// signal tailing. The configured index is the only index this client can read.
func NewClient(cfg config.AgentElasticsearchSourceConfig) (*Client, error) {
	if len(cfg.Addresses) == 0 {
		return nil, invalidConfig("at least one address is required")
	}
	if !validIndex(cfg.Index) {
		return nil, invalidConfig("index is required and must not contain URL delimiters")
	}
	if cfg.Password != "" && cfg.Username == "" {
		return nil, invalidConfig("password requires username")
	}
	hasCredentials := cfg.Username != "" || cfg.Password != "" || cfg.APIKey != ""
	if hasCredentials && cfg.InsecureSkipVerify {
		return nil, invalidConfig("credentials require verified HTTPS; insecure_skip_verify cannot be enabled")
	}
	addresses := make([]string, 0, len(cfg.Addresses))
	for _, address := range cfg.Addresses {
		parsed, err := url.Parse(strings.TrimSpace(address))
		if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return nil, invalidConfig("each address must be an absolute HTTP or HTTPS URL")
		}
		if parsed.User != nil {
			return nil, invalidConfig("URL userinfo is forbidden; use username/password or api_key fields")
		}
		if parsed.RawQuery != "" || parsed.Fragment != "" {
			return nil, invalidConfig("addresses must not contain a query string or fragment")
		}
		if hasCredentials && parsed.Scheme != "https" {
			return nil, invalidConfig("credentials require verified HTTPS")
		}
		if !validEndpointHost(parsed.Hostname(), cfg.AllowLoopback) {
			if isLoopbackHost(parsed.Hostname()) {
				return nil, invalidConfig("loopback addresses require allow_loopback: true")
			}
			return nil, invalidConfig("address host is not permitted")
		}
		addresses = append(addresses, strings.TrimRight(parsed.String(), "/"))
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	dialer := &net.Dialer{}
	transport.DialContext = guardedDialContext(dialer, cfg.AllowLoopback)
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: cfg.InsecureSkipVerify}
	return &Client{
		addresses: addresses, index: cfg.Index, username: cfg.Username, password: cfg.Password, apiKey: cfg.APIKey,
		toolPolicy:   operationPolicy{timeout: toolTimeout, maxBytes: toolMaxResponse},
		ingestPolicy: operationPolicy{timeout: ingestTimeout, maxBytes: ingestMaxResponse},
		httpClient:   &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return ErrBackend }},
	}, nil
}

func invalidConfig(reason string) error {
	return fmt.Errorf("%w: %s", ErrInvalidConfig, reason)
}

func isLoopbackHost(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	address := net.ParseIP(host)
	return address != nil && address.IsLoopback()
}

func validEndpointHost(host string, allowLoopback bool) bool {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if host == "" || host == "169.254.169.254" || host == "metadata" || host == "metadata.google.internal" || host == "metadata.google" || host == "instance-data" || host == "instance-data.ec2.internal" {
		return false
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return allowLoopback
	}
	address := net.ParseIP(host)
	if address == nil {
		return true
	}
	if address.IsLoopback() {
		return allowLoopback
	}
	return !address.IsUnspecified() && !address.IsLinkLocalUnicast() && !address.IsLinkLocalMulticast()
}

func guardedDialContext(dialer *net.Dialer, allowLoopback bool) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, ErrBackend
		}
		addresses := []net.IPAddr{{IP: net.ParseIP(host)}}
		if addresses[0].IP == nil {
			addresses, err = net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil || len(addresses) == 0 {
				return nil, ErrBackend
			}
		}
		for _, candidate := range addresses {
			if !validResolvedAddress(candidate.IP, allowLoopback) {
				return nil, ErrBackend
			}
		}
		var lastErr error
		for _, candidate := range addresses {
			connection, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(candidate.IP.String(), port))
			if dialErr == nil {
				return connection, nil
			}
			lastErr = dialErr
		}
		return nil, lastErr
	}
}

func validResolvedAddress(address net.IP, allowLoopback bool) bool {
	if address == nil || address.IsUnspecified() || address.IsMulticast() || address.IsLinkLocalUnicast() || address.IsLinkLocalMulticast() {
		return false
	}
	return !address.IsLoopback() || allowLoopback
}

func validIndex(index string) bool {
	index = strings.TrimSpace(index)
	return index != "" && !strings.ContainsAny(index, "/?#\\\r\n")
}

func (client *Client) readJSON(ctx context.Context, policy operationPolicy, method, suffix string, query url.Values, body []byte, output any) error {
	_, err := client.readJSONOn(ctx, policy, method, suffix, query, body, output, "")
	return err
}

func (client *Client) readJSONOn(ctx context.Context, policy operationPolicy, method, suffix string, query url.Values, body []byte, output any, pinnedAddress string) (string, error) {
	if client == nil || client.httpClient == nil || (method != http.MethodGet && method != http.MethodPost && (method != http.MethodDelete || suffix != "/_search/scroll")) || !strings.HasPrefix(suffix, "/_") || strings.ContainsAny(suffix, "?#\\\r\n") {
		return "", ErrInvalidArguments
	}
	destination := reflect.ValueOf(output)
	if destination.Kind() != reflect.Pointer || destination.IsNil() {
		return "", ErrInvalidArguments
	}
	if len(body) > maximumRequestBytes || policy.timeout <= 0 || policy.maxBytes <= 0 || policy.maxBytes > maximumResponseBytes {
		return "", ErrInvalidArguments
	}
	addresses := client.addresses
	if pinnedAddress != "" {
		addresses = nil
		for _, address := range client.addresses {
			if address == pinnedAddress {
				addresses = []string{address}
				break
			}
		}
		if len(addresses) == 0 {
			return "", ErrInvalidArguments
		}
	}
	operationCtx, cancelOperation := context.WithTimeout(ctx, policy.timeout)
	defer cancelOperation()
	var lastErr error
	for _, address := range addresses {
		if err := operationCtx.Err(); err != nil {
			return "", err
		}
		requestURL := address + client.scopedPath(suffix)
		if len(query) > 0 {
			requestURL += "?" + query.Encode()
		}
		request, err := http.NewRequestWithContext(operationCtx, method, requestURL, bytes.NewReader(body))
		if err != nil {
			lastErr = preferredError(lastErr, ErrBackend)
			continue
		}
		request.Header.Set("Accept", "application/json")
		if len(body) > 0 {
			request.Header.Set("Content-Type", "application/json")
		}
		client.applyAuth(request)
		response, err := client.httpClient.Do(request)
		if err != nil {
			if operationCtx.Err() != nil {
				return "", operationCtx.Err()
			} else {
				lastErr = preferredError(lastErr, ErrBackend)
			}
			continue
		}
		data, readErr := io.ReadAll(io.LimitReader(response.Body, policy.maxBytes+1))
		_ = response.Body.Close()
		if readErr != nil {
			if operationCtx.Err() != nil {
				return "", operationCtx.Err()
			}
			lastErr = preferredError(lastErr, ErrBackend)
			if suffix == "/_search" && query.Get("scroll") != "" {
				return "", lastErr
			}
			continue
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			lastErr = preferredError(lastErr, classifyStatus(response.StatusCode))
			continue
		}
		if int64(len(data)) > policy.maxBytes {
			lastErr = preferredError(lastErr, ErrResponseTooLarge)
			if suffix == "/_search" && query.Get("scroll") != "" {
				return "", lastErr
			}
			continue
		}
		decoded := reflect.New(destination.Elem().Type())
		if err := json.Unmarshal(data, decoded.Interface()); err != nil {
			lastErr = preferredError(lastErr, ErrBackend)
			if suffix == "/_search" && query.Get("scroll") != "" {
				return "", lastErr
			}
			continue
		}
		destination.Elem().Set(decoded.Elem())
		return address, nil
	}
	if lastErr == nil {
		lastErr = ErrBackend
	}
	return "", lastErr
}

func preferredError(current, candidate error) error {
	if errorPriority(candidate) > errorPriority(current) {
		return candidate
	}
	return current
}

func errorPriority(err error) int {
	switch {
	case errors.Is(err, ErrUnauthorized), errors.Is(err, ErrForbidden):
		return 4
	case errors.Is(err, ErrResponseTooLarge):
		return 3
	case errors.Is(err, ErrNotFound):
		return 2
	case err != nil:
		return 1
	default:
		return 0
	}
}

func (client *Client) scopedPath(suffix string) string {
	switch suffix {
	case "/_cat/indices", "/_cat/shards":
		return suffix + "/" + client.index
	case "/_search/scroll":
		return suffix
	default:
		return "/" + client.index + suffix
	}
}

func (client *Client) applyAuth(request *http.Request) {
	if client.apiKey != "" {
		request.Header.Set("Authorization", "ApiKey "+client.apiKey)
		return
	}
	if client.username != "" {
		token := base64.StdEncoding.EncodeToString([]byte(client.username + ":" + client.password))
		request.Header.Set("Authorization", "Basic "+token)
	}
}

func classifyStatus(status int) error {
	switch status {
	case http.StatusUnauthorized:
		return ErrUnauthorized
	case http.StatusForbidden:
		return ErrForbidden
	case http.StatusNotFound:
		return ErrNotFound
	default:
		return fmt.Errorf("%w: HTTP status %d", ErrBackend, status)
	}
}

// SearchJSON executes a bounded POST _search against the configured index.
// It exists for both the application service and the signal tail source.
func (client *Client) SearchJSON(ctx context.Context, body []byte, output any) error {
	return client.readJSON(ctx, client.toolPolicy, http.MethodPost, "/_search", nil, body, output)
}

// SearchIngestJSON executes a tail search with the historical 30-second
// operation timeout and a bounded response budget sized for structured pages.
func (client *Client) SearchIngestJSON(ctx context.Context, body []byte, output any) error {
	return client.SearchIngestJSONWithPreference(ctx, "", body, output)
}

// SearchIngestJSONWithPreference is SearchIngestJSON with an Elasticsearch
// `preference` routing value, so requests sharing it hit the same shard copies.
// An empty preference sends none.
func (client *Client) SearchIngestJSONWithPreference(ctx context.Context, preference string, body []byte, output any) error {
	var query url.Values
	if preference != "" {
		query = url.Values{"preference": {preference}}
	}
	return client.readJSON(ctx, client.ingestPolicy, http.MethodPost, "/_search", query, body, output)
}

// OpenIngestScroll starts an index-scoped snapshot and returns its route.
func (client *Client) OpenIngestScroll(ctx context.Context, body []byte, output any) (string, error) {
	return client.readJSONOn(ctx, client.ingestPolicy, http.MethodPost, "/_search", url.Values{"scroll": {"1m"}}, body, output, "")
}

// NextIngestScroll reads the next page from a scroll opened by this client.
func (client *Client) NextIngestScroll(ctx context.Context, address, scrollID string, output any) error {
	if address == "" || scrollID == "" || len(scrollID) > 8192 {
		return ErrInvalidArguments
	}
	body, err := json.Marshal(map[string]string{"scroll": "1m", "scroll_id": scrollID})
	if err != nil {
		return ErrInvalidArguments
	}
	_, err = client.readJSONOn(ctx, client.ingestPolicy, http.MethodPost, "/_search/scroll", nil, body, output, address)
	return err
}

// ClearIngestScroll releases a server-side snapshot even after Pull is canceled.
func (client *Client) ClearIngestScroll(ctx context.Context, address, scrollID string) error {
	if address == "" || scrollID == "" {
		return ErrInvalidArguments
	}
	body, err := json.Marshal(map[string][]string{"scroll_id": {scrollID}})
	if err != nil {
		return ErrInvalidArguments
	}
	var output struct{}
	_, err = client.readJSONOn(ctx, client.ingestPolicy, http.MethodDelete, "/_search/scroll", nil, body, &output, address)
	return err
}

// setTestBounds replaces transport bounds for package tests.
func (client *Client) setTestBounds(httpClient *http.Client, timeout time.Duration, maxBytes int64) {
	if httpClient != nil {
		client.httpClient = httpClient
	}
	if timeout > 0 {
		client.toolPolicy.timeout = timeout
	}
	if maxBytes > 0 && maxBytes <= maximumResponseBytes {
		client.toolPolicy.maxBytes = maxBytes
	}
}
