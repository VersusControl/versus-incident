package signalsources

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// -----------------------------------------------------------------------------
// SigNozQuerier — a read-only client over SigNoz's v5 query API.
//
// SigNoz answers logs, traces and metrics from ONE endpoint, so this client is
// shared infrastructure: the OSS logs source consumes it today and the metric
// and trace sources consume the same surface later. It issues exactly one kind
// of request, a POST to SigNozQueryRangePath, and there is no write surface —
// the path is a single pinned literal so no caller (present or future) can
// steer it at another endpoint.
//
// POSTing to read is deliberate and has precedent in this package: the query
// lives in a JSON body, exactly as SplunkSource and ElasticsearchSource already
// do for their backends.
//
// Auth is the `SIGNOZ-API-KEY` header — identical on Cloud and self-hosted, and
// never a query parameter, so request URLs are safe to put in error strings by
// construction.
// -----------------------------------------------------------------------------

// SigNozQueryRangePath is the ONLY path this client may ever request. It is a
// literal, not a template: there is nothing to interpolate and therefore
// nothing to steer.
const SigNozQueryRangePath = "/api/v5/query_range"

// Signal names accepted by `spec.signal` and request types accepted by
// `requestType` in a v5 query body.
const (
	SigNozSignalLogs    = "logs"
	SigNozSignalTraces  = "traces"
	SigNozSignalMetrics = "metrics"

	SigNozRequestTypeRaw        = "raw"
	SigNozRequestTypeTimeSeries = "time_series"
	SigNozRequestTypeScalar     = "scalar"
)

// sigNozMaxBodyBytes caps how much of a response we will read into memory. A
// page is bounded (page_size ≤ 1000 rows), so a body larger than this is either
// a misconfigured query or a hostile/broken endpoint; either way we refuse it
// rather than letting one tick exhaust the worker's memory.
const sigNozMaxBodyBytes = 16 << 20 // 16 MiB

// sigNozMaxAttempts bounds how many times one request is issued, including the
// first. Retries cover transient rejection only (429 / 5xx / transport error);
// a 4xx other than 429 is a permanent request defect and is never retried.
const sigNozMaxAttempts = 3

// SigNozOrderBy is one ordering term of a builder query. SigNoz requires a
// tiebreak key alongside `timestamp` for a stable order — see SigNozSource.
type SigNozOrderBy struct {
	Key       string // attribute name, e.g. "timestamp" or "id"
	Direction string // "asc" or "desc"
}

// SigNozAggregation is one aggregation term of a builder query. For the
// `metrics` signal SigNoz carries the metric NAME here and nowhere else, so a
// metrics query without an aggregation has no subject at all — hence the
// constructor rejects it rather than letting SigNoz answer an opaque 400.
type SigNozAggregation struct {
	// MetricName is the series to read, e.g. "http_server_duration". Required.
	MetricName string
	// Temporality is the metric's OTLP temporality ("delta", "cumulative" or
	// "unspecified"). Empty lets SigNoz infer it from the metric's metadata.
	Temporality string
	// TimeAggregation reduces samples within one step ("avg", "sum", "min",
	// "max", "count", "rate", "increase", ...). Empty lets SigNoz default.
	TimeAggregation string
	// SpaceAggregation reduces series across the group-by dimensions ("avg",
	// "sum", "min", "max", "p90", "p95", "p99", ...). Empty lets SigNoz default.
	SpaceAggregation string
}

// SigNozSelectField names one column a `raw` read should return. SigNoz answers
// a raw query with a fixed default column set and returns anything else — a
// span's `has_error` flag, an OTLP attribute — only when it is selected here.
//
// Selecting REPLACES that default set rather than extending it, so a caller
// must name every column it reads, not just the extra one. Naming a column the
// backend does not have fails the whole query with a 400.
type SigNozSelectField struct {
	// Name is the field to return, e.g. "has_error" or "service.name". Required.
	Name string
	// FieldContext disambiguates where the name lives ("resource", "attribute",
	// "span", "log", ...). Empty lets SigNoz resolve it.
	FieldContext string
	// FieldDataType is the field's type ("string", "bool", "int64", ...). Empty
	// lets SigNoz resolve it.
	FieldDataType string
}

// SigNozBuilderQuery is one `builder_query` inside a composite query.
type SigNozBuilderQuery struct {
	// Name labels the query in the response ("A", "B", ...).
	Name string
	// Signal is one of SigNozSignalLogs / Traces / Metrics.
	Signal string
	// Filter is a v5 filter expression. Empty omits the filter entirely,
	// matching everything in the window.
	Filter string
	Order  []SigNozOrderBy
	Offset int
	Limit  int
	// Aggregations is required for the `metrics` signal and unused by the raw
	// logs/traces reads. Omitted from the body entirely when empty — SigNoz
	// validates the envelope and an empty array is not the same as absent.
	Aggregations []SigNozAggregation
	// StepInterval is the resolution of a time-series read. The wire format is
	// whole SECONDS; a value below one second, or zero, omits the key.
	StepInterval time.Duration
	// SelectFields extends the default column set of a `raw` read. Omitted from
	// the body entirely when empty, which leaves SigNoz's defaults in place.
	SelectFields []SigNozSelectField
}

