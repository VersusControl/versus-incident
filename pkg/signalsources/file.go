package signalsources

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
)

// FileSource tails a log file on disk. It is intended primarily for local
// testing and small-scale deployments — production users should prefer the
// Elasticsearch source.
//
// Behavior:
//
//   - Position is tracked as a byte offset stored in a sidecar cursor file
//     so it survives process restarts.
//   - If the file shrinks between ticks (truncate / rotate / re-create) the
//     source reads from the start.
//   - The Pull `since` argument is ignored: the byte offset is the source of
//     truth. The returned cursor timestamp is just `time.Now()` so the
//     worker has something to log.
//   - Lines longer than MaxLineBytes are truncated (with a marker).
//   - Empty / whitespace-only lines are skipped.
type FileSource struct {
	name string
	cfg  config.AgentFileSourceConfig

	mu        sync.Mutex
	offset    int64 // current read position
	cursorFP  string
	tsLayouts []string
}

// Defaults applied when the corresponding option is empty / zero.
const (
	defaultFileFormat          = "text"
	defaultFileMaxLineBytes    = 64 * 1024
	defaultFileMaxLinesPerPull = 1000
	defaultJSONMessageField    = "message"
	defaultJSONTimestampField  = "@timestamp"
	defaultJSONSeverityField   = "level"
)

// defaultTextTimestampLayouts are tried in order when TimestampLayout is empty.
var defaultTextTimestampLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02T15:04:05.000Z",
	"2006-01-02 15:04:05",
}

// NewFileSource validates configuration and locates the cursor sidecar file.
// It does NOT open the log file; that happens lazily inside Pull so a missing
// file at startup doesn't crash the worker (the file may appear later).
func NewFileSource(name string, cfg config.AgentFileSourceConfig) (*FileSource, error) {
	if cfg.Path == "" {
		return nil, fmt.Errorf("file source %q: path is required", name)
	}
	switch cfg.Format {
	case "", "text", "json":
		// ok
	default:
		return nil, fmt.Errorf("file source %q: unknown format %q (want \"text\" or \"json\")", name, cfg.Format)
	}

	cursorPath := cfg.CursorPath
	if cursorPath == "" {
		// Sit next to the log file by default — keeps everything self-contained
		// for local testing.
		dir := filepath.Dir(cfg.Path)
		cursorPath = filepath.Join(dir, ".versus-cursor-"+sanitizeName(name))
	}

	s := &FileSource{
		name:     name,
		cfg:      cfg,
		cursorFP: cursorPath,
	}
	if cfg.TimestampLayout != "" {
		s.tsLayouts = []string{cfg.TimestampLayout}
	} else {
		s.tsLayouts = defaultTextTimestampLayouts
	}

	if off, err := s.loadOffset(); err == nil {
		s.offset = off
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("file source %q: read cursor %s: %w", name, cursorPath, err)
	} else if !cfg.FromBeginning {
		// First ever run with from_beginning=false: jump to current EOF so we
		// don't pick up history.
		if fi, statErr := os.Stat(cfg.Path); statErr == nil {
			s.offset = fi.Size()
		}
	}

	return s, nil
}

func (s *FileSource) Name() string { return "file:" + s.name }

// Rewind resets the read position to what a brand-new FileSource would use when
// it finds no persisted sidecar cursor: offset 0 when from_beginning is set (so
// the whole file is re-read), else the current EOF (so history the operator
// chose to skip stays skipped). It also removes the sidecar so a restart
// mid-rewind starts from the same place.
//
// This implements core.SourceRewinder. The file source's byte offset is its own
// cursor of truth and it ignores the worker's `since` cursor, so a catalog clear
// that only rewinds the worker cursor would leave this source pinned at EOF and
// unable to re-emit already-consumed lines. Rewind reconciles the two so a clear
// makes the SAME running worker re-read the file in place — the in-memory
// equivalent of recreating the container.
func (s *FileSource) Rewind(_ context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Drop the persisted byte offset so this matches a fresh process with no
	// sidecar yet. A missing sidecar is not an error.
	if err := os.Remove(s.cursorFP); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("file source %q: reset cursor %s: %w", s.name, s.cursorFP, err)
	}

	if s.cfg.FromBeginning {
		s.offset = 0
		return nil
	}
	// from_beginning=false: a fresh start jumps to the current EOF so the
	// backlog the operator opted out of is not replayed by a clear.
	if fi, err := os.Stat(s.cfg.Path); err == nil {
		s.offset = fi.Size()
	} else {
		s.offset = 0
	}
	return nil
}

