package main

import (
	"context"
	"errors"

	"github.com/gofiber/fiber/v2"

	"github.com/VersusControl/versus-incident/pkg/agent"
	awstools "github.com/VersusControl/versus-incident/pkg/agent/ai/tools/aws"
	awscollect "github.com/VersusControl/versus-incident/pkg/aws/collect"
	awssettings "github.com/VersusControl/versus-incident/pkg/aws/settings"
	awssnapshot "github.com/VersusControl/versus-incident/pkg/aws/snapshot"
	"github.com/VersusControl/versus-incident/pkg/aws/snapshot/alarmsuspect"
	c "github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/controllers"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

// registerAWSChanges mounts the read-only Changes page over the snapshot
// store. The expected scope is the configured accounts and regions, read
// from settings at request time, so a region added a moment ago shows as
// not yet read rather than as quiet.
func registerAWSChanges(app *fiber.App, store storage.Provider, cfg *c.Config, connector awsConnector) *awssnapshot.ChangeFeed {
	current := func() c.AWSConnectorConfig {
		if live := c.GetConfigOrNil(); live != nil {
			return live.Connectors.AWS
		}
		return cfg.Connectors.AWS
	}
	scope := tenancy.DefaultOrgScope()
	settingsStore := awssettings.NewStore(store, scope)
	feed := awssnapshot.NewChangeFeed(connector.snapshots,
		awssnapshot.WithSuspectEngine(alarmsuspect.New(connector.snapshots)),
		awssnapshot.WithScope(func(context.Context) ([]awssnapshot.ScopeAccount, bool, error) {
			conn := current()
			if !conn.Enable {
				return nil, true, nil
			}
			return changeScope(settingsStore, scope.Write, conn)
		}),
		awssnapshot.WithFeedOptions(func() awssnapshot.FeedOptions {
			collect := current().Collect
			return awssnapshot.FeedOptions{
				Lookback:    collect.ChangeLookback(),
				Overlap:     collect.ChangeOverlap(),
				Interval:    collect.ChangesInterval(),
				BurstWindow: collect.ChangeBurstWindow(),
				BurstMin:    collect.ChangeBurstMin(),
				ReadCap:     collect.ChangeReadCap(),
			}
		}),
	)
	controllers.NewAWSChangesController(feed).Register(app.Group("/api"))
	// The agent's get_aws_changes tool and the recent_changes feed answer
	// from the same change list as the page.
	agent.ConfigureAWSChanges(changeListFor(current, feed))
	return feed
}

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

// changeScope lists every connected account with its readable regions, the
// selected regions AWS reports as not enabled, the services its last probe
// found denied or not on its plan, and any account-level outage from its
// saved health.
func changeScope(store *awssettings.Store, org string, conn c.AWSConnectorConfig) ([]awssnapshot.ScopeAccount, bool, error) {
	stored, err := store.ListAccounts(org)
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
		regions := account.Regions
		if len(regions) == 0 {
			for _, region := range health.Regions {
				if region.Enabled {
					regions = append(regions, region.Region)
				}
			}
		}
		// Selected regions AWS reports as not enabled are listed as such,
		// never read and never quiet; a region whose state is unknown is
		// still expected to be read.
		entry.NotEnabled = health.NotEnabledRegions(regions)
		notEnabled := make(map[string]bool, len(entry.NotEnabled))
		for _, region := range entry.NotEnabled {
			notEnabled[region] = true
		}
		for _, region := range regions {
			if !notEnabled[region] {
				entry.Regions = append(entry.Regions, region)
			}
		}
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
