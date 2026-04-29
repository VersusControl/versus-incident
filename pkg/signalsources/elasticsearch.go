// Package signalsources contains concrete SignalSource implementations.
//
// Each source implements pkg/core.SignalSource and must:
//
//   - Be cursor-aware: `since` defines the lower bound, the returned cursor
//     is the upper bound that should be passed back next tick.
//   - Be polite: respect AgentSourceConfig.Elasticsearch.PageSize (or its
//     equivalent) and never load arbitrarily many docs into memory.
//   - Be best-effort: a single failed tick must not crash the worker —
//     return the error and let the worker decide whether to retry.
package signalsources

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
)

// ElasticsearchSource pulls log documents from one or more Elasticsearch
// addresses using the `_search` API with a `range` filter on the configured
// time field. It uses sort-by-time + `search_after` for stable pagination.
//
// This intentionally avoids the official ES client to keep the dependency
// surface small. The set of features used (basic auth, API-key auth,
// `_search`, `range`, `query_string`, `sort`, `search_after`) is stable
// across ES 7.x and 8.x.
type ElasticsearchSource struct {
	name   string
	cfg    config.AgentElasticsearchSourceConfig
	client *http.Client
}

// NewElasticsearchSource validates config and returns a ready source.
func NewElasticsearchSource(name string, cfg config.AgentElasticsearchSourceConfig) (*ElasticsearchSource, error) {
	if len(cfg.Addresses) == 0 {
		return nil, fmt.Errorf("elasticsearch source %q: no addresses configured", name)
	}
	if cfg.Index == "" {
		return nil, fmt.Errorf("elasticsearch source %q: index is required", name)
	}
	if cfg.TimeField == "" {
		cfg.TimeField = "@timestamp"
	}
	if cfg.MessageField == "" {
		cfg.MessageField = "message"
	}
	if cfg.PageSize <= 0 {
		cfg.PageSize = 500
	}

	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: cfg.InsecureSkipVerify},
	}
	return &ElasticsearchSource{
		name:   name,
		cfg:    cfg,
		client: &http.Client{Transport: tr, Timeout: 30 * time.Second},
	}, nil
}

func (s *ElasticsearchSource) Name() string { return "elasticsearch:" + s.name }

// Pull issues a `_search` query with `range[time_field] > since` and walks
// pages with `search_after` until the page is short or we've collected
// enough docs. The returned cursor is the maximum timestamp seen so duplicate
// reads are skipped on the next tick.
func (s *ElasticsearchSource) Pull(ctx context.Context, since time.Time) ([]core.Signal, time.Time, error) {
	cursor := since
	var signals []core.Signal
	var searchAfter []interface{}

	// Cap total iterations so a misconfigured query can't loop forever.
	const maxPages = 20
	for page := 0; page < maxPages; page++ {
		body, err := s.buildQuery(since, searchAfter)
		if err != nil {
			return signals, cursor, err
		}
		resp, err := s.doSearch(ctx, body)
		if err != nil {
			return signals, cursor, err
		}
		hits := resp.Hits.Hits
		if len(hits) == 0 {
			break
		}
		for _, h := range hits {
			sig, ok := s.signalFromHit(h)
			if !ok {
				continue
			}
			if sig.Timestamp.After(cursor) {
				cursor = sig.Timestamp
			}
			signals = append(signals, sig)
		}
		if len(hits) < s.cfg.PageSize {
			break
		}
		searchAfter = hits[len(hits)-1].Sort
	}
	return signals, cursor, nil
}

// -----------------------------------------------------------------------------
// internals
// -----------------------------------------------------------------------------

type esSearchResponse struct {
	Hits struct {
		Hits []esHit `json:"hits"`
	} `json:"hits"`
}

type esHit struct {
	ID     string                 `json:"_id"`
	Source map[string]interface{} `json:"_source"`
	Sort   []interface{}          `json:"sort,omitempty"`
}

