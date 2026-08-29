package chat

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/VersusControl/versus-incident/pkg/storage"
)

const reportSettingsBlobName = "models/settings/report-settings"

// LocationFromReportSettings returns the durable report timezone used when a
// chat turn resolves time hints. Runtime changes apply to the next turn;
// unreadable, absent, or invalid settings fall back to UTC.
func LocationFromReportSettings(provider storage.Provider) *time.Location {
	if provider == nil {
		return time.UTC
	}
	data, err := provider.ReadBlob(reportSettingsBlobName)
	if err != nil || len(data) == 0 {
		return time.UTC
	}
	var settings struct {
		Timezone string `json:"timezone"`
	}
	if json.Unmarshal(data, &settings) != nil {
		return time.UTC
	}
	location, err := time.LoadLocation(strings.TrimSpace(settings.Timezone))
	if err != nil {
		return time.UTC
	}
	return location
}
