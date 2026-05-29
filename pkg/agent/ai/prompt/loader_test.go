package prompt

import (
	"embed"
	"strings"
	"testing"
)

//go:embed testdata/*.md
var testFS embed.FS

func TestAssemble_OrderAndSeparator(t *testing.T) {
	got, err := Assemble(testFS, []string{
		"testdata/A.md",
		"testdata/B.md",
	})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if !strings.HasPrefix(got, "A-content") {
		t.Fatalf("want A first, got: %q", got)
	}
	if !strings.Contains(got, "A-content\n\n\nB-content") &&
		!strings.Contains(got, "A-content\nB-content") {
		// Either trailing newline in A or not; require exactly one blank
		// line separator between fragments.
		if !strings.Contains(got, "\n\nB-content") {
			t.Fatalf("want blank-line separator between fragments, got: %q", got)
		}
	}
}

func TestAssemble_MissingFile(t *testing.T) {
	if _, err := Assemble(testFS, []string{"testdata/nope.md"}); err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestMustAssemble_PanicsOnMissing(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic")
		}
	}()
	_ = MustAssemble(testFS, []string{"testdata/nope.md"})
}

func TestAssemble_Idempotent(t *testing.T) {
	order := []string{"testdata/A.md", "testdata/B.md"}
	a, _ := Assemble(testFS, order)
	b, _ := Assemble(testFS, order)
	if a != b {
		t.Fatal("Assemble not idempotent")
	}
}
