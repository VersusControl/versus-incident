package mount

import (
	"errors"
	"time"

	awstools "github.com/VersusControl/versus-incident/pkg/agent/ai/tools/aws"
	awscollect "github.com/VersusControl/versus-incident/pkg/aws/collect"
	awssettings "github.com/VersusControl/versus-incident/pkg/aws/settings"
	awssnapshot "github.com/VersusControl/versus-incident/pkg/aws/snapshot"
	c "github.com/VersusControl/versus-incident/pkg/config"
)

// changeListFor binds the agent's change list to the live config: with the
// AWS connector or its collection off there is no change list, so the tools
// say AWS changes are not set up rather than answer from stale data.
func changeListFor(current func() c.AWSConnectorConfig, feed *awssnapshot.ChangeFeed) awstools.ChangesFunc {
	return func() awstools.ChangeAnswerer {
		conn := current()
		if !conn.Enable || !conn.Collect.Enable {
			return nil
		}
		return feed
	}
}

// accountSource is where the request-path scopes read the account list
// and each account's health. stored, when set, is the settings service's
// listing kept until the account revision changes, so a page request does
// not list the stored accounts again; health is still read per account.
type accountSource struct {
	store  *awssettings.Store
	stored func() ([]awssettings.AccountRecord, error)
}

func (source accountSource) listAccounts(org string) ([]awssettings.AccountRecord, error) {
	if source.stored != nil {
		return source.stored()
	}
	return source.store.ListAccounts(org)
}

// changeScope lists every connected account with its readable regions, the
// selected regions AWS reports as not enabled, the services its last probe
// found denied or not on its plan, and any account-level outage from its
// saved health.
func changeScope(store accountSource, org string, conn c.AWSConnectorConfig) ([]awssnapshot.ScopeAccount, bool, error) {
	return scopeAccounts(store, org, conn, changeRegions)
}

// changeRegions is the Changes page's Region rule: the selected Regions,
// or the ones the health check found enabled, less those AWS reports as
// not enabled.
func changeRegions(account awssettings.AccountRecord, health awssettings.HealthRecord) (regions, notEnabled []string) {
	regions = account.Regions
	if len(regions) == 0 {
		for _, region := range health.Regions {
			if region.Enabled {
				regions = append(regions, region.Region)
			}
		}
	}
	return readableRegions(regions, health.NotEnabledRegions(regions))
}

// coverageScope lists the accounts the coverage page expects, each with the
// Regions the collectors really read (settings.CollectRegions), so a unit
// read in a Region whose opt-in status went stale stays in scope.
func coverageScope(store accountSource, org string, conn c.AWSConnectorConfig, now func() time.Time) ([]awssnapshot.ScopeAccount, bool, error) {
	return scopeAccounts(store, org, conn, func(account awssettings.AccountRecord, health awssettings.HealthRecord) ([]string, []string) {
		regions, off := awssettings.CollectRegions(account, health, now().UTC())
		return readableRegions(regions, off)
	})
}

// readableRegions splits regions into the ones to read and the ones off.
func readableRegions(regions, off []string) ([]string, []string) {
	skip := make(map[string]bool, len(off))
	for _, region := range off {
		skip[region] = true
	}
	var readable []string
	for _, region := range regions {
		if !skip[region] {
			readable = append(readable, region)
		}
	}
	return readable, off
}

// scopeAccounts lists every connected account with the Regions regionsFor
// gives it, the services its last probe found denied or not on its plan,
// and any account-level outage from its saved health.
func scopeAccounts(source accountSource, org string, conn c.AWSConnectorConfig,
	regionsFor func(awssettings.AccountRecord, awssettings.HealthRecord) (regions, notEnabled []string)) ([]awssnapshot.ScopeAccount, bool, error) {
	store := source.store
	stored, err := source.listAccounts(org)
	if err != nil {
		return nil, false, err
	}
	var out []awssnapshot.ScopeAccount
	for _, account := range awssettings.MergeAccounts(stored, conn.Accounts) {
		health, err := store.GetHealth(org, account.AccountID)
		if err != nil && !errors.Is(err, awssettings.ErrNotFound) {
			return nil, false, err
		}
		entry := awssnapshot.ScopeAccount{Account: account.AccountID, Name: account.DisplayName}
		entry.Regions, entry.NotEnabled = regionsFor(account, health)
		if len(health.ProbeResults) > 0 {
			counts := awssettings.CountResults(health.ProbeResults)
			entry.DeniedServices, entry.NotOnPlanServices = counts.DeniedServices, counts.NotOnPlanServices
		}
		if health.Status == awssettings.StatusDisconnected {
			entry.Disconnected = true
			entry.Outage = outageReason(health.ErrorCode)
			entry.OutageSince = health.DisconnectedSince
			if entry.OutageSince.IsZero() {
				entry.OutageSince = health.FailureSince
			}
			entry.LastReadAt = health.CheckedAt
		}
		out = append(out, entry)
	}
	return out, true, nil
}

// collectAccountHealth reads an account's last identity check for the
// collector: healthy when the check signed in, failing when it is failing
// or the account is disconnected, unknown before any check finished.
func collectAccountHealth(store *awssettings.Store, org string) func(account string) awscollect.AccountHealth {
	return func(account string) awscollect.AccountHealth {
		health, err := store.GetHealth(org, account)
		if err != nil {
			return awscollect.AccountHealthUnknown
		}
		return accountHealthOf(health)
	}
}

func accountHealthOf(health awssettings.HealthRecord) awscollect.AccountHealth {
	switch {
	case health.Status == awssettings.StatusDisconnected, !health.AttemptedAt.IsZero() && !health.AttemptOK:
		return awscollect.AccountHealthFailing
	case health.AttemptOK:
		return awscollect.AccountHealthHealthy
	}
	return awscollect.AccountHealthUnknown
}

// outageReason maps a saved health error onto the change gap reason.
func outageReason(code string) awssnapshot.ChangeGapReason {
	switch code {
	case "no_server_identity", awssettings.CauseCredentialsInvalid:
		return awssnapshot.GapCredentialsInvalid
	case "account_mismatch":
		return awssnapshot.GapAccountMismatch
	case "role_not_found", "trust_denied", "external_id_mismatch", "session_duration_invalid":
		return awssnapshot.GapAssumeRoleFailed
	}
	return awssnapshot.GapAccountDisconnected
}
