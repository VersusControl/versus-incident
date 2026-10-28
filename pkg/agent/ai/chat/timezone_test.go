package chat

import (
	"testing"

	"github.com/VersusControl/versus-incident/pkg/storage"
)

func TestLocationFromReportSettings(t *testing.T) {
	provider := storage.NewMemory()
	if err := provider.WriteBlob(reportSettingsBlobName, []byte(`{"timezone":"America/New_York"}`)); err != nil {
		t.Fatal(err)
	}
	if got := LocationFromReportSettings(provider); got.String() != "America/New_York" {
		t.Fatalf("location = %s", got)
	}
	if got := LocationFromReportSettings(storage.NewMemory()); got.String() != "UTC" {
		t.Fatalf("default location = %s", got)
	}
}
