package services

import (
	"context"
	"log"
	"time"

	"github.com/VersusControl/versus-incident/pkg/scheduler"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// report_scheduler.go — the runtime loop that drives the recurring daily
// incident digest. It is the single shared starter both the OSS entrypoint
// and the enterprise entrypoint call, so the ticker cadence, the live
// settings re-read, the single-owner gate, and the send path live in ONE
// place instead of being mirrored per binary. The pure "should it fire?"
// decision stays in report_schedule.go (ReportSendDue); this file owns only
// timing, ownership gating, and delivery.

// ReportScheduleJobName is the ownership key the daily digest gates on. Under
// enterprise HA the cluster identity installs a predicate via
// scheduler.SetOwnership; keyed by this name, exactly one replica sends. OSS
// installs no predicate (scheduler.Owns is a constant true), so a single-
// instance deployment always sends. Both entrypoints reference this SAME
// constant so the two binaries gate on an identical key.
const ReportScheduleJobName = "report-daily-digest"

// StartReportScheduler drives the recurring daily incident digest from a
// single 1-minute ticker bound to ctx. Each tick it RE-READS the report
// settings (so an operator's change to the send time / timezone / channel
// applies live, no restart) and asks the pure ReportSendDue predicate whether
// the digest is due right now. When it is, it sends the DefaultWindow report
// to the DefaultChannel via the same SendIncidentsReport path the manual
// button uses — which enforces the enable flag, the rate limiter, and
// redaction.
//
// HA / multi-instance caveat: the SEND is gated behind scheduler.Owns so that
// under enterprise HA (where cluster.Identity installs an ownership predicate
// via scheduler.SetOwnership) exactly one replica fires the digest. OSS
// installs no predicate, so scheduler.Owns is always true and a single-
// instance deployment always sends; if an operator runs multiple replicas
// WITHOUT installing an ownership predicate, each replica would send its own
// copy — the same single-owner caveat that applies to every scheduled job.
func StartReportScheduler(ctx context.Context, store storage.Provider) {
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()

		// lastSent is the wall-clock moment of the last successful send; the
		// due predicate uses it to fire at most once per local day.
		var lastSent time.Time

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}

			settings := LoadReportSettings(store)
			now := time.Now()
			if !ReportSendDue(now, settings, lastSent) {
				continue
			}
			// Single-owner gate (no-op in OSS single-instance).
			if !scheduler.Owns(ReportScheduleJobName) {
				continue
			}
			// Nil-safe: without a renderer installed the send would fail with
			// ErrReportNoRenderer every tick; skip quietly instead.
			if ReportRenderer() == nil {
				continue
			}

			out, err := SendIncidentsReport(ctx, ReportSendOptions{
				Window:      settings.DefaultWindow,
				RequestedBy: "scheduler",
			})
			if err != nil {
				// Do NOT advance lastSent on failure — the next tick within
				// the same minute retries; the once-per-day guard only kicks
				// in after a success.
				log.Printf("report scheduler: daily digest send failed: %v", err)
				continue
			}
			lastSent = now
			log.Printf("report scheduler: daily digest sent window=%s channel=%q sent=%v fallback=%v failed=%d",
				settings.DefaultWindow, settings.DefaultChannel, out.Sent, out.Fallback, len(out.Failed))
		}
	}()
}
