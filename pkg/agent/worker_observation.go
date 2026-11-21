package agent

import (
	"context"
	"errors"
	"log"

	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// handleObservation runs the mode-specific tail for one ALREADY-FOLDED
// observation: training logs discovery only; shadow/detect classify against the
// pre-fold snapshot (mean/std/confident) and sink (shadow → NDJSON, detect →
// AI + emit). It performs NO promotion — the caller runs promoteByCount AFTER
// this returns, so the detector's Classify reads the pre-fold verdict before
// promotion persists it. Early-returns replace the old inline `continue`s so
// the caller's promotion step still runs on the grace / not-confident / known
// suppressed paths.
func (w *Worker) handleObservation(
	ctx context.Context,
	mode string,
	src core.SignalSource,
	o core.Observation,
	mean, std float64,
	confident bool,
	detector core.SignalDetector,
	verdicts map[string]int,
) (core.TypedVerdict, bool, bool) {
	switch mode {
	case "training":
		// Pure observation. No verdict, no incident.
		verdicts["learned"]++
		if o.IsNew {
			log.Printf("%sagent: new pattern %q (source=%s tag=%q) → %q%s",
				colorGreen, o.Key, src.Name(), w.shadowTag(o), truncateString(o.Signal, 120), colorReset)
		}
		return core.TypedVerdict{}, false, false

	case "shadow", "detect":
		// New-service grace is shared by shadow and detect: shadow is meant
		// to be a faithful preview of detect, so whatever detect filters
		// out, shadow filters too. During grace the signal is learned
		// (folded above) but not surfaced as a "would alert" / AI candidate.
		if w.newServiceGrace > 0 &&
			w.catalog.IsServiceInGrace(o.Service, w.newServiceGrace) {
			verdicts["grace"]++
			if o.IsNew {
				log.Printf("%sagent[%s]: new pattern %q (service=%q in grace, learning only) → %q%s",
					colorGreen, mode, o.Key, o.Service, truncateString(o.Signal, 120), colorReset)
			}
			return core.TypedVerdict{}, false, true
		}

		v := detector.Classify(o, mean, std, confident)
		verdicts[v.Class.String()]++

		// Per-key training gate: a model that is not yet confident for this
		// key suppresses in EVERY mode. Logs are always confident, so this
		// is a no-op for the OSS log brain; it is what lets a metric/trace
		// brain keep learning a fresh key without firing.
		if !v.Confident {
			return v, true, false
		}
		// A known, non-spiking pattern is normal — nothing to surface.
		if v.Class == core.VerdictKnownPattern {
			if mode == "detect" {
				w.recordKnownDetectionContinuation(src.Name(), detector.Kind(), o)
			}
			return v, true, false
		}

		// Mode-specific sink: shadow records to NDJSON; detect calls
		// the AI SRE and emits an incident.
		if mode == "shadow" {
			sample := ""
			if len(o.Samples) > 0 {
				sample = o.Samples[0].Message
			}
			if w.shadow != nil {
				w.shadow.Record(src.Name(), o.Key, o.Service, o.Signal, sample,
					w.shadowTag(o), v.Class.String(), o.Frequency)
			}
			log.Printf("%sagent[shadow]: would alert pattern=%q service=%q tag=%q verdict=%s freq=%d%s",
				colorGreen, o.Key, o.Service, w.shadowTag(o), v.Class, o.Frequency, colorReset)
		} else {
			outcome := w.emitDetect(ctx, src.Name(), detector.Kind(), o.Key, o.Signal, o.Service, o.Frequency, o.Samples, v.Class, v.Baseline, v.Score, std, v.Reason, observationSeverity(o))
			verdicts["emit_"+outcome]++
		}
		return v, true, false

	default:
		log.Printf("agent: unknown mode=%q, treating as training", mode)
		verdicts["learned"]++
		return core.TypedVerdict{}, false, false
	}
}

func (w *Worker) recordKnownDetectionContinuation(source, signalKind string, observation core.Observation) {
	if w.continueDetectionEpisode == nil || observation.Frequency <= 0 {
		return
	}
	decision, err := w.continueDetectionEpisode(storage.DetectionOccurrence{
		Identity: storage.DetectionIdentity{
			AgentKind: "detect", Source: source, Service: observation.Service,
			SignalKind: signalKind, ConditionKey: observation.Key,
		},
		Frequency: int64(observation.Frequency), Severity: observationSeverity(observation),
		ReceivedAt: observation.Timestamp, ExistingOnly: true,
	})
	if errors.Is(err, storage.ErrNotFound) || errors.Is(err, storage.ErrUnsupported) {
		return
	}
	event := &DetectEvent{
		Source: source, PatternID: observation.Key, Service: observation.Service,
		Verdict: core.VerdictKnownPattern.String(), Frequency: observation.Frequency,
		Timestamp: observation.Timestamp,
	}
	emission := &core.DetectionEmissionResult{NotificationOutcome: "not_applicable"}
	if err != nil {
		message := err.Error()
		if len(message) > 256 {
			message = message[:256]
		}
		log.Printf("agent: episode_error operation=continue_known error=%q", message)
		emission.EpisodeAction = "episode_error"
		emission.EpisodeError = message
	} else {
		setDetectionEmissionFromDecision(emission, decision)
	}
	w.recordDetectEvent(event, emission)
}

func setDetectionEmissionFromDecision(emission *core.DetectionEmissionResult, decision storage.DetectionEpisodeDecision) {
	emission.Fingerprint = decision.Fingerprint
	emission.EpisodeID = decision.EpisodeID
	emission.IncidentID = decision.IncidentID
	emission.OccurrenceDelta = decision.OccurrenceDelta
	emission.OccurrenceCount = decision.OccurrenceCount
	emission.EpisodeAction = string(decision.Action)
}

// signalPromoter is an OPTIONAL capability a brain may implement to persist a
// count-based auto-promotion on the LEARN path, after a tick has folded. The
// worker calls it via promoteByCount once per observation in EVERY mode, so a
// pattern that crosses its promotion threshold is marked in training exactly as
// it is in shadow/detect. Brains that own their own promotion (the enterprise
// metric/trace brains) simply do not implement it, so the seam stays generic.
type signalPromoter interface {
	// Promote persists a count-based promotion for key if it has just crossed
	// the threshold. It must be idempotent (a no-op once already promoted) and
	// safe to call every tick.
	Promote(key string)
}

// promoteByCount runs the learn-path promotion for brains that opt into the
// signalPromoter seam (the OSS log brain). It is a no-op for brains without it,
// keeping the worker type-agnostic. Called AFTER the mode tail so the detector
// has already read the pre-fold verdict this tick.
func promoteByCount(learner core.SignalLearner, key string) {
	if p, ok := learner.(signalPromoter); ok {
		p.Promote(key)
	}
}
