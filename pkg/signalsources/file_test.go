package signalsources

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/config"
)

func writeFile(t *testing.T, p, s string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
}

func appendFile(t *testing.T, p, s string) {
	t.Helper()
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open append %s: %v", p, err)
	}
	defer f.Close()
	if _, err := f.WriteString(s); err != nil {
		t.Fatalf("append %s: %v", p, err)
	}
}

func TestFileSource_FromBeginningReadsAllAndAdvances(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "app.log")
	cursorPath := filepath.Join(dir, "cursor")

	writeFile(t, logPath, "line one\nline two\nline three\n")

	src, err := NewFileSource("test", config.AgentFileSourceConfig{
		Path:          logPath,
		Format:        "text",
		FromBeginning: true,
		CursorPath:    cursorPath,
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if src.Name() != "file:test" {
		t.Errorf("unexpected name %q", src.Name())
	}

	signals, _, err := src.Pull(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	if len(signals) != 3 {
		t.Fatalf("expected 3 signals, got %d", len(signals))
	}
	if signals[0].Message != "line one" || signals[2].Message != "line three" {
		t.Errorf("unexpected messages: %+v", signals)
	}

	// Cursor must have been written.
	if _, err := os.Stat(cursorPath); err != nil {
		t.Fatalf("cursor not persisted: %v", err)
	}

	// Second pull with no new content returns nothing.
	signals, _, err = src.Pull(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("pull2: %v", err)
	}
	if len(signals) != 0 {
		t.Errorf("expected 0 signals on second pull, got %d", len(signals))
	}

	// Append more lines and verify only new ones come through.
	appendFile(t, logPath, "line four\nline five\n")
	signals, _, err = src.Pull(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("pull3: %v", err)
	}
	if len(signals) != 2 || signals[0].Message != "line four" || signals[1].Message != "line five" {
		t.Errorf("unexpected after append: %+v", signals)
	}
}

func TestFileSource_TailFromEOF(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "app.log")
	cursorPath := filepath.Join(dir, "cursor")

	writeFile(t, logPath, "history one\nhistory two\n")

	src, err := NewFileSource("test", config.AgentFileSourceConfig{
		Path:          logPath,
		Format:        "text",
		FromBeginning: false, // tail
		CursorPath:    cursorPath,
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	signals, _, err := src.Pull(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	if len(signals) != 0 {
		t.Errorf("tail mode must skip pre-existing content, got %d signals", len(signals))
	}

	appendFile(t, logPath, "fresh\n")
	signals, _, err = src.Pull(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("pull2: %v", err)
	}
	if len(signals) != 1 || signals[0].Message != "fresh" {
		t.Errorf("expected single fresh signal, got %+v", signals)
	}
}

func TestFileSource_HandlesRotation(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "app.log")

	writeFile(t, logPath, "old line one\nold line two\nold line three\nold line four\n")
	src, err := NewFileSource("rot", config.AgentFileSourceConfig{
		Path:          logPath,
		Format:        "text",
		FromBeginning: true,
		CursorPath:    filepath.Join(dir, "cursor"),
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if _, _, err := src.Pull(context.Background(), time.Time{}); err != nil {
		t.Fatalf("pull1: %v", err)
	}

	// Truncate + re-write smaller (rotation).
	writeFile(t, logPath, "r1\n")

	signals, _, err := src.Pull(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("pull2: %v", err)
	}
	if len(signals) != 1 || signals[0].Message != "r1" {
		t.Errorf("rotation not detected: %+v", signals)
	}
}

func TestFileSource_JSONFormat(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "app.json")

	writeFile(t, logPath,
		`{"@timestamp":"2026-04-20T10:00:00Z","level":"error","message":"db down","service":"api"}`+"\n"+
			`{"@timestamp":"2026-04-20T10:00:01Z","level":"info","message":"ok"}`+"\n",
	)

	src, err := NewFileSource("j", config.AgentFileSourceConfig{
		Path:          logPath,
		Format:        "json",
		FromBeginning: true,
		CursorPath:    filepath.Join(dir, "cursor"),
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	signals, _, err := src.Pull(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	if len(signals) != 2 {
		t.Fatalf("expected 2, got %d", len(signals))
	}
	if signals[0].Message != "db down" {
		t.Errorf("message field not extracted: %q", signals[0].Message)
	}
	if signals[0].Severity != "error" {
		t.Errorf("severity not extracted: %q", signals[0].Severity)
	}
	if got, _ := signals[0].Fields["service"].(string); got != "api" {
		t.Errorf("fields not preserved: %+v", signals[0].Fields)
	}
	want, _ := time.Parse(time.RFC3339, "2026-04-20T10:00:00Z")
	if !signals[0].Timestamp.Equal(want) {
		t.Errorf("timestamp not parsed: got %v want %v", signals[0].Timestamp, want)
	}
}

func TestFileSource_TruncatesLongLines(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "app.log")
	long := strings.Repeat("A", 1000) + "\nshort\n"
	writeFile(t, logPath, long)

	src, err := NewFileSource("t", config.AgentFileSourceConfig{
		Path:          logPath,
		Format:        "text",
		FromBeginning: true,
		CursorPath:    filepath.Join(dir, "cursor"),
		MaxLineBytes:  100,
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	signals, _, err := src.Pull(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	if len(signals) != 2 {
		t.Fatalf("expected 2 signals, got %d", len(signals))
	}
	if !strings.Contains(signals[0].Message, "[truncated]") {
		t.Errorf("expected truncation marker, got %q", signals[0].Message)
	}
	if signals[1].Message != "short" {
		t.Errorf("second line lost after truncated first: %q", signals[1].Message)
	}
}

func TestFileSource_MissingFileIsNotFatal(t *testing.T) {
	dir := t.TempDir()
	src, err := NewFileSource("m", config.AgentFileSourceConfig{
		Path:          filepath.Join(dir, "does-not-exist.log"),
		Format:        "text",
		FromBeginning: true,
		CursorPath:    filepath.Join(dir, "cursor"),
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	signals, _, err := src.Pull(context.Background(), time.Time{})
	if err != nil {
		t.Errorf("missing file should not error, got %v", err)
	}
	if len(signals) != 0 {
		t.Errorf("expected no signals, got %d", len(signals))
	}
}
