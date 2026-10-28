package controllers

import (
	"context"
	"sync"
)

// report_channels.go — the single-slot, process-wide report channel-lister seam.
//
// The incident-report channel picker (capabilities.report.channels) must offer
// exactly the notification channels an operator can actually target, INCLUDING
// channels configured at runtime (the admin-page "Alert channels" hot-config),
// not just the ones enabled in the static YAML `alert.*` floor.
//
// LISTING a runtime channel is a different job from RESOLVING it for a send:
// listing must never open a channel's sealed secret and must key off the
// REQUEST org. A consumer registers a lister that reports the effective enabled
// channels from the masked runtime view (the same source the alert-fatigue
// picker uses), so the two pickers stay consistent. OSS registers none, so the
// capabilities handler falls back to the static enabled-channel list and
// single-tenant behaviour is byte-for-byte unchanged.
//
// It is the direct sibling of config.AlertConfigResolver / SetAlertConfigResolver:
// one process-wide slot, registered once at boot, mutex-guarded.

// ReportChannelLister lists the notification channels to offer in the incident
// report channel picker for the request's org.
//
// Contract:
//   - EnabledAlertChannels returns the channel names (e.g. "slack", "telegram")
//     to offer for org, in a stable order. It MUST reflect the effective enable
//     (runtime override overlaid on the YAML floor) without opening any channel
//     secret — listing never decrypts.
//   - ConfiguredDisabledChannels returns the channel names that are configured
//     (a runtime override exists) but currently disabled, so NOT in
//     EnabledAlertChannels, in a stable order. It lets the report UI tell the
//     operator a channel is set up but turned OFF, instead of silently omitting
//     it. Same no-decrypt, REQUEST-org rule as EnabledAlertChannels.
//   - org is the REQUEST org (middleware.OrgFromContext), never a boot-pinned or
//     caller-supplied value; ctx carries the request context.
//   - Both methods return a non-nil (possibly empty) slice.
type ReportChannelLister interface {
	EnabledAlertChannels(ctx context.Context, org string) []string
	ConfiguredDisabledChannels(ctx context.Context, org string) []string
}

// Process-wide single slot. A consumer registers a lister at boot; the
// capabilities handler reads it once per request. Mutex-guarded so a boot-time
// registration is safely visible to the HTTP handler goroutines.
var (
	reportChannelListerMu   sync.Mutex
	reportChannelListerSlot ReportChannelLister
)

// SetReportChannelLister registers the lister the report channel picker consults
// for its offered-channel list. Last-wins: a second call replaces the first.
// Passing nil clears the slot (back to the static enabled-channel fallback). OSS
// ships none, so the picker uses the static `alert.*` enabled list unchanged.
// This is the entry point the enterprise wiring attaches to (mirror of
// config.SetAlertConfigResolver). Call at boot.
func SetReportChannelLister(l ReportChannelLister) {
	reportChannelListerMu.Lock()
	defer reportChannelListerMu.Unlock()
	reportChannelListerSlot = l
}

// reportChannelLister returns the registered lister, or nil when none is set
// (community mode).
func reportChannelLister() ReportChannelLister {
	reportChannelListerMu.Lock()
	defer reportChannelListerMu.Unlock()
	return reportChannelListerSlot
}
