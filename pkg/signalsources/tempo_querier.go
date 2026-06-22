package signalsources

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// -----------------------------------------------------------------------------
// TempoQuerier — a read-only client over Tempo's HTTP search API.
//
// Shared OSS infrastructure: the analyze `query_traces` tool consumes it (via a
// bridge in pkg/agent), and the enterprise trace data source reuses the exact
// same client. It only issues GET search requests — there is no write surface.
//
// NOTE: only the Tempo backend is implemented in this client. A Jaeger reader
// variant is a separate, deferred slice; consumers that need it switch on their
// own config.
// -----------------------------------------------------------------------------

// TraceSummary is one trace returned by a search, flattened to the fields
// the analyze agent reasons over.
type TraceSummary struct {
	TraceID    string
	Service    string
	Operation  string
	DurationMs float64
	Start      time.Time
	Error      bool
}

// TempoQuerier issues TraceQL searches against a Tempo HTTP endpoint.
type TempoQuerier struct {
	address string
	auth    PrometheusAuth // same shape (bearer-or-basic); reused intentionally
	client  *http.Client
}

// NewTempoQuerier validates the address and returns a ready querier.
func NewTempoQuerier(address string, auth PrometheusAuth, insecureSkipVerify bool) (*TempoQuerier, error) {
	if address == "" {
		return nil, fmt.Errorf("tempo: address is required")
	}
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: insecureSkipVerify},
	}
	return &TempoQuerier{
		address: address,
		auth:    auth,
		client:  &http.Client{Transport: tr, Timeout: 30 * time.Second},
	}, nil
}

// Search runs a TraceQL `query` over [start, end] and returns up to
// `limit` trace summaries (newest first).
func (q *TempoQuerier) Search(ctx context.Context, query string, start, end time.Time, limit int) ([]TraceSummary, error) {
	if limit <= 0 {
		limit = 20
	}
	v := url.Values{}
	if query != "" {
		v.Set("q", query)
	}
	v.Set("start", strconv.FormatInt(start.UTC().Unix(), 10))
	v.Set("end", strconv.FormatInt(end.UTC().Unix(), 10))
	v.Set("limit", strconv.Itoa(limit))

	u := q.address + "/api/search?" + v.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	q.applyAuth(req)

	resp, err := q.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("tempo %s: %d %s", u, resp.StatusCode, truncate(string(body), 256))
	}

	var out tempoSearchResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode tempo response: %w", err)
	}

	summaries := make([]TraceSummary, 0, len(out.Traces))
	for _, tr := range out.Traces {
		summaries = append(summaries, tr.toSummary())
	}
	sort.SliceStable(summaries, func(i, j int) bool {
		return summaries[i].Start.After(summaries[j].Start)
	})
	if len(summaries) > limit {
		summaries = summaries[:limit]
	}
	return summaries, nil
}

func (q *TempoQuerier) applyAuth(req *http.Request) {
	if q.auth.BearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+q.auth.BearerToken)
		return
	}
	if q.auth.Username != "" {
		token := base64.StdEncoding.EncodeToString([]byte(q.auth.Username + ":" + q.auth.Password))
		req.Header.Set("Authorization", "Basic "+token)
	}
}

// -----------------------------------------------------------------------------
// Discovery reads (GET-only).
//
// These let a caller LEARN the shape of a Tempo instance — what tags exist and
// what values a tag takes — so a trace brain can discover (service, operation)
// dimensions without being handed a query. Both are GETs against Tempo's search
// tag API; neither accepts or issues a write.
// -----------------------------------------------------------------------------

// Tags returns the searchable tag names advertised by Tempo. It prefers the v2
// scoped endpoint (/api/v2/search/tags) and falls back to the v1 flat list
// (/api/search/tags) for older Tempo. The result is de-duplicated and sorted.
func (q *TempoQuerier) Tags(ctx context.Context) ([]string, error) {
	// v2 scoped tags: { "scopes": [ { "name": "...", "tags": [...] }, ... ] }.
	if body, err := q.get(ctx, q.address+"/api/v2/search/tags"); err == nil {
		var v2 struct {
			Scopes []struct {
				Name string   `json:"name"`
				Tags []string `json:"tags"`
			} `json:"scopes"`
		}
		if json.Unmarshal(body, &v2) == nil && len(v2.Scopes) > 0 {
			names := make([]string, 0, 16)
			for _, s := range v2.Scopes {
				names = append(names, s.Tags...)
			}
			if len(names) > 0 {
				return dedupeSortStrings(names), nil
			}
		}
	}
	// v1 flat tags fallback: { "tagNames": [...] }.
	body, err := q.get(ctx, q.address+"/api/search/tags")
	if err != nil {
		return nil, err
	}
	var v1 struct {
		TagNames []string `json:"tagNames"`
	}
	if err := json.Unmarshal(body, &v1); err != nil {
		return nil, fmt.Errorf("decode tempo tags: %w", err)
	}
	return dedupeSortStrings(v1.TagNames), nil
}