// SigNozQueryRangeRequest is one call to the v5 query endpoint. Start and End
// are wall-clock times; the wire format is epoch MILLISECONDS.
type SigNozQueryRangeRequest struct {
	Start       time.Time
	End         time.Time
	RequestType string
	Queries     []SigNozBuilderQuery
}

// SigNozRawRow is one record of a `raw` result. `timestamp` is omitted by
// SigNoz when zero, so consumers must be prepared to read the timestamp out of
// Data instead.
type SigNozRawRow struct {
	Timestamp time.Time              `json:"timestamp"`
	Data      map[string]interface{} `json:"data"`
}

// SigNozRawResult is the `raw` result for one named query.
type SigNozRawResult struct {
	QueryName string `json:"queryName"`
	// NextCursor is returned by SigNoz but not used for tailing: offset+limit
	// is the documented pagination for builder queries.
	NextCursor string          `json:"nextCursor"`
	Rows       []*SigNozRawRow `json:"rows"`
}

// SigNozQuerier issues v5 query_range reads against one SigNoz instance.
type SigNozQuerier struct {
	address string
	apiKey  string
	client  *http.Client

	// retryBase is the first backoff interval; each further attempt doubles it.
	// Overridable in tests so retry behaviour can be asserted without sleeping.
	retryBase time.Duration
	// sleep is the delay used between attempts; nil ⇒ time.Sleep.
	sleep func(time.Duration)
}

// NewSigNozQuerier validates the endpoint and returns a ready client.
func NewSigNozQuerier(address, apiKey string, insecureSkipVerify bool) (*SigNozQuerier, error) {
	address = strings.TrimRight(strings.TrimSpace(address), "/")
	if address == "" {
		return nil, fmt.Errorf("signoz: address is required")
	}
	u, err := url.Parse(address)
	if err != nil {
		return nil, fmt.Errorf("signoz: address %q is not a valid URL", address)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("signoz: address %q must use http or https", address)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("signoz: address %q is missing a host", address)
	}
	if strings.TrimSpace(apiKey) == "" {
		return nil, fmt.Errorf("signoz: api_key is required")
	}

	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: insecureSkipVerify},
	}
	return &SigNozQuerier{
		address:   address,
		apiKey:    apiKey,
		client:    &http.Client{Transport: tr, Timeout: 30 * time.Second},
		retryBase: 250 * time.Millisecond,
	}, nil
}

// Endpoint is the single URL this client requests. Exposed so callers (and
// tests) can assert the path allowlist without reaching into the struct.
func (q *SigNozQuerier) Endpoint() string { return q.address + SigNozQueryRangePath }

// QueryRange issues one v5 query and returns the validated response body. It is
// the general entry point: `time_series` and `scalar` request types decode from
// the same body, so a metric or trace consumer can build on this without the
// client growing per-signal knowledge.
func (q *SigNozQuerier) QueryRange(ctx context.Context, req SigNozQueryRangeRequest) ([]byte, error) {
	body, err := buildSigNozQueryBody(req)
	if err != nil {
		return nil, err
	}
	return q.post(ctx, body)
}

// QueryRangeRaw issues a `raw` query and decodes the per-query row sets.
//
// Decoding is deliberately forgiving about the envelope: SigNoz wraps the
// query response in `{"status":..,"data":..}` and the payload itself carries a
// `data.results` array, so both nestings are accepted. A body that matches
// neither yields an error, never a panic — a broken or hostile endpoint must
// not be able to wedge the worker.
func (q *SigNozQuerier) QueryRangeRaw(ctx context.Context, req SigNozQueryRangeRequest) ([]SigNozRawResult, error) {
	if req.RequestType == "" {
		req.RequestType = SigNozRequestTypeRaw
	}
	body, err := q.QueryRange(ctx, req)
	if err != nil {
		return nil, err
	}
	return decodeSigNozRawResults(body)
}

// post issues the single allowed request with a bounded retry.
func (q *SigNozQuerier) post(ctx context.Context, body []byte) ([]byte, error) {
	u := q.Endpoint()
	var lastErr error

	for attempt := 0; attempt < sigNozMaxAttempts; attempt++ {
		if attempt > 0 {
			if err := q.backoff(ctx, attempt); err != nil {
				return nil, err
			}
		}

		// A fresh reader per attempt: the previous attempt consumed the last one.
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		req.Header.Set("SIGNOZ-API-KEY", q.apiKey)

		resp, err := q.client.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("signoz %s: %w", u, err)
			continue
		}
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, sigNozMaxBodyBytes))
		_ = resp.Body.Close()
		if readErr != nil {
			lastErr = fmt.Errorf("signoz %s: read response: %w", u, readErr)
			continue
		}

		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("signoz %s: %d %q", u, resp.StatusCode, truncate(string(data), 256))
			continue
		}
		if resp.StatusCode >= 400 {
			// Permanent: a bad filter expression or a rejected key. Retrying
			// only multiplies the damage.
			return nil, fmt.Errorf("signoz %s: %d %q", u, resp.StatusCode, truncate(string(data), 256))
		}
		return data, nil
	}
	return nil, lastErr
}

