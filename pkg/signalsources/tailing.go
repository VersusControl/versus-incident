package signalsources

import "time"

// -----------------------------------------------------------------------------
// Tailing cursor convention (shared OSS seam).
//
// Every cursor-driven SignalSource derives its next poll cursor from the MAX
// document timestamp it observed this tick, and every subsequent tick queries
// `>= cursor` (offset by an optional reorder window). That timestamp is
// UNTRUSTED DATA: it is whatever the producer stamped on the record. A single
// document dated in the future — a bad client clock, a mis-configured shipper,
// injected/garbage data — would otherwise push the cursor far ahead of the
// wall clock, after which every `>= cursor` query matches nothing until real
// time catches up. The source "learns the first batch then stops pulling",
// and only wiping the cursor (Clear-all) makes it briefly resume before
// stranding again. Observed live on a real cluster: docs dated 2048 pinned the
// cursor there while present-day logs were silently skipped.
//
// The invariant that keeps a tail honest, shared by every source that does NOT
// exhibit the bug (Loki, Graylog, Splunk, the enterprise standing sources):
// the scan is upper-bounded at `now` and the cursor never advances past `now`.
// ClampCursor is the single, testable realization of the second half so the
// affected sources (Elasticsearch, CloudWatch Logs) enforce it identically,
// and so any future source — OSS or enterprise — can adopt one convention.
// -----------------------------------------------------------------------------

// ClampCursor bounds a tailing source's next cursor to the closed interval
// [since, now]:
//
//   - it never rewinds below `since` (the lower bound the worker asked for, so
//     an empty or all-older tick reports "still here" rather than moving back);
//   - it never advances beyond `now` (the wall clock at pull time), so an
//     untrusted future-dated document cannot strand the cursor ahead of real
//     time and blank every following query.
//
// `now` should be the same clock reading the source used to upper-bound its
// scan window, so the returned cursor and the query stay consistent within the
// tick. When `since` is itself already in the future (a cursor persisted before
// this convention existed), the result collapses to `now`, letting the next
// tick resume real tailing instead of querying an empty future window forever.
func ClampCursor(candidate, since, now time.Time) time.Time {
	if candidate.Before(since) {
		candidate = since
	}
	if candidate.After(now) {
		candidate = now
	}
	return candidate
}
