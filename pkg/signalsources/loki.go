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
	"strconv"
	"time"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
)

// LokiSource pulls log entries from Grafana Loki using the
// `query_range` HTTP endpoint with `direction=forward`. Loki returns
// entries grouped by stream (label set); this source flattens them into
// a single timestamp-sorted batch and tracks the maximum timestamp seen
// as the cursor for the next tick.
//
// Loki entry timestamps are nanoseconds since epoch encoded as a string.
type LokiSource struct {
	name   string
	cfg    config.AgentLokiSourceConfig
	client *http.Client
}

// NewLokiSource validates config and returns a ready source.
func NewLokiSource(name string, cfg config.AgentLokiSourceConfig) (*LokiSource, error) {
	if cfg.Address == "" {
		return nil, fmt.Errorf("loki source %q: address is required", name)
	}
	if cfg.Query == "" {
		return nil, fmt.Errorf("loki source %q: query is required", name)
	}
	if cfg.PageSize <= 0 {
		cfg.PageSize = 500
	}
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: cfg.InsecureSkipVerify},
	}
	return &LokiSource{
		name:   name,
		cfg:    cfg,
		client: &http.Client{Transport: tr, Timeout: 30 * time.Second},
	}, nil
}

func (s *LokiSource) Name() string { return "loki:" + s.name }

// Pull issues a `query_range` request with `start = since + 1ns` (Loki's
// `start` is inclusive) and `end = now`. Results are returned forward
// (oldest first) so the cursor at the end is the max timestamp seen.
//
// We do not paginate: Loki caps the result set at PageSize. If the cap
// is hit we still advance the cursor to the last entry's timestamp so
// the next tick continues from there. This is the standard pattern for
// streaming pulls.
func (s *LokiSource) Pull(ctx context.Context, since time.Time) ([]core.Signal, time.Time, error) {
	cursor := since
	startNs := since.UTC().UnixNano() + 1
	endNs := time.Now().UTC().UnixNano()
	if endNs <= startNs {
		return nil, cursor, nil
	}

	q := url.Values{}
	q.Set("query", s.cfg.Query)
	q.Set("start", strconv.FormatInt(startNs, 10))
	q.Set("end", strconv.FormatInt(endNs, 10))
	q.Set("limit", strconv.Itoa(s.cfg.PageSize))
	q.Set("direction", "forward")

	u := s.cfg.Address + "/loki/api/v1/query_range?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, cursor, err
	}
	req.Header.Set("Accept", "application/json")
	s.applyAuth(req)

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, cursor, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, cursor, err
	}
	if resp.StatusCode >= 400 {
		return nil, cursor, fmt.Errorf("loki %s: %d %s", u, resp.StatusCode, truncate(string(body), 256))
	}

	var out lokiQueryRangeResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, cursor, fmt.Errorf("decode loki response: %w", err)
	}

	var signals []core.Signal
	for _, stream := range out.Data.Result {
		sev := ""
		if s.cfg.SeverityField != "" {
			sev = stream.Stream[s.cfg.SeverityField]
		}
		fields := make(map[string]interface{}, len(s.cfg.ExtraLabels))
		for _, lbl := range s.cfg.ExtraLabels {
			if v, ok := stream.Stream[lbl]; ok {
				fields[lbl] = v
			}
		}
		for _, entry := range stream.Values {
			if len(entry) < 2 {
				continue
			}
			tsNs, perr := strconv.ParseInt(entry[0], 10, 64)
			if perr != nil {
				continue
			}
			ts := time.Unix(0, tsNs).UTC()
			if ts.After(cursor) {
				cursor = ts
			}
			// Copy labels into Raw so downstream consumers can keep stream
			// context without it bleeding into Fields by default.
			raw := make(map[string]interface{}, len(stream.Stream)+1)
			for k, v := range stream.Stream {
				raw[k] = v
			}
			raw["message"] = entry[1]
			signals = append(signals, core.Signal{
				Source:    s.Name(),
				Timestamp: ts,
				Severity:  sev,
				Message:   entry[1],
				Fields:    fields,
				Raw:       raw,
			})
		}
	}
	return signals, cursor, nil
}

func (s *LokiSource) applyAuth(req *http.Request) {
	if s.cfg.TenantID != "" {
		req.Header.Set("X-Scope-OrgID", s.cfg.TenantID)
	}
	if s.cfg.BearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+s.cfg.BearerToken)
		return
	}
	if s.cfg.Username != "" {
		token := base64.StdEncoding.EncodeToString(
			[]byte(s.cfg.Username + ":" + s.cfg.Password),
		)
		req.Header.Set("Authorization", "Basic "+token)
	}
}

// -----------------------------------------------------------------------------
// response shape
// -----------------------------------------------------------------------------

type lokiQueryRangeResponse struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string             `json:"resultType"`
		Result     []lokiStreamResult `json:"result"`
	} `json:"data"`
}

type lokiStreamResult struct {
	Stream map[string]string `json:"stream"`
	// Values is a list of [<unix_ns_str>, <line_str>] pairs.
	Values [][]string `json:"values"`
}
