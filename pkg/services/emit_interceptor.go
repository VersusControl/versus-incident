// emit_interceptor.go — a generic, unopinionated emit-interception seam
// (companion to the admin_audit.go audit seam and the extension.go org seams).
//
// Every alert — agent findings AND raw webhooks — funnels through
// CreateIncident. This seam lets an external wrapper (the enterprise build)
// inspect a finding BEFORE it becomes a page and decide to divert it to another
// channel set, or hold it back entirely. OSS ships NO interceptor, so every
// call is a community-mode no-op: the decision is always EmitProceed and an
// untouched OSS binary pages exactly as before. There is no dedup, grouping, or
// configuration in the open-core tree — only the hook and a stable fingerprint
// the wrapper reuses as its own dedup key.
//
// Registration is process-wide and expected to happen once at boot, before the
// server starts accepting connections.
package services

import (
	"crypto/sha256"
	"encoding/hex"
	"log"
	"strings"
	"sync/atomic"

	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/utils"
)

// EmitAction is the interceptor's verdict for one finding.
type EmitAction int

const (
	// EmitProceed pages as normal — the default and the fail-open verdict.
	EmitProceed EmitAction = iota
	// EmitDivert pages, but to DivertChannels instead of the primary channels.
	EmitDivert
	// EmitSuppress does NOT page the primary channels (the wrapper already
	// recorded the finding in its own store).
	EmitSuppress
	// EmitGroup does NOT page — attach to a parent. Reserved for a later phase.
	EmitGroup
	// EmitDelay does NOT page now — hold for a digest. Reserved for a later
	// phase.
	EmitDelay
)

// EmitDecision carries the verdict plus its auditable reason. DivertChannels is
// set only for an EmitDivert that OSS should deliver. For detection incidents,
// DeliveryHandled marks an EmitDivert whose external interceptor already
// attempted delivery, so OSS must not run the incident lifecycle a second time.
// Non-detection callers ignore DeliveryHandled and fail open through the normal
// webhook lifecycle. DeliveryError reports the external attempt's failure
// without exposing provider details in the action or reason.
type EmitDecision struct {
	Action          EmitAction
	Reason          string
	DivertChannels  []string
	DeliveryHandled bool
	DeliveryError   error
}

// EmitInterceptor decides what happens to a finding before it becomes an
// incident. It is ctx-less like CreateIncident; the fingerprint is derived from
// the already-redacted content map. OSS ships none (community no-op).
type EmitInterceptor func(content map[string]interface{}, teamID string) EmitDecision

// emitInterceptorSlot holds the registered interceptor. It uses atomic.Value so
// a boot-time registration is safely visible to request goroutines without a
// data race.
var emitInterceptorSlot atomic.Value // EmitInterceptor

// SetEmitInterceptor registers the interceptor consulted for every emitted
// finding. OSS ships none (the slot is a no-op). Passing nil clears it (back to
// the community no-op). Last registration wins. Call at boot.
func SetEmitInterceptor(i EmitInterceptor) {
	if i == nil {
		emitInterceptorSlot = atomic.Value{}
		return
	}
	emitInterceptorSlot.Store(i)
}

// resolveEmitDecision consults the registered interceptor for a finding. With
// no interceptor registered (community mode) the verdict is always EmitProceed.
// A panic in the interceptor is recovered and treated as EmitProceed — a noisy
// interceptor must never drop a page (fail-open).
func resolveEmitDecision(content map[string]interface{}, teamID string) (d EmitDecision) {
	i, _ := emitInterceptorSlot.Load().(EmitInterceptor)
	if i == nil {
		return EmitDecision{Action: EmitProceed}
	}
	defer func() {
		if r := recover(); r != nil {
			log.Printf("incident: emit interceptor panicked, failing open (page): %v", r)
			d = EmitDecision{Action: EmitProceed}
		}
	}()
	return i(content, teamID)
}

// applyEmitRouting selects which providers a decision fans out to. EmitDivert
// restricts the fan-out to the named channels; every other action (EmitProceed
// and any unknown value) returns the full built set unchanged (fail-open).
func applyEmitRouting(decision EmitDecision, providers []core.AlertProvider) []core.AlertProvider {
	if decision.Action == EmitDivert {
		return filterProvidersByChannel(providers, decision.DivertChannels)
	}
	return providers
}

// filterProvidersByChannel keeps only the providers whose channel name appears
// in names, matched on the provider's own Name() (slack/telegram/viber/email/
// msteams/lark). An empty names set yields no providers.
func filterProvidersByChannel(providers []core.AlertProvider, names []string) []core.AlertProvider {
	want := make(map[string]struct{}, len(names))
	for _, n := range names {
		want[strings.ToLower(strings.TrimSpace(n))] = struct{}{}
	}
	var out []core.AlertProvider
	for _, p := range providers {
		if _, ok := want[strings.ToLower(p.Name())]; ok {
			out = append(out, p)
		}
	}
	return out
}

