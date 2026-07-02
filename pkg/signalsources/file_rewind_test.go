package signalsources

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
)

// TestFileSource_Rewind_ReReadsFromBeginning proves the core.SourceRewinder
// implementation: after a rewind, a from_beginning file source re-reads the
// whole file in place — the in-memory equivalent of a fresh process that found
// no sidecar. This is what lets a catalog clear make the SAME running worker
// relearn a file source's backlog without recreating the container.
func TestFileSource_Rewind_ReReadsFromBeginning(t *testing.T) {
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

	ctx := context.Background()
	first, _, err := src.Pull(ctx, time.Time{})
	if err != nil {
		t.Fatalf("pull1: %v", err)
	}
	if len(first) != 3 {
		t.Fatalf("pull1: got %d signals, want 3", len(first))
	}
	// A second pull with no new content is empty (offset at EOF).
	if again, _, _ := src.Pull(ctx, time.Time{}); len(again) != 0 {
		t.Fatalf("pull2 before rewind: got %d signals, want 0", len(again))
	}

	// The rewind seam resets the byte offset to the start.
	r, ok := interface{}(src).(core.SourceRewinder)
	if !ok {
		t.Fatal("FileSource does not implement core.SourceRewinder")
	}
	if err := r.Rewind(ctx); err != nil {
		t.Fatalf("rewind: %v", err)
	}

	after, _, err := src.Pull(ctx, time.Time{})
	if err != nil {
		t.Fatalf("pull after rewind: %v", err)
	}
	if len(after) != 3 {
		t.Fatalf("after rewind: re-read %d signals, want 3 (rewind did not reset the byte offset)", len(after))
	}
	if after[0].Message != "line one" || after[2].Message != "line three" {
		t.Errorf("after rewind: unexpected messages: %+v", after)
	}
}

// TestFileSource_Rewind_NoBeginningJumpsToEOF proves rewind is faithful to a
// fresh from_beginning=false start: it does NOT replay the backlog the operator
// opted out of; only content appended after the rewind is read.
func TestFileSource_Rewind_NoBeginningJumpsToEOF(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "app.log")
	cursorPath := filepath.Join(dir, "cursor")
	writeFile(t, logPath, "old one\nold two\n")

	src, err := NewFileSource("test", config.AgentFileSourceConfig{
		Path:          logPath,
		Format:        "text",
		FromBeginning: false,
		CursorPath:    cursorPath,
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	ctx := context.Background()
	// First pull: from_beginning=false skips the existing backlog.
	if got, _, _ := src.Pull(ctx, time.Time{}); len(got) != 0 {
		t.Fatalf("pull1: got %d signals, want 0 (from_beginning=false skips backlog)", len(got))
	}
	appendFile(t, logPath, "new one\n")
	if got, _, _ := src.Pull(ctx, time.Time{}); len(got) != 1 {
		t.Fatalf("pull2: got %d signals, want 1", len(got))
	}

	// Rewind must NOT replay the pre-existing backlog — a fresh no-beginning
	// start jumps to EOF.
	r := interface{}(src).(core.SourceRewinder)
	if err := r.Rewind(ctx); err != nil {
		t.Fatalf("rewind: %v", err)
	}
	if got, _, _ := src.Pull(ctx, time.Time{}); len(got) != 0 {
		t.Fatalf("after rewind: got %d signals, want 0 (from_beginning=false must not replay backlog)", len(got))
	}
	// Only content appended after the rewind is read.
	appendFile(t, logPath, "post rewind\n")
	got, _, err := src.Pull(ctx, time.Time{})
	if err != nil {
		t.Fatalf("pull after append: %v", err)
	}
	if len(got) != 1 || got[0].Message != "post rewind" {
		t.Fatalf("after rewind append: got %+v, want [post rewind]", got)
	}
}
