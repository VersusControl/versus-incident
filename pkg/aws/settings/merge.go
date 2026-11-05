package settings

import (
	"sort"

	"github.com/VersusControl/versus-incident/pkg/config"
)

// MergeAccounts combines stored wizard accounts with config-defined accounts.
// A config account wins on the same account ID, so an operator's YAML always
// describes the truth for an account it names. The result is sorted by account
// ID for a stable response.
func MergeAccounts(stored []AccountRecord, configured []config.AWSAccountConfig) []AccountRecord {
	merged := make(map[string]AccountRecord, len(stored)+len(configured))
	for _, record := range stored {
		if !ValidAccountID(record.AccountID) {
			continue
		}
		record.Source = SourceWizard
		merged[record.AccountID] = record
	}
	for _, account := range configured {
		if !ValidAccountID(account.AccountID) {
			continue
		}
		record := AccountRecord{
			AccountID:   account.AccountID,
			DisplayName: account.DisplayName,
			RoleARN:     account.Auth.RoleARN,
			ExternalID:  account.Auth.ExternalID,
			Regions:     append([]string(nil), account.Regions...),
			Source:      SourceConfig,
		}
		if record.DisplayName == "" {
			record.DisplayName = account.AccountID
		}
		merged[account.AccountID] = record
	}
	out := make([]AccountRecord, 0, len(merged))
	for _, record := range merged {
		out = append(out, record)
	}
	sort.Slice(out, func(left, right int) bool { return out[left].AccountID < out[right].AccountID })
	return out
}