// Pull reads new content from the file since the last recorded byte offset.
// Errors from a single tick are returned (worker logs and continues); the
// offset is only advanced for content that was successfully read.
func (s *FileSource) Pull(_ context.Context, _ time.Time) ([]core.Signal, time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	cursor := time.Now().UTC()

	f, err := os.Open(s.cfg.Path)
	if err != nil {
		if os.IsNotExist(err) {
			// File not there yet — that's fine, just nothing to do.
			return nil, cursor, nil
		}
		return nil, cursor, fmt.Errorf("open %s: %w", s.cfg.Path, err)
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return nil, cursor, fmt.Errorf("stat %s: %w", s.cfg.Path, err)
	}

	// Detect rotation / truncation: if the file is smaller than where we
	// stopped reading, assume it was rotated and start over.
	if fi.Size() < s.offset {
		s.offset = 0
	}

	if _, err := f.Seek(s.offset, io.SeekStart); err != nil {
		return nil, cursor, fmt.Errorf("seek %s: %w", s.cfg.Path, err)
	}

	maxLine := s.cfg.MaxLineBytes
	if maxLine <= 0 {
		maxLine = defaultFileMaxLineBytes
	}
	maxLines := s.cfg.MaxLinesPerPull
	if maxLines <= 0 {
		maxLines = defaultFileMaxLinesPerPull
	}

	signals, bytesRead, err := s.readSignals(f, maxLine, maxLines)
	// Advance offset by what we successfully consumed even if we hit a
	// read error mid-stream — so we don't infinitely re-read a bad line.
	s.offset += bytesRead
	if persistErr := s.saveOffset(s.offset); persistErr != nil {
		// Non-fatal: offset will be re-saved next tick.
		log.Printf("file source %s: cursor save failed: %v", s.name, persistErr)
	}
	if err != nil && err != io.EOF {
		return signals, cursor, fmt.Errorf("read %s: %w", s.cfg.Path, err)
	}
	return signals, cursor, nil
}

