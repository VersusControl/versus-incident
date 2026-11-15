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
	"strings"
	"time"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
)

// SplunkSource pulls events from Splunk Enterprise / Splunk Cloud using
// the synchronous `search/v2/jobs/export` REST endpoint. Export is
// preferred over `oneshot` because it streams results sorted by `_time`
// without holding state on the indexer — exactly the cursor-friendly
// pattern the agent worker wants.
//
// Cursor contract: `earliest_time` is sent as sub-second epoch
// (since.Unix() + fractional). `latest_time` is `now`. Splunk's
// `earliest_time` is INCLUSIVE — we drop any returned events whose
// `_time` is not strictly after `since` to honor the >`since`
// requirement. The returned cursor is the max `_time` observed.
//
// `_time` is requested as epoch (`output_time_format`): left to its default,
// Splunk renders it in the server's display format ("2026-11-15 08:40:23.294
// GMT"), which is not RFC 3339, and every row would be dropped as untimed.
type SplunkSource struct {
	name   string
	cfg    config.AgentSplunkSourceConfig
	client *http.Client
}

// NewSplunkSource validates the config and constructs a ready source.
func NewSplunkSource(name string, cfg config.AgentSplunkSourceConfig) (*SplunkSource, error) {
	if cfg.Address == "" {
		return nil, fmt.Errorf("splunk source %q: address is required", name)
	}
	if cfg.Search == "" {
		return nil, fmt.Errorf("splunk source %q: search is required", name)
	}
	if cfg.PageSize <= 0 {
		cfg.PageSize = 500
	}
	if cfg.MessageField == "" {
		cfg.MessageField = "_raw"
	}
	if cfg.TimeField == "" {
		cfg.TimeField = "_time"
	}
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: cfg.InsecureSkipVerify},
	}
	return &SplunkSource{
		name:   name,
		cfg:    cfg,
		client: &http.Client{Transport: tr, Timeout: 30 * time.Second},
	}, nil
}

func (s *SplunkSource) Name() string { return "splunk:" + s.name }

// Pull issues an `export` request over the (since, now) window. Results
// are returned as one JSON object per line (output_mode=json); each line
// has shape `{"preview":false,"offset":N,"result":{...}}`.
func (s *SplunkSource) Pull(ctx context.Context, since time.Time) ([]core.Signal, time.Time, error) {
	cursor := since
	earliest := since.UTC()
	if earliest.IsZero() {
		// Cold start window — see GraylogSource for the same rationale.
		earliest = time.Now().UTC().Add(-5 * time.Minute)
	}
	latest := time.Now().UTC()
	if !latest.After(earliest) {
		return nil, cursor, nil
	}

	// `search` is expected to start with the `search` operator; we
	// don't try to be clever about user input here.
	form := url.Values{}
	form.Set("search", normalizeSplunkSearch(s.cfg.Search))
	form.Set("earliest_time", formatSplunkEpoch(earliest))
	form.Set("latest_time", formatSplunkEpoch(latest))
	form.Set("output_mode", "json")
	form.Set("output_time_format", splunkOutputTimeFormat)
	form.Set("count", strconv.Itoa(s.cfg.PageSize))

	u := strings.TrimRight(s.cfg.Address, "/") + "/services/search/v2/jobs/export"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, cursor, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
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
		return nil, cursor, fmt.Errorf("splunk %s: %d %s", u, resp.StatusCode, truncate(string(body), 256))
	}

	signals, maxTS, err := s.parseExport(body, since)
	if err != nil {
		return nil, cursor, err
	}
	if maxTS.After(cursor) {
		cursor = maxTS
	}
	return signals, cursor, nil
}

// parseExport reads the newline-delimited JSON stream Splunk returns
// from /export. Lines without a `result` object (e.g. preview / final
// status records) are silently skipped.
//
// A malformed record fails the whole pull. Export streams an event search
// newest first, so keeping the rows before the bad record and advancing the
// cursor to their max would skip the older rows after it for good; failing
// leaves the cursor where it was and the next tick re-reads the window.
func (s *SplunkSource) parseExport(body []byte, since time.Time) ([]core.Signal, time.Time, error) {
	var signals []core.Signal
	var maxTS time.Time
	dec := json.NewDecoder(strings.NewReader(string(body)))
	for dec.More() {
		var line splunkExportLine
		if err := dec.Decode(&line); err != nil {
			return nil, time.Time{}, fmt.Errorf("splunk export: malformed record after %d results: %w", len(signals), err)
		}
		if line.Result == nil {
			continue
		}
		ts := parseSplunkTime(line.Result[s.cfg.TimeField])
		if ts.IsZero() || !ts.After(since) {
			continue
		}
		if ts.After(maxTS) {
			maxTS = ts
		}

		msg := stringField(line.Result, s.cfg.MessageField)
		sev := stringField(line.Result, s.cfg.SeverityField)

		fields := make(map[string]interface{})
		for _, f := range s.cfg.ExtraFields {
			if v, ok := line.Result[f]; ok {
				fields[f] = v
			}
		}

		signals = append(signals, core.Signal{
			Source:    s.Name(),
			Timestamp: ts,
			Severity:  sev,
			Message:   msg,
			Fields:    fields,
			Raw:       line.Result,
		})
	}
	return signals, maxTS, nil
}

