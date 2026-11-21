package agent

import (
	"context"
	"errors"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/scheduler"
	"github.com/VersusControl/versus-incident/pkg/signalsources"
)

// tick runs one poll across every source. Errors from one source never
// affect the others — the worker keeps moving. The effective lifecycle
// mode is resolved once here, at the top of the cycle, so every source in
// this tick runs under one coherent mode and a hot-switch takes effect on
// the next cycle.
func (w *Worker) tick(ctx context.Context) {
	mode := w.effectiveMode(ctx)
	var wg sync.WaitGroup
	for _, src := range w.sources {
		wg.Add(1)
		go func(s core.SignalSource) {
			defer wg.Done()
			w.tickSource(ctx, s, mode)
		}(src)
	}
	wg.Wait()
}

func (w *Worker) tickSource(ctx context.Context, src core.SignalSource, mode string) {
	since := w.loadCursor(ctx, src.Name())

	signals, newCursor, err := src.Pull(ctx, since)
	if w.healthRecorder != nil && scheduler.Owns("service-health") && w.sourceKind(src.Name()) == signalsources.KindLogs {
		if recordErr := w.healthRecorder.RecordSourceHealth(ctx, core.SourceHealthObservation{
			OrgID: w.orgID, SourceID: src.Name(), AttemptedAt: time.Now().UTC(),
			Succeeded: err == nil, ErrorClass: sourceHealthErrorClass(err),
		}); recordErr != nil {
			log.Printf("agent: recording source health for %s failed: %v", src.Name(), recordErr)
		}
	}
	if w.ai.ObserveSourceHealth != nil {
		w.ai.ObserveSourceHealth(src.Name(), err, time.Now())
	}
	if err != nil {
		log.Printf("agent: pull from %s failed: %v", src.Name(), err)
		return
	}
	if len(signals) == 0 {
		// Persist a legitimately-advanced cursor even though this tick emitted
		// nothing. A source that scanned a window and found no signals reports
		// newCursor > since to say "I am caught up to here"; dropping it would
		// strand the cursor and force the next tick to re-scan the same empty
		// range forever. This never skips unprocessed data: by the SignalSource
		// contract newCursor is the max timestamp the source actually scanned,
		// so anything after it is still pulled next tick. The OSS query sources
		// return newCursor == since on an empty pull, so this is a no-op for
		// them; it only bites a source that advances its cursor without emitting.
		if newCursor.After(since) {
			w.saveCursor(ctx, src.Name(), newCursor)
		}
		return
	}

	batchSize := len(signals)
	if w.cfg.BatchMax > 0 && batchSize > w.cfg.BatchMax {
		batchSize = w.cfg.BatchMax
	}
	for start := 0; start < len(signals); start += batchSize {
		end := min(start+batchSize, len(signals))
		if !w.processSourceBatch(ctx, src, mode, signals[start:end], since, newCursor) {
			return
		}
	}
	w.saveCursor(ctx, src.Name(), newCursor)
}

