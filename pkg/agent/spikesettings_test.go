package agent

import (
	"testing"

	"github.com/VersusControl/versus-incident/pkg/storage"
)

func TestSpikeSettings_DefaultsWhenAbsent(t *testing.T) {
	// nil store → built-in default (global rate baseline).
	if got := LoadSpikeSettings(nil); got.BaselineMode != baselineModeDefault {
		t.Fatalf("nil-store default = %+v, want baseline_mode=default", got)
	}
	// fresh store with no blob → same default.
	if got := LoadSpikeSettings(storage.NewMemory()); got.BaselineMode != baselineModeDefault {
		t.Fatalf("empty-store default = %+v, want baseline_mode=default", got)
	}
}

func TestSpikeSettings_SaveLoadRoundTrip(t *testing.T) {
	st := storage.NewMemory()
	for _, mode := range []string{baselineModeDefault, baselineModeAverage, baselineModeTimeOfDay} {
		if err := SaveSpikeSettings(st, SpikeSettings{BaselineMode: mode}); err != nil {
			t.Fatalf("SaveSpikeSettings(%q): %v", mode, err)
		}
		if got := LoadSpikeSettings(st); got.BaselineMode != mode {
			t.Fatalf("roundtrip %q = %+v", mode, got)
		}
	}
}

func TestSpikeSettings_UnknownNormalizesToDefault(t *testing.T) {
	st := storage.NewMemory()
	// Save-side sanitize folds an unknown mode to the default.
	if err := SaveSpikeSettings(st, SpikeSettings{BaselineMode: "wat"}); err != nil {
		t.Fatalf("SaveSpikeSettings: %v", err)
	}
	if got := LoadSpikeSettings(st); got.BaselineMode != baselineModeDefault {
		t.Fatalf("save-side normalize = %+v, want default", got)
	}
	// Read-side normalize: a hand-edited/legacy blob with a bogus mode loads as
	// the default rather than the raw value.
	if err := st.WriteBlob(SpikeSettingsBlobName, []byte(`{"baseline_mode":"nonsense"}`)); err != nil {
		t.Fatalf("WriteBlob: %v", err)
	}
	if got := LoadSpikeSettings(st); got.BaselineMode != baselineModeDefault {
		t.Fatalf("read-side normalize = %+v, want default", got)
	}
}

func TestSaveSpikeSettings_NoStorage(t *testing.T) {
	if err := SaveSpikeSettings(nil, SpikeSettings{BaselineMode: baselineModeAverage}); err != ErrSpikeNoStorage {
		t.Fatalf("err = %v, want ErrSpikeNoStorage", err)
	}
}

func TestKnownBaselineMode(t *testing.T) {
	for _, ok := range []string{baselineModeDefault, baselineModeAverage, baselineModeTimeOfDay} {
		if !KnownBaselineMode(ok) {
			t.Fatalf("KnownBaselineMode(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"", "wat", "Default", " average", "hour_of_week"} {
		if KnownBaselineMode(bad) {
			t.Fatalf("KnownBaselineMode(%q) = true, want false", bad)
		}
	}
}
