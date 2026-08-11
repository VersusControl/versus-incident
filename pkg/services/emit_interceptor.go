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
	"fmt"
	"hash/fnv"
	"log"
	"strings"
	"sync/atomic"

	"github.com/VersusControl/versus-incident/pkg/core"
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
// set only for EmitDivert. ParentID / DelayUntil are reserved for later phases.
type EmitDecision struct {
	Action         EmitAction
	Reason         string
	DivertChannels []string
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
// a normalized-title hash so distinct titles still separate. It reads only
// values already present in the content map — never a raw payload — so the
// enterprise wrapper can reuse the exact same key as its own store key.
func EmitFingerprint(content map[string]interface{}) string {
	source := firstString(content, "source")
	service := extractService(content)
	pattern := firstString(content, "patternid", "pattern_id", "key")
	severity := firstString(content, "severity")

	if pattern == "" {
		if title := firstString(content, "title", "alertname", "summary", "subject", "name"); title != "" {
			pattern = "t:" + hashNormalized(title)
		}
	}

	return strings.Join([]string{source, service, pattern, severity}, "|")
}

// hashNormalized lowercases, trims, and collapses internal whitespace before
// hashing, so cosmetically different renderings of the same title fold to one
// key.
func hashNormalized(s string) string {
	norm := strings.ToLower(strings.Join(strings.Fields(s), " "))
	h := fnv.New64a()
	_, _ = h.Write([]byte(norm))
	return fmt.Sprintf("%016x", h.Sum64())
}