func (w *Worker) processSourceBatch(ctx context.Context, src core.SignalSource, mode string, signals []core.Signal, since, newCursor time.Time) bool {

	// Redact every payload before doing anything else with it.
	for i := range signals {
		if w.redactor != nil {
			signals[i].Message = w.redactor.Scrub(signals[i].Message)
			signals[i].Fields = w.redactor.ScrubFields(signals[i].Fields)
			// Raw is intentionally left alone — operators never see it
			// outside admin debug; redacting it would hide useful structure.
		}
	}

	// Learn-exclusion chokepoint ("Disable-Learn"). Runs immediately after
	// redaction and BEFORE the pre-brain matcher / grouping, so an excluded
	// (service, signal) never folds into the model in ANY mode — the mode
	// switch (training/shadow/detect) is downstream of learner.Group. OSS
	// installs no policy ⇒ learnExclusion() is nil ⇒ this block is skipped and
	// the tick is byte-for-byte unchanged.
	excluded := 0
	if x := learnExclusion(); x != nil {
		kept := signals[:0]
		for _, sig := range signals {
			svc, _ := sig.Fields[core.FieldService].(string)
			if svc == "" {
				svc = w.services.Extract(sig.Message)
				if svc == "" {
					svc = "_unknown"
				}
			}
			signalName, _ := sig.Fields[core.FieldSignal].(string)
			if x.ExcludeFromLearning(ctx, svc, signalName) {
				excluded++
				continue
			}
			kept = append(kept, sig)
		}
		signals = kept
	}

	// Per-kind override (agent.regex.metrics / agent.regex.traces):
	// when the operator set it, drop metric/trace signals whose message does NOT
	// match BEFORE the brain sees them, so the override bites brain-agnostically
	// — including a licensed source bound to its enterprise brain, which never
	// builds a log brain. Logs are never pre-filtered here (the log brain owns
	// their default_pattern + rules); an unset override ⇒ nil matcher ⇒ all
	// signals flow (learn-all), unchanged. pulled keeps the pre-filter count so
	// the tick's skipped_no_match still reports the dropped signals.
	pulled := len(signals)
	if pf := w.preBrainMatcher(src.Name()); pf != nil {
		kept := signals[:0]
		for _, sig := range signals {
			if pf.Match(sig.Message).Matched() {
				kept = append(kept, sig)
			}
		}
		signals = kept
	}

	// Group by pattern within this tick so we can update the EWMA with
	// per-pattern frequency, not per-signal.
	//
	// The keying, learning and scoring are owned by the source's per-type
	// brain (the SignalLearner + SignalDetector). In OSS every source uses the
	// built-in log brain (drain-miner catalog + frequency-novelty classifier);
	// when an enterprise metric/trace brain is registered for the source's
	// type it plugs into this exact lifecycle. The worker stays type-agnostic.
	learner, detector := w.brainFor(src.Name())

	// The read range lets the log brain measure each pattern's own span of
	// source time, so a catch-up read folds at the real rate (see readSpan).
	observations, err := learner.Group(withReadRange(ctx, since, newCursor), signals)
	if err != nil {
		log.Printf("agent: grouping signals from %s failed: %v", src.Name(), err)
		return false
	}

	// Per-log-pattern learn-exclusion. The pre-Group chokepoint above can
	// only see a signal's service + signal name; a plain log line carries no
	// pattern identity there, so a single noisy LOG PATTERN could not be held
	// out without excluding its whole service. Now that Group has assigned each
	// observation its stable pattern Key, consult the resolver once per
	// observation and drop the excluded PATTERNS before Learn — the log analogue
	// of the metric/trace per-signal exclusion. LOG brains only: metric/trace
	// signal exclusion already bit at the pre-Group hook, so re-checking their
	// keys here would be redundant (and their Key is not a log pattern id). A
	// nil resolver (community/OSS) is never installed, so this block is skipped
	// and the tick is byte-for-byte unchanged. Excluded observations' signal
	// frequency is booked as excluded (not as no-match) so the tick stat stays
	// honest.
	excludedPatternFreq := 0
	if x := learnExclusion(); x != nil && learner.Kind() == "logs" {
		kept := observations[:0]
		for _, o := range observations {
			if x.ExcludeLogPattern(ctx, o.Service, o.Key) {
				excludedPatternFreq += o.Frequency
				continue
			}
			kept = append(kept, o)
		}
		observations = kept
	}

	// Update the model and produce verdicts, one observation at a time.
	verdicts := make(map[string]int) // verdict-name → count, for stats
	matched := 0                     // signals that landed in an observation
	for _, o := range observations {
		matched += o.Frequency

		// Track the service so the grace window starts even in training mode.
		if o.Service != "" && o.Service != "_unknown" {
			if w.catalog.RegisterService(o.Service) {
				log.Printf("agent: new service discovered: %q", o.Service)
			}
		}

		// Snapshot the learned expectation BEFORE folding this tick so a
		// deviation is judged against the prior baseline rather than the
		// freshly-smoothed value (otherwise a spike would smear into the EWMA
		// before we ever classify it). Then fold the tick into the model.
		mean, std, confident := learner.Expected(ctx, o.Key, o.Timestamp)
		if err := learner.Learn(ctx, []core.Observation{o}); err != nil {
			log.Printf("agent: learning key=%q from %s failed: %v", o.Key, src.Name(), err)
			continue
		}

		// Mode-specific tail: training logs discovery only; shadow/detect
		// classify against the pre-fold snapshot and sink. Split into its own
		// method (with early-returns for grace/known/suppressed) so the
		// promotion step below always runs, on every path, in every mode.
		verdict, classified, inGrace := w.handleObservation(ctx, mode, src, o, mean, std, confident, detector, verdicts)
		if w.healthRecorder != nil && scheduler.Owns("service-health") && learner.Kind() == "logs" {
			projection := core.LogHealthObservation{
				OrgID: w.orgID, SourceID: src.Name(), Service: o.Service,
				PatternID: o.Key, ObservedAt: o.Timestamp, Frequency: o.Frequency,
				StrongestSeverity: o.StrongestSeverity, NewPattern: o.IsNew, InGrace: inGrace,
				ExpectedRate: mean, ExpectedSpread: std, LifecycleClassified: classified,
			}
			if classified {
				projection.LifecycleClass = verdict.Class.String()
			}
			if err := w.healthRecorder.RecordLogHealth(ctx, projection); err != nil {
				log.Printf("agent: recording log health fold from %s failed: %v", src.Name(), err)
				if statusErr := w.healthRecorder.RecordSourceHealth(ctx, core.SourceHealthObservation{
					OrgID: w.orgID, SourceID: src.Name(), AttemptedAt: time.Now().UTC(),
					Succeeded: false, ErrorClass: "unavailable",
				}); statusErr != nil {
					log.Printf("agent: recording degraded health projection for %s failed: %v", src.Name(), statusErr)
				}
			}
		}

		// Auto-promotion runs on the LEARN path, once per folded observation, in
		// EVERY mode — including training, which never calls Classify. It is
		// sequenced AFTER handleObservation so any Classify has already consumed
		// the pre-fold verdict this tick; a pattern that just crossed
		// auto_promote_after now has its stored Verdict persisted to "known", so
		// the Verdict column and the readiness "To known" column agree in every
		// mode. Brains that own their own promotion (the enterprise metric/trace
		// brains) do not implement the seam and are a no-op here.
		promoteByCount(learner, o.Key)
	}

	log.Printf("agent: tick %s signals=%d matched=%d patterns=%d skipped_no_match=%d skipped_excluded=%d verdicts=%v cursor=%s",
		src.Name(), pulled, matched, len(observations), pulled-matched-excludedPatternFreq, excluded+excludedPatternFreq, verdicts, newCursor.Format(time.RFC3339))
	return true
}

func (w *Worker) sourceKind(name string) signalsources.Kind {
	if kind := w.kindByName[name]; kind != "" {
		return kind
	}
	return signalsources.KindUnknown
}

func sourceHealthErrorClass(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return "timeout"
	}
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "unauthorized"), strings.Contains(message, "authentication"):
		return "authentication"
	case strings.Contains(message, "forbidden"), strings.Contains(message, "permission"):
		return "permission"
	case strings.Contains(message, "rate limit"), strings.Contains(message, "too many requests"):
		return "rate_limit"
	case strings.Contains(message, "connect"), strings.Contains(message, "network"):
		return "connection"
	default:
		return "unavailable"
	}
}
