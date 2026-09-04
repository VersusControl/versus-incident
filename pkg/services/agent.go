package services

import (
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// CreateIncidentFromFinding maps an AI SRE finding into the standard
// content map and delegates to CreateIncident so all existing channels
// (Slack, Telegram, MS Teams, Lark, Email, Viber) and the on-call
// workflow trigger with no per-channel changes.
//
// Source identifies the agent + signal source (e.g. "agent:elasticsearch:prod-app").
// Service is the operator-facing service name extracted from the log
// pattern; "_unknown" when no service_pattern matched.
//
// All keys added here are stable and additive — existing template files
// keep working unchanged. Operators who want to surface AI-specific
// detail can reference {{ .Summary }}, {{ .Suggestions }},
// {{ .Confidence }}, {{ .PatternID }}, {{ .Verdict }}.
func CreateIncidentFromFinding(f *core.AIFinding, r core.AgentResult, source, service string) error {
	return createIncidentFromFindingWithLease(f, r, source, service, storage.DefaultDetectionNotificationLease)
}

func createIncidentFromFindingWithLease(f *core.AIFinding, r core.AgentResult, source, service string, claimLease time.Duration) error {
	if f == nil {
		return nil
	}
	if service == "" {
		service = "_unknown"
	}

	// First sample message, if any, becomes Logs/Description so the
	// existing channel templates that key off .Logs or .description
	// render useful context out of the box.
	var firstSample string
	if len(r.SampleSignals) > 0 {
		firstSample = r.SampleSignals[0].Message
	}

	content := map[string]interface{}{
		// AI-supplied
		"AlertName":   f.Title,
		"Summary":     f.Summary,
		"Severity":    f.Severity,
		"Category":    f.Category,
		"Confidence":  f.Confidence,
		"Suggestions": f.Suggestions,

		// Pattern context
		"PatternID":       r.PatternID,
		"PatternTemplate": r.Template,
		"Frequency":       r.Frequency,
		"Baseline":        r.Baseline,
		"Verdict":         r.Verdict.String(),

		// Provenance
		"ServiceName": service,
		"Service":     service, // alias used by some default templates
		"Source":      source,

		// Default-template friendliness
		"Logs":        firstSample,
		"description": f.Summary,

		// "firing" so isResolved returns false → AckURL injected, on-call
		// escalation triggers when configured.
		"Status": "firing",
	}

	// Escalate to on-call for high/critical severity findings when
	// on-call is configured. Lower severities skip on-call even if it
	// is globally enabled — agent medium/low findings are informational
	// and should not page operators.
	params := map[string]string{}
	switch f.Severity {
	case "critical", "high":
		params["oncall_enable"] = "true"
	default:
		params["oncall_enable"] = "false"
	}

	episodeStore, episodeCapable := store.(storage.DetectionEpisodeStore)
	if !episodeCapable || strings.TrimSpace(r.PatternID) == "" || r.Frequency <= 0 {
		return CreateIncident("", &content, &params)
	}
	agentKind := strings.TrimSpace(r.AgentKind)
	if agentKind == "" {
		agentKind = "detect"
	}
	signalKind := strings.TrimSpace(r.SignalKind)
	if signalKind == "" {
		signalKind = "logs"
	}
	decision, err := episodeStore.RecordDetectionOccurrence(storage.DetectionOccurrence{
		Identity: storage.DetectionIdentity{
			AgentKind: agentKind, Source: source, Service: service,
			SignalKind: signalKind, ConditionKey: r.PatternID,
		},
		Frequency: int64(r.Frequency), Severity: f.Severity, ReceivedAt: time.Now().UTC(), ClaimLease: claimLease,
	})
	if err != nil {
		if !errors.Is(err, storage.ErrUnsupported) {
			setDetectionEmissionError(r.DetectionEmission, err)
			logBoundedEpisodeError("record", err)
		}
		result := createIncident("", &content, incidentCreateOptions{}, &params)
		if r.DetectionEmission != nil {
			r.DetectionEmission.NotificationOutcome = detectionNotificationOutcome(result)
		}
		return result.Err
	}
	setDetectionEmissionDecision(r.DetectionEmission, decision)
	content["Frequency"] = decision.OccurrenceCount
	if !decision.NotificationClaimed {
		if r.DetectionEmission != nil {
			r.DetectionEmission.NotificationOutcome = "not_applicable"
		}
		return updateCoalescedDetectionIncident(decision, content)
	}
	heartbeat := startDetectionClaimHeartbeat(episodeStore, decision, claimLease)

	skipOnCall := false
	if decision.NotificationKind == storage.DetectionNotificationEscalation {
		existing, getErr := store.GetIncident(decision.IncidentID)
		switch {
		case getErr == nil:
			skipOnCall = existing.OnCallTriggered
		case errors.Is(getErr, storage.ErrNotFound):
		default:
			if r.DetectionEmission != nil {
				r.DetectionEmission.NotificationOutcome = "failed"
			}
			renewalErr := heartbeat.stop()
			completionErr := episodeStore.CompleteDetectionNotification(storage.DetectionNotificationCompletion{
				EpisodeID: decision.EpisodeID, ClaimToken: decision.ClaimToken,
				Outcome: "failed", CompletedAt: time.Now().UTC(),
			})
			return errors.Join(fmt.Errorf("inspect escalation incident: %w", getErr), renewalErr, completionErr)
		}
	}
	result := createIncident("", &content, incidentCreateOptions{
		IncidentID: decision.IncidentID,
		Detection:  &decision,
		SkipOnCall: skipOnCall,
	}, &params)
	outcome := detectionNotificationOutcome(result)
	if r.DetectionEmission != nil {
		r.DetectionEmission.NotificationOutcome = outcome
	}
	renewalErr := heartbeat.stop()
	if renewalErr != nil {
		logBoundedEpisodeError("renew", renewalErr)
	}
	completionErr := episodeStore.CompleteDetectionNotification(storage.DetectionNotificationCompletion{
		EpisodeID: decision.EpisodeID, ClaimToken: decision.ClaimToken,
		Outcome: outcome, CompletedAt: time.Now().UTC(),
	})
	if completionErr != nil {
		logBoundedEpisodeError("complete", completionErr)
	}
	if completionErr != nil || errors.Is(renewalErr, storage.ErrDetectionClaimLost) || errors.Is(renewalErr, storage.ErrNotFound) {
		return errors.Join(result.Err, renewalErr, completionErr)
	}
	return result.Err
}

type detectionClaimHeartbeat struct {
	stopOnce sync.Once
	stopCh   chan struct{}
	done     chan error
	err      error
}

func startDetectionClaimHeartbeat(store storage.DetectionEpisodeStore, decision storage.DetectionEpisodeDecision, claimLease time.Duration) *detectionClaimHeartbeat {
	if claimLease <= 0 {
		claimLease = storage.DefaultDetectionNotificationLease
	}
	interval := claimLease / 3
	if interval <= 0 {
		interval = time.Nanosecond
	}
	heartbeat := &detectionClaimHeartbeat{stopCh: make(chan struct{}), done: make(chan error, 1)}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		var firstErr error
		for {
			select {
			case <-heartbeat.stopCh:
				heartbeat.done <- firstErr
				return
			case <-ticker.C:
				err := store.RenewDetectionNotificationClaim(storage.DetectionNotificationRenewal{
					EpisodeID: decision.EpisodeID, ClaimToken: decision.ClaimToken,
					ClaimLease: claimLease, RenewedAt: time.Now().UTC(),
				})
				if err == nil {
					continue
				}
				renewalErr := fmt.Errorf("renew detection notification claim: %w", err)
				if firstErr == nil {
					firstErr = renewalErr
				}
				if errors.Is(err, storage.ErrDetectionClaimLost) || errors.Is(err, storage.ErrNotFound) {
					if !errors.Is(firstErr, err) {
						firstErr = errors.Join(firstErr, renewalErr)
					}
					heartbeat.done <- firstErr
					return
				}
			}
		}
	}()
	return heartbeat
}

