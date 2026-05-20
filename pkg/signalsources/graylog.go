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

// GraylogSource pulls log messages from Graylog using the legacy
// `search/universal/absolute` REST endpoint. That endpoint is
// synchronous (no async query lifecycle), accepts a free-form Graylog
// query string, and returns messages sorted by timestamp — exactly the
// shape the agent worker wants.
//
// Cursor contract: the source asks for `from = since` (Graylog `from`
// is INCLUSIVE) and filters the response client-side to messages with
// timestamp > since. The cursor returned is the maximum timestamp seen.
type GraylogSource struct {
	name   string
	cfg    config.AgentGraylogSourceConfig
	client *http.Client
}

// NewGraylogSource validates the config and constructs a ready source.
func NewGraylogSource(name string, cfg config.AgentGraylogSourceConfig) (*GraylogSource, error) {
	if cfg.Address == "" {
		return nil, fmt.Errorf("graylog source %q: address is required", name)
	}
	if cfg.Query == "" {
		// Graylog requires a non-empty query string; "*" matches all.
		cfg.Query = "*"
	}
	if cfg.PageSize <= 0 {
		cfg.PageSize = 500
	}
	if cfg.MessageField == "" {
		cfg.MessageField = "message"
	}
	if cfg.SeverityField == "" {
		cfg.SeverityField = "level"
	}
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: cfg.InsecureSkipVerify},
	}
	return &GraylogSource{
		name:   name,
		cfg:    cfg,
		client: &http.Client{Transport: tr, Timeout: 30 * time.Second},
	}, nil
}

func (s *GraylogSource) Name() string { return "graylog:" + s.name }

// Pull issues a `search/universal/absolute` request between (since, now)
// and returns every message strictly newer than `since`. The cursor is
// the max message timestamp seen this tick — when zero messages match,
// the cursor is unchanged so the next tick re-asks for the same window.
func (s *GraylogSource) Pull(ctx context.Context, since time.Time) ([]core.Signal, time.Time, error) {
	cursor := since
	from := since.UTC()
	if from.IsZero() {
		// Cold start: pull the last 5 minutes so the first tick has
		// something to look at instead of replaying the full retention.
		from = time.Now().UTC().Add(-5 * time.Minute)
	}
	to := time.Now().UTC()
	if !to.After(from) {
		return nil, cursor, nil
	}

	q := url.Values{}
	q.Set("query", s.cfg.Query)
	// Graylog accepts RFC3339 with ms precision. Use UTC explicitly so
	// we never accidentally send a non-Zulu offset.
	q.Set("from", from.Format("2006-01-02T15:04:05.000Z"))
	q.Set("to", to.Format("2006-01-02T15:04:05.000Z"))
	q.Set("limit", strconv.Itoa(s.cfg.PageSize))
	q.Set("sort", "timestamp:asc")
	if s.cfg.StreamID != "" {
		q.Set("filter", "streams:"+s.cfg.StreamID)
	}
	if len(s.cfg.Fields) > 0 {
		// Graylog returns this comma-separated subset (plus the
		// always-present "message" + "timestamp" fields).
		fields := s.cfg.Fields[0]
		for _, f := range s.cfg.Fields[1:] {
			fields += "," + f
		}
		q.Set("fields", fields)
	}

	u := s.cfg.Address + "/api/search/universal/absolute?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, cursor, err
	}
	req.Header.Set("Accept", "application/json")
	// Graylog requires this header on every API call (CSRF defense).
	req.Header.Set("X-Requested-By", "versus-incident")
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
		return nil, cursor, fmt.Errorf("graylog %s: %d %s", u, resp.StatusCode, truncate(string(body), 256))
	}

	var out graylogSearchResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, cursor, fmt.Errorf("decode graylog response: %w", err)
	}

	signals := make([]core.Signal, 0, len(out.Messages))
	for _, m := range out.Messages {
		ts := parseGraylogTimestamp(m.Message["timestamp"])
		if ts.IsZero() || !ts.After(since) {
			// Graylog `from` is inclusive; drop anything we already saw.
			continue
		}
		if ts.After(cursor) {
			cursor = ts
		}

		msgText := stringField(m.Message, s.cfg.MessageField)
		sev := stringField(m.Message, s.cfg.SeverityField)

		fields := make(map[string]interface{})
		for _, f := range s.cfg.ExtraFields {
			if v, ok := m.Message[f]; ok {
				fields[f] = v
			}
		}

		signals = append(signals, core.Signal{
			Source:    s.Name(),
			Timestamp: ts,
			Severity:  sev,
			Message:   msgText,
			Fields:    fields,
			Raw:       m.Message,
		})
	}
	return signals, cursor, nil
}

// applyAuth wires up Graylog auth in priority order:
//  1. APIToken — sent as Basic auth with username="<token>" and password="token".
//  2. Username + Password — plain HTTP Basic.
func (s *GraylogSource) applyAuth(req *http.Request) {
	if s.cfg.APIToken != "" {
		token := base64.StdEncoding.EncodeToString(
			[]byte(s.cfg.APIToken + ":token"),
		)
		req.Header.Set("Authorization", "Basic "+token)
		return
	}
	if s.cfg.Username != "" {
		token := base64.StdEncoding.EncodeToString(
			[]byte(s.cfg.Username + ":" + s.cfg.Password),
		)
		req.Header.Set("Authorization", "Basic "+token)
	}
}

// parseGraylogTimestamp accepts the RFC3339-ish strings Graylog returns
// ("2026-05-20T10:00:00.000Z"). Returns zero time on failure.
func parseGraylogTimestamp(v interface{}) time.Time {
	s, ok := v.(string)
	if !ok || s == "" {
		return time.Time{}
	}
	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05.000Z",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

// stringField is shared with the elasticsearch source — see elasticsearch.go.

// -----------------------------------------------------------------------------
// response shape
// -----------------------------------------------------------------------------

type graylogSearchResponse struct {
	Messages     []graylogSearchHit `json:"messages"`
	TotalResults int                `json:"total_results"`
}

type graylogSearchHit struct {
	Index   string                 `json:"index"`
	Message map[string]interface{} `json:"message"`
}
