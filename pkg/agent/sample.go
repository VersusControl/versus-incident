package agent

import (
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/utils"
)

// Per-pattern redacted raw-sample store (see
// plans/productization/sre/pattern-raw-sample-store-design.md).
//
// Every learned pattern/signal keeps a bounded ring of the most recent
// POST-REDACTION examples it was learned from, so an operator can see what the
// raw signal looked like and the AI gets a concrete example. The ring value
// type + push helper live here in OSS so the enterprise metric/trace brains can
// import them and stay consistent without a fork; OSS never imports enterprise.

const (
	// SampleRingCap is the maximum number of redacted examples kept per
	// pattern/signal (drop-oldest past the cap). The ring stores oldest→newest,
	// so the LATEST example is ring[len(ring)-1].
	SampleRingCap = 10
	// SampleMaxLen caps each stored line (in bytes) so the ring can never bloat
	// the whole-blob catalog. Worst case per pattern is
	// SampleRingCap × SampleMaxLen ≈ 5 KB. It is the one knob to turn if a
	// deployment proves the ring heavy — no schema change.
	SampleMaxLen = 512
)

// PushSample scrubs sample (defence-in-depth), one-lines + truncates it to
// SampleMaxLen, appends it to ring, and trims ring to the last SampleRingCap
// entries (drop-oldest, order oldest→newest).
//
// scrub is applied FIRST so it sees the whole line — a planted secret can never
// survive to storage even if a future refactor composed the sample from a
// not-yet-scrubbed source — and the truncate runs AFTER so SampleMaxLen stays a
// hard ceiling even when a placeholder (e.g. <REDACTED:basic_auth>) is longer
// than the secret it replaced. scrub MAY be nil (input already redacted); when
// non-nil it is ALWAYS applied. An empty result is not appended.
//
// This is the generic seam the enterprise metric/trace brains import to keep
// their per-signal ring identical in shape to the OSS log ring.
func PushSample(ring []string, sample string, scrub core.Scrubber) []string {
	if scrub != nil {
		sample = scrub.Scrub(sample)
	}
	sample = utils.OneLine(sample, SampleMaxLen)
	if sample == "" {
		return ring
	}
	ring = append(ring, sample)
	if len(ring) > SampleRingCap {
		// Return an independent, exactly-capped slice so the trimmed ring never
		// shares a backing array with a caller's prior view (e.g. a Catalog.Get
		// copy) and never carries stale leading capacity.
		ring = append([]string(nil), ring[len(ring)-SampleRingCap:]...)
	}
	return ring
}