// applyAuth wires up Splunk auth in priority order:
//  1. Token — `Authorization: Bearer <token>`. Splunk's preferred
//     mechanism for REST clients (HEC tokens, auth tokens).
//  2. Username + Password — HTTP Basic.
func (s *SplunkSource) applyAuth(req *http.Request) {
	if s.cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+s.cfg.Token)
		return
	}
	if s.cfg.Username != "" {
		token := base64.StdEncoding.EncodeToString(
			[]byte(s.cfg.Username + ":" + s.cfg.Password),
		)
		req.Header.Set("Authorization", "Basic "+token)
	}
}

// normalizeSplunkSearch ensures the SPL begins with the `search`
// command — Splunk's REST API rejects bare index= expressions.
func normalizeSplunkSearch(q string) string {
	q = strings.TrimSpace(q)
	if q == "" {
		return q
	}
	first := strings.Fields(q)[0]
	if first == "search" || strings.HasPrefix(first, "|") {
		return q
	}
	return "search " + q
}

// formatSplunkEpoch renders a time as sub-second epoch (e.g.
// "1716192000.123") — Splunk's `earliest_time` / `latest_time` accept
// this format directly.
func formatSplunkEpoch(t time.Time) string {
	sec := t.Unix()
	ms := t.UnixNano()/int64(time.Millisecond) - sec*1000
	return fmt.Sprintf("%d.%03d", sec, ms)
}

// parseSplunkTime accepts the renderings Splunk uses for a time field:
// epoch seconds with an optional fraction (what Pull requests for `_time`),
// RFC 3339 ("2026-05-20T10:00:00.000+00:00"), and the display format
// ("2026-05-20 10:00:00.000 GMT", or with a numeric offset). A display time
// with any zone abbreviation other than GMT/UTC is ambiguous and returns
// zero, as does anything unparseable.
func parseSplunkTime(v interface{}) time.Time {
	s, ok := v.(string)
	if !ok {
		return time.Time{}
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	if t, ok := parseSplunkEpoch(s); ok {
		return t
	}
	for _, layout := range []string{
		time.RFC3339Nano,
		"2006-01-02 15:04:05 -0700",
		"2006-01-02 15:04:05 -07:00",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	for _, zone := range []string{" GMT", " UTC", " Z"} {
		if rest, ok := strings.CutSuffix(s, zone); ok {
			if t, err := time.ParseInLocation("2006-01-02 15:04:05", rest, time.UTC); err == nil {
				return t
			}
		}
	}
	return time.Time{}
}

// parseSplunkEpoch reads "1794732023" or "1794732023.294836" without going
// through float64, so microsecond fractions survive exactly.
func parseSplunkEpoch(s string) (time.Time, bool) {
	secPart, fracPart, hasFrac := strings.Cut(s, ".")
	if secPart == "" || (hasFrac && fracPart == "") || len(fracPart) > 9 {
		return time.Time{}, false
	}
	sec, err := strconv.ParseInt(secPart, 10, 64)
	if err != nil || sec < 0 || secPart[0] == '+' {
		return time.Time{}, false
	}
	var nsec int64
	if hasFrac {
		frac, err := strconv.ParseUint(fracPart, 10, 32)
		if err != nil || fracPart[0] == '+' {
			return time.Time{}, false
		}
		nsec = int64(frac)
		for i := len(fracPart); i < 9; i++ {
			nsec *= 10
		}
	}
	return time.Unix(sec, nsec).UTC(), true
}

// -----------------------------------------------------------------------------
// response shape
// -----------------------------------------------------------------------------

type splunkExportLine struct {
	Preview bool                   `json:"preview"`
	Offset  int                    `json:"offset"`
	Result  map[string]interface{} `json:"result"`
}

// splunkOutputTimeFormat asks export to render `_time` as epoch seconds with
// microseconds, which carries no timezone and parses exactly.
const splunkOutputTimeFormat = "%s.%6N"
