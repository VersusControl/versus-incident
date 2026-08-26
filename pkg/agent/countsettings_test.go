package agent

import (
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/storage"
)

func TestCountSettings_DefaultsWhenAbsent(t *testing.T) {
	// nil store → built-in default (last 7 days).
	if got := LoadCountSettings(nil); got.Window != CountWindow7d {
		t.Fatalf("nil-store default = %+v, want window=7d", got)
	}
	// fresh store with no blob → same default.
	if got := LoadCountSettings(storage.NewMemory()); got.Window != CountWindow7d {
		t.Fatalf("empty-store default = %+v, want window=7d", got)
	}
}

func TestCountSettings_SaveLoadRoundTrip(t *testing.T) {
	st := storage.NewMemory()
	for _, w := range []string{CountWindow24h, CountWindow7d, CountWindow30d, CountWindow90d, CountWindowAll} {
		if err := SaveCountSettings(st, CountSettings{Window: w}); err != nil {
			t.Fatalf("SaveCountSettings(%q): %v", w, err)
		}
		if got := LoadCountSettings(st); got.Window != w {
			t.Fatalf("roundtrip %q = %+v", w, got)
		}
	}
}

func TestCountSettings_UnknownNormalizesToDefault(t *testing.T) {
	st := storage.NewMemory()
	// Save-side sanitize folds an unknown window to the default.
	if err := SaveCountSettings(st, CountSettings{Window: "6mo"}); err != nil {
		t.Fatalf("SaveCountSettings: %v", err)
	}
	if got := LoadCountSettings(st); got.Window != CountWindow7d {
		t.Fatalf("save-side normalize = %+v, want 7d", got)
	}
	// Read-side normalize: a hand-edited or legacy blob with a bogus window
	// loads as the default rather than the raw value.
	if err := st.WriteBlob(CountSettingsBlobName, []byte(`{"window":"nonsense"}`)); err != nil {
		t.Fatalf("WriteBlob: %v", err)
	}
	if got := LoadCountSettings(st); got.Window != CountWindow7d {
		t.Fatalf("read-side normalize = %+v, want 7d", got)
	}
	// A corrupt blob is a fresh start, not an error.
	if err := st.WriteBlob(CountSettingsBlobName, []byte(`{not json`)); err != nil {
		t.Fatalf("WriteBlob: %v", err)
	}
	if got := LoadCountSettings(st); got.Window != CountWindow7d {
		t.Fatalf("corrupt blob = %+v, want 7d", got)
	}
}

func TestSaveCountSettings_NoStorage(t *testing.T) {
	if err := SaveCountSettings(nil, CountSettings{Window: CountWindow30d}); err != ErrCountNoStorage {
		t.Fatalf("err = %v, want ErrCountNoStorage", err)
	}
}

func TestKnownCountWindow(t *testing.T) {
	for _, ok := range []string{CountWindow24h, CountWindow7d, CountWindow30d, CountWindow90d, CountWindowAll} {
		if !KnownCountWindow(ok) {
			t.Fatalf("KnownCountWindow(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"", " 7d", "7D", "1h", "7days", "forever"} {
		if KnownCountWindow(bad) {
			t.Fatalf("KnownCountWindow(%q) = true, want false", bad)
		}
	}
}

// TestCountSettings_Since pins the window→cutoff mapping the count path reads.
// "all" and an unrecognized window must both yield the ZERO time, which every
// count path treats as unbounded — a non-zero cutoff there would silently hide
// incidents.
func TestCountSettings_Since(t *testing.T) {
	now := time.Date(2026, 3, 15, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		window string
		want   time.Time
	}{
		{CountWindow24h, now.Add(-24 * time.Hour)},
		{CountWindow7d, now.AddDate(0, 0, -7)},
		{CountWindow30d, now.AddDate(0, 0, -30)},
		{CountWindow90d, now.AddDate(0, 0, -90)},
		{CountWindowAll, time.Time{}},
		// An absent or bogus window sanitizes to the default, so it must behave
		// as 7d — NOT as "all".
		{"", now.AddDate(0, 0, -7)},
		{"nonsense", now.AddDate(0, 0, -7)},
	}

	for _, tc := range cases {
		got := CountSettings{Window: tc.window}.Since(now)
		if !got.Equal(tc.want) {
			t.Fatalf("Since(%q) = %v, want %v", tc.window, got, tc.want)
		}
	}
}