// TagValues returns the observed values of one tag
// (GET /api/search/tag/<tag>/values). The values are de-duplicated and sorted.
func (q *TempoQuerier) TagValues(ctx context.Context, tag string) ([]string, error) {
	if tag == "" {
		return nil, fmt.Errorf("tempo: tag name is required")
	}
	body, err := q.get(ctx, q.address+"/api/search/tag/"+url.PathEscape(tag)+"/values")
	if err != nil {
		return nil, err
	}
	var out struct {
		TagValues []string `json:"tagValues"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode tempo tag values: %w", err)
	}
	return dedupeSortStrings(out.TagValues), nil
}

// get issues a GET with auth and returns the raw body, failing on non-2xx.
func (q *TempoQuerier) get(ctx context.Context, u string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	q.applyAuth(req)

	resp, err := q.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("tempo %s: %d %s", u, resp.StatusCode, truncate(string(body), 256))
	}
	return body, nil
}

// dedupeSortStrings drops empty entries, de-duplicates and sorts a slice.
func dedupeSortStrings(in []string) []string {
	set := make(map[string]struct{}, len(in))
	for _, s := range in {
		if s != "" {
			set[s] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// -----------------------------------------------------------------------------
// response shape (Tempo /api/search)
// -----------------------------------------------------------------------------

type tempoSearchResponse struct {
	Traces []tempoTrace `json:"traces"`
}

type tempoTrace struct {
	TraceID           string         `json:"traceID"`
	RootServiceName   string         `json:"rootServiceName"`
	RootTraceName     string         `json:"rootTraceName"`
	StartTimeUnixNano string         `json:"startTimeUnixNano"`
	DurationMs        float64        `json:"durationMs"`
	SpanSets          []tempoSpanSet `json:"spanSets"`
	SpanSet           *tempoSpanSet  `json:"spanSet"`
}

type tempoSpanSet struct {
	Spans []tempoSpan `json:"spans"`
}

type tempoSpan struct {
	Attributes []tempoAttr `json:"attributes"`
}

type tempoAttr struct {
	Key   string         `json:"key"`
	Value tempoAttrValue `json:"value"`
}

type tempoAttrValue struct {
	StringValue string `json:"stringValue"`
	BoolValue   bool   `json:"boolValue"`
}

func (t tempoTrace) toSummary() TraceSummary {
	var start time.Time
	if t.StartTimeUnixNano != "" {
		if ns, err := strconv.ParseInt(t.StartTimeUnixNano, 10, 64); err == nil {
			start = time.Unix(0, ns).UTC()
		}
	}
	return TraceSummary{
		TraceID:    t.TraceID,
		Service:    t.RootServiceName,
		Operation:  t.RootTraceName,
		DurationMs: t.DurationMs,
		Start:      start,
		Error:      t.hasError(),
	}
}

// hasError scans the matched span attributes for a status/error marker so
// the summary can flag error traces. Best-effort: a backend that does not
// return spanSet attributes simply yields Error=false.
func (t tempoTrace) hasError() bool {
	sets := t.SpanSets
	if t.SpanSet != nil {
		sets = append(sets, *t.SpanSet)
	}
	for _, ss := range sets {
		for _, sp := range ss.Spans {
			for _, a := range sp.Attributes {
				key := strings.ToLower(a.Key)
				switch key {
				case "error":
					if a.Value.BoolValue || strings.EqualFold(a.Value.StringValue, "true") {
						return true
					}
				case "status", "status.code", "otel.status_code":
					if strings.EqualFold(a.Value.StringValue, "error") ||
						strings.EqualFold(a.Value.StringValue, "status_code_error") {
						return true
					}
				}
			}
		}
	}
	return false
}