// readSignals reads complete lines from r and converts them to signals.
// It returns the number of bytes consumed (so the caller can advance the
// persistent offset by exactly that much) plus any non-EOF error. When
// maxLines > 0 the loop stops after that many lines have been emitted as
// signals, leaving the rest for the next Pull (the byte offset only
// advances over the lines this call actually consumed).
func (s *FileSource) readSignals(r io.Reader, maxLine, maxLines int) ([]core.Signal, int64, error) {
	br := bufio.NewReaderSize(r, 64*1024)
	var signals []core.Signal
	var consumed int64
	format := s.cfg.Format
	if format == "" {
		format = defaultFileFormat
	}

	for {
		line, err := readLineLimited(br, maxLine)
		if len(line) > 0 {
			consumed += int64(len(line))
			text := strings.TrimRight(line, "\r\n")
			if strings.TrimSpace(text) != "" {
				if sig, ok := s.lineToSignal(text, format); ok {
					signals = append(signals, sig)
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				return signals, consumed, nil
			}
			return signals, consumed, err
		}
		if maxLines > 0 && len(signals) >= maxLines {
			return signals, consumed, nil
		}
	}
}

// readLineLimited reads up to maxLine bytes of a single line plus its newline
// terminator. If the line exceeds maxLine, the rest is consumed and discarded
// so the offset advances correctly. Returns the (possibly truncated) line
// including the trailing newline if any.
func readLineLimited(br *bufio.Reader, maxLine int) (string, error) {
	var sb strings.Builder
	overflowed := false
	for {
		b, err := br.ReadByte()
		if err != nil {
			if sb.Len() == 0 {
				return "", err
			}
			return sb.String(), err
		}
		if b == '\n' {
			if !overflowed {
				sb.WriteByte(b)
			}
			return sb.String(), nil
		}
		if !overflowed {
			if sb.Len() < maxLine {
				sb.WriteByte(b)
			} else {
				// Mark truncation once and keep draining until newline.
				sb.WriteString("…[truncated]")
				overflowed = true
			}
		}
	}
}

func (s *FileSource) lineToSignal(line, format string) (core.Signal, bool) {
	switch format {
	case "json":
		return s.jsonLineToSignal(line)
	default:
		return s.textLineToSignal(line)
	}
}

func (s *FileSource) textLineToSignal(line string) (core.Signal, bool) {
	ts, rest, ok := s.tryParseLeadingTimestamp(line)
	if !ok {
		ts = time.Now().UTC()
		rest = line
	}
	return core.Signal{
		Source:    s.Name(),
		Timestamp: ts,
		// Message is the line WITHOUT the leading timestamp so identical
		// messages with different timestamps cluster together. Raw keeps the
		// full original line for debugging.
		Message: rest,
		Raw:     map[string]interface{}{"line": line},
	}, true
}

// tryParseLeadingTimestamp tests each configured layout against the start of
// the line. We accept either a bare timestamp followed by whitespace or a
// timestamp wrapped in brackets like "[2024-01-02T15:04:05Z] ...". On success
// it also returns the remainder of the line (with the timestamp + any
// trailing bracket / whitespace stripped).
//
// The lookup splits on the first whitespace and tries to parse the prefix.
// We do NOT key off `len(layout)` because Go layouts contain reference
// strings like `Z07:00` that are longer than the actual rendered timestamp
// (e.g. RFC3339 is 25 chars but a `…Z` timestamp is only 20). Splitting on
// the first space is robust to any layout the user configures.
func (s *FileSource) tryParseLeadingTimestamp(line string) (time.Time, string, bool) {
	candidate := strings.TrimLeft(line, "[")
	leadingBracket := len(line) - len(candidate)

	// Find the first whitespace — that's the end of the timestamp token.
	// Falls back to the whole string if the line has no whitespace.
	end := strings.IndexAny(candidate, " \t")
	if end < 0 {
		end = len(candidate)
	}
	head := candidate[:end]
	// If the timestamp was wrapped in brackets the closing `]` will be at
	// the end of `head`; strip it before parsing.
	if leadingBracket > 0 && strings.HasSuffix(head, "]") {
		head = head[:len(head)-1]
	}

	for _, layout := range s.tsLayouts {
		if t, err := time.Parse(layout, head); err == nil {
			rest := candidate[end:]
			if leadingBracket > 0 {
				rest = strings.TrimPrefix(rest, "]")
			}
			rest = strings.TrimLeft(rest, " \t")
			return t.UTC(), rest, true
		}
	}
	return time.Time{}, "", false
}

func (s *FileSource) jsonLineToSignal(line string) (core.Signal, bool) {
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		// Fall back to text behavior so a malformed line isn't lost.
		return s.textLineToSignal(line)
	}

	msgField := s.cfg.MessageField
	if msgField == "" {
		msgField = defaultJSONMessageField
	}
	tsField := s.cfg.TimestampField
	if tsField == "" {
		tsField = defaultJSONTimestampField
	}
	sevField := s.cfg.SeverityField
	if sevField == "" {
		sevField = defaultJSONSeverityField
	}

	msg, _ := m[msgField].(string)
	if msg == "" {
		// No usable message — emit the whole line so the operator sees it.
		msg = line
	}
	severity, _ := m[sevField].(string)

	ts := time.Now().UTC()
	if v, ok := m[tsField]; ok {
		if parsed, ok := parseJSONTimestamp(v); ok {
			ts = parsed
		}
	}

	return core.Signal{
		Source:    s.Name(),
		Timestamp: ts,
		Severity:  severity,
		Message:   msg,
		Fields:    m,
		Raw:       m,
	}, true
}

func parseJSONTimestamp(v interface{}) (time.Time, bool) {
	switch t := v.(type) {
	case string:
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05"} {
			if parsed, err := time.Parse(layout, t); err == nil {
				return parsed.UTC(), true
			}
		}
	case float64:
		// Heuristic: > 1e12 is millis, otherwise seconds.
		if t > 1e12 {
			return time.UnixMilli(int64(t)).UTC(), true
		}
		return time.Unix(int64(t), 0).UTC(), true
	}
	return time.Time{}, false
}

// -----------------------------------------------------------------------------
// cursor sidecar file
// -----------------------------------------------------------------------------

func (s *FileSource) loadOffset() (int64, error) {
	b, err := os.ReadFile(s.cursorFP)
	if err != nil {
		return 0, err
	}
	off, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid cursor file %s: %w", s.cursorFP, err)
	}
	if off < 0 {
		return 0, nil
	}
	return off, nil
}

func (s *FileSource) saveOffset(off int64) error {
	if err := os.MkdirAll(filepath.Dir(s.cursorFP), 0o755); err != nil {
		return err
	}
	tmp := s.cursorFP + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.FormatInt(off, 10)), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.cursorFP)
}

func sanitizeName(s string) string {
	r := strings.NewReplacer("/", "_", " ", "_", ":", "_", "..", "_")
	return r.Replace(s)
}