func (s *ElasticsearchSource) buildQuery(since time.Time, searchAfter []interface{}) ([]byte, error) {
	rangeFilter := map[string]interface{}{
		s.cfg.TimeField: map[string]interface{}{
			"gt":     since.UTC().Format(time.RFC3339Nano),
			"format": "strict_date_optional_time_nanos",
		},
	}

	must := []interface{}{
		map[string]interface{}{"range": rangeFilter},
	}
	if strings.TrimSpace(s.cfg.Query) != "" {
		must = append(must, map[string]interface{}{
			"query_string": map[string]interface{}{"query": s.cfg.Query},
		})
	}

	body := map[string]interface{}{
		"size": s.cfg.PageSize,
		"sort": []interface{}{
			map[string]interface{}{s.cfg.TimeField: map[string]interface{}{"order": "asc"}},
		},
		"query": map[string]interface{}{
			"bool": map[string]interface{}{"must": must},
		},
	}
	if len(searchAfter) > 0 {
		body["search_after"] = searchAfter
	}
	return json.Marshal(body)
}

func (s *ElasticsearchSource) doSearch(ctx context.Context, body []byte) (*esSearchResponse, error) {
	var lastErr error
	for _, addr := range s.cfg.Addresses {
		u := strings.TrimRight(addr, "/") + "/" + s.cfg.Index + "/_search"
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
		if err != nil {
			lastErr = err
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		s.applyAuth(req)

		resp, err := s.client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		data, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode >= 400 {
			lastErr = fmt.Errorf("elasticsearch %s: %d %s", u, resp.StatusCode, truncate(string(data), 256))
			continue
		}
		var out esSearchResponse
		if err := json.Unmarshal(data, &out); err != nil {
			lastErr = fmt.Errorf("decode elasticsearch response: %w", err)
			continue
		}
		return &out, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no elasticsearch addresses configured")
	}
	return nil, lastErr
}

func (s *ElasticsearchSource) applyAuth(req *http.Request) {
	if s.cfg.APIKey != "" {
		req.Header.Set("Authorization", "ApiKey "+s.cfg.APIKey)
		return
	}
	if s.cfg.Username != "" {
		token := base64.StdEncoding.EncodeToString(
			[]byte(s.cfg.Username + ":" + s.cfg.Password),
		)
		req.Header.Set("Authorization", "Basic "+token)
	}
}

// signalFromHit maps an _source document to a core.Signal. It returns false
// when the document is missing the configured time field (we cannot cursor
// past it, so it's safer to skip).
func (s *ElasticsearchSource) signalFromHit(h esHit) (core.Signal, bool) {
	ts, ok := extractTime(h.Source, s.cfg.TimeField)
	if !ok {
		return core.Signal{}, false
	}

	msg := stringField(h.Source, s.cfg.MessageField)
	sev := stringField(h.Source, s.cfg.SeverityField)

	fields := make(map[string]interface{})
	for _, f := range s.cfg.ExtraFields {
		if v, ok := lookupField(h.Source, f); ok {
			fields[f] = v
		}
	}
	return core.Signal{
		Source:    s.Name(),
		Timestamp: ts,
		Severity:  sev,
		Message:   msg,
		Fields:    fields,
		Raw:       h.Source,
	}, true
}

// -----------------------------------------------------------------------------
// small field helpers (dotted-path lookup, time parsing, truncation)
// -----------------------------------------------------------------------------

func stringField(src map[string]interface{}, path string) string {
	if path == "" {
		return ""
	}
	v, ok := lookupField(src, path)
	if !ok {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	default:
		// Best-effort stringification for nested objects.
		b, err := json.Marshal(t)
		if err != nil {
			return ""
		}
		return string(b)
	}
}

// lookupField walks a dotted path ("error.stack_trace") through nested maps.
// Elasticsearch flattens dotted field names automatically, so we try the
// flat name first and fall back to a recursive walk.
func lookupField(src map[string]interface{}, path string) (interface{}, bool) {
	if v, ok := src[path]; ok {
		return v, true
	}
	parts := strings.Split(path, ".")
	var cur interface{} = src
	for _, p := range parts {
		m, ok := cur.(map[string]interface{})
		if !ok {
			return nil, false
		}
		cur, ok = m[p]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

func extractTime(src map[string]interface{}, field string) (time.Time, bool) {
	v, ok := lookupField(src, field)
	if !ok {
		return time.Time{}, false
	}
	switch t := v.(type) {
	case string:
		// Try a couple of common ES formats.
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.000Z"} {
			if ts, err := time.Parse(layout, t); err == nil {
				return ts.UTC(), true
			}
		}
	case float64:
		// epoch millis
		return time.UnixMilli(int64(t)).UTC(), true
	}
	return time.Time{}, false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