// backoff waits before the next attempt, aborting early if the context ends.
func (q *SigNozQuerier) backoff(ctx context.Context, attempt int) error {
	base := q.retryBase
	if base <= 0 {
		base = 250 * time.Millisecond
	}
	d := base << (attempt - 1)
	if q.sleep != nil {
		q.sleep(d)
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// validateSigNozBuilderQuery rejects the query defects we can see locally, so
// they surface as our error rather than an opaque 400 a round trip later.
func validateSigNozBuilderQuery(bq SigNozBuilderQuery) error {
	if bq.Signal == SigNozSignalMetrics && len(bq.Aggregations) == 0 {
		return fmt.Errorf("signoz: query %q on the metrics signal needs at least one aggregation (the metric name lives there)", bq.Name)
	}
	for _, a := range bq.Aggregations {
		if strings.TrimSpace(a.MetricName) == "" {
			return fmt.Errorf("signoz: query %q has an aggregation with no metricName", bq.Name)
		}
	}
	for _, f := range bq.SelectFields {
		if strings.TrimSpace(f.Name) == "" {
			return fmt.Errorf("signoz: query %q has a selectField with no name", bq.Name)
		}
	}
	return nil
}

// buildSigNozQueryBody renders the v5 request body. `start`/`end` are epoch
// MILLISECONDS — not seconds, not nanoseconds.
func buildSigNozQueryBody(req SigNozQueryRangeRequest) ([]byte, error) {
	if len(req.Queries) == 0 {
		return nil, fmt.Errorf("signoz: query_range needs at least one query")
	}
	queries := make([]interface{}, 0, len(req.Queries))
	for _, bq := range req.Queries {
		if err := validateSigNozBuilderQuery(bq); err != nil {
			return nil, err
		}
		spec := map[string]interface{}{
			"name":   bq.Name,
			"signal": bq.Signal,
			"offset": bq.Offset,
			"limit":  bq.Limit,
		}
		if strings.TrimSpace(bq.Filter) != "" {
			spec["filter"] = map[string]interface{}{"expression": bq.Filter}
		}
		if len(bq.Order) > 0 {
			order := make([]interface{}, 0, len(bq.Order))
			for _, o := range bq.Order {
				order = append(order, map[string]interface{}{
					"key":       map[string]interface{}{"name": o.Key},
					"direction": o.Direction,
				})
			}
			spec["order"] = order
		}
		if len(bq.Aggregations) > 0 {
			aggs := make([]interface{}, 0, len(bq.Aggregations))
			for _, a := range bq.Aggregations {
				agg := map[string]interface{}{"metricName": a.MetricName}
				// Each optional key is omitted rather than sent empty: SigNoz
				// infers temporality and defaults the reducers, but rejects a
				// blank string for them.
				if strings.TrimSpace(a.Temporality) != "" {
					agg["temporality"] = a.Temporality
				}
				if strings.TrimSpace(a.TimeAggregation) != "" {
					agg["timeAggregation"] = a.TimeAggregation
				}
				if strings.TrimSpace(a.SpaceAggregation) != "" {
					agg["spaceAggregation"] = a.SpaceAggregation
				}
				aggs = append(aggs, agg)
			}
			spec["aggregations"] = aggs
		}
		if secs := int64(bq.StepInterval / time.Second); secs > 0 {
			spec["stepInterval"] = secs
		}
		if len(bq.SelectFields) > 0 {
			fields := make([]interface{}, 0, len(bq.SelectFields))
			for _, f := range bq.SelectFields {
				field := map[string]interface{}{"name": f.Name}
				// Same rule as the aggregation keys: SigNoz resolves an absent
				// context or data type, but rejects a blank string for them.
				if strings.TrimSpace(f.FieldContext) != "" {
					field["fieldContext"] = f.FieldContext
				}
				if strings.TrimSpace(f.FieldDataType) != "" {
					field["fieldDataType"] = f.FieldDataType
				}
				fields = append(fields, field)
			}
			spec["selectFields"] = fields
		}
		queries = append(queries, map[string]interface{}{
			"type": "builder_query",
			"spec": spec,
		})
	}

	return json.Marshal(map[string]interface{}{
		"start":       req.Start.UTC().UnixMilli(),
		"end":         req.End.UTC().UnixMilli(),
		"requestType": req.RequestType,
		"compositeQuery": map[string]interface{}{
			"queries": queries,
		},
	})
}

// sigNozRawEnvelope tolerates both the wrapped (`data.data.results`) and
// unwrapped (`data.results`) shapes of the query response.
type sigNozRawEnvelope struct {
	Data struct {
		Data struct {
			Results []SigNozRawResult `json:"results"`
		} `json:"data"`
		Results []SigNozRawResult `json:"results"`
	} `json:"data"`
}

func decodeSigNozRawResults(body []byte) ([]SigNozRawResult, error) {
	var env sigNozRawEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("decode signoz response: %w", err)
	}
	if len(env.Data.Data.Results) > 0 {
		return env.Data.Data.Results, nil
	}
	return env.Data.Results, nil
}