// EmitFingerprint builds a stable, redaction-safe dedup key from an
// already-redacted content map: Source + ServiceName/Service + PatternID +
// Severity. When PatternID is absent (e.g. a webhook incident) it falls back to
// a normalized-title hash so distinct titles still separate. All four
// components are read through the shared extraction, so a payload is grouped on
// the same values it is displayed under, and all four come off ONE folded view
// of the payload, so a wide payload is scanned once rather than once per
// component. That shared accessor also resolves a payload that carries two
// spellings of a key the same way on every read, so one payload can never split
// into two dedup buckets. It reads only values already present in the content
// map — never a raw payload — so the enterprise wrapper can reuse the exact same
// key as its own store key.
//
// Every component is escaped before it is joined, so a separator inside a
// component can never be read as a component boundary. Without that, an alert
// whose service label legitimately carries the separator ("checkout|eu") could
// be landed on by a crafted PatternID ("eu|P42"), which shifts the boundary and
// silences the victim through its own dedup bucket.
//
// Operator note: escaping the components, moving the title hash to SHA-256, and
// resolving source/pattern through the shared accessor (which trims and folds
// duplicate spellings) re-key existing fingerprints once. They ship together
// with the AlarmName title change so operators absorb ONE combined re-key:
// fatigue rows written under the old keys stop matching, new rows are created,
// and the stale rows age out on their normal retention.
//
// A caller that already holds a utils.FoldedPayload should call
// EmitFingerprintFrom instead, so the emit folds once rather than twice.
func EmitFingerprint(content map[string]interface{}) string {
	return EmitFingerprintFrom(utils.NewFoldedPayload(content))
}

// EmitFingerprintFrom is EmitFingerprint for a caller that already holds a
// folded view of the payload. Building the fold is the expensive half of an
// emit, so an interceptor that reads service/severity/source off its own
// utils.FoldedPayload passes that payload here and the whole emit path folds
// exactly once instead of twice. Both entry points compose the identical key
// for the same content — EmitFingerprint is a thin wrapper that builds a
// payload and delegates — so a store can mix keys from either without
// splitting a dedup bucket. A nil payload yields the all-empty key, matching
// EmitFingerprint(nil).
func EmitFingerprintFrom(payload *utils.FoldedPayload) string {
	if payload == nil {
		payload = utils.NewFoldedPayload(nil)
	}

	source := payload.Source()
	service := payload.Service()
	pattern := payload.PatternID()
	severity := payload.Severity()

	if pattern == "" {
		if title := payload.Title(); title != "" {
			pattern = "t:" + hashNormalized(title)
		}
	}

	parts := []string{source, service, pattern, severity}
	for i, p := range parts {
		parts[i] = escapeFingerprintComponent(p)
	}
	return strings.Join(parts, string(fingerprintSeparator))
}

const (
	// fingerprintSeparator joins the fingerprint's escaped components.
	fingerprintSeparator = '|'
	// fingerprintEscape prefixes a separator, or itself, inside a component.
	fingerprintEscape = '\\'
	// titleHashHexLen is the hex width of the title hash: 32 chars, the leading
	// 128 bits of the SHA-256 digest. Fixed-width hex keeps the component the
	// same shape for any store or index keyed on it.
	titleHashHexLen = 32
)

// escapeFingerprintComponent leaves a component free of unescaped separators.
// The encoding is injective, so two payloads share a fingerprint only when
// their components are equal one for one.
func escapeFingerprintComponent(s string) string {
	if !strings.ContainsRune(s, fingerprintSeparator) && !strings.ContainsRune(s, fingerprintEscape) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 8)
	for _, r := range s {
		if r == fingerprintSeparator || r == fingerprintEscape {
			b.WriteRune(fingerprintEscape)
		}
		b.WriteRune(r)
	}
	return b.String()
}

// hashNormalized lowercases, trims, and collapses internal whitespace before
// hashing, so cosmetically different renderings of the same title fold to one
// key. The digest is SHA-256 truncated to titleHashHexLen: this hash names a
// dedup bucket, so an attacker must not be able to construct a title that lands
// in someone else's — a promise a non-cryptographic hash cannot make.
func hashNormalized(s string) string {
	norm := strings.ToLower(strings.Join(strings.Fields(s), " "))
	sum := sha256.Sum256([]byte(norm))
	return hex.EncodeToString(sum[:titleHashHexLen/2])
}