func (heartbeat *detectionClaimHeartbeat) stop() error {
	heartbeat.stopOnce.Do(func() {
		close(heartbeat.stopCh)
		heartbeat.err = <-heartbeat.done
	})
	return heartbeat.err
}

// ContinueDetectionEpisode records normal traffic only when the same condition
// already has an active detection episode. It never opens an episode or claims
// a notification.
func ContinueDetectionEpisode(occurrence storage.DetectionOccurrence) (storage.DetectionEpisodeDecision, error) {
	episodeStore, ok := store.(storage.DetectionEpisodeStore)
	if !ok {
		return storage.DetectionEpisodeDecision{}, storage.ErrUnsupported
	}
	occurrence.ExistingOnly = true
	decision, err := episodeStore.RecordDetectionOccurrence(occurrence)
	if err != nil {
		return storage.DetectionEpisodeDecision{}, err
	}
	return decision, nil
}

func setDetectionEmissionDecision(emission *core.DetectionEmissionResult, decision storage.DetectionEpisodeDecision) {
	if emission == nil {
		return
	}
	emission.Fingerprint = decision.Fingerprint
	emission.EpisodeID = decision.EpisodeID
	emission.IncidentID = decision.IncidentID
	emission.OccurrenceDelta = decision.OccurrenceDelta
	emission.OccurrenceCount = decision.OccurrenceCount
	emission.EpisodeAction = string(decision.Action)
}

func setDetectionEmissionError(emission *core.DetectionEmissionResult, err error) {
	if emission == nil {
		return
	}
	message := err.Error()
	if len(message) > 256 {
		message = message[:256]
	}
	emission.EpisodeAction = "episode_error"
	emission.EpisodeError = message
}

func updateCoalescedDetectionIncident(decision storage.DetectionEpisodeDecision, content map[string]interface{}) error {
	detectionStore, ok := store.(storage.DetectionIncidentStore)
	if !ok {
		return storage.ErrUnsupported
	}
	rec, err := store.GetIncident(decision.IncidentID)
	if errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	merged := make(map[string]interface{}, len(rec.Content)+len(content))
	for key, value := range rec.Content {
		merged[key] = value
	}
	for key, value := range content {
		merged[key] = value
	}
	rec.Content = merged
	rec.Title = ExtractTitle(merged)
	rec.Service = ExtractService(merged)
	applyDetectionDecision(rec, decision)
	rec.NotifyStatus = ""
	return detectionStore.SaveDetectionIncident(rec)
}

func detectionNotificationOutcome(result incidentCreateResult) string {
	if result.NotifyStatus == "failed" {
		return "failed"
	}
	switch result.Decision.Action {
	case EmitSuppress:
		return "suppressed"
	case EmitDelay:
		return "delayed"
	case EmitGroup:
		return "grouped"
	case EmitDivert:
		if result.Err == nil {
			return "diverted"
		}
	}
	if result.NotifyStatus != "" {
		return result.NotifyStatus
	}
	if result.Err != nil {
		return "failed"
	}
	return "sent"
}

func logBoundedEpisodeError(operation string, err error) {
	message := err.Error()
	if len(message) > 256 {
		message = message[:256]
	}
	log.Printf("incident: episode_error operation=%s error=%q", operation, message)
}
