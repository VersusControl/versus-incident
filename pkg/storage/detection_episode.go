package storage

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/VersusControl/versus-incident/pkg/utils"
)

const (
	DetectionIdentityVersion           = "v1"
	DefaultDetectionEpisodeQuietPeriod = 10 * time.Minute
	DefaultDetectionNotificationLease  = time.Minute
	MaxClosedDetectionEpisodesDefault  = 1000
)

var (
	ErrInvalidDetectionOccurrence = errors.New("storage: invalid detection occurrence")
	ErrDetectionClaimLost         = errors.New("storage: detection notification claim lost")
)

type DetectionEpisodeStore interface {
	RecordDetectionOccurrence(DetectionOccurrence) (DetectionEpisodeDecision, error)
	RenewDetectionNotificationClaim(DetectionNotificationRenewal) error
	CompleteDetectionNotification(DetectionNotificationCompletion) error
	CloseDetectionEpisodeByIncident(incidentID string, closedAt time.Time) error
}

type ScopedDetectionEpisodeStore interface {
	RecordDetectionOccurrenceForOrg(orgID string, occurrence DetectionOccurrence) (DetectionEpisodeDecision, error)
	RenewDetectionNotificationClaimForOrg(orgID string, renewal DetectionNotificationRenewal) error
	CompleteDetectionNotificationForOrg(orgID string, completion DetectionNotificationCompletion) error
	CloseDetectionEpisodeByIncidentForOrg(orgID, incidentID string, closedAt time.Time) error
}

// DetectionIncidentStore is an optional companion capability for episode
// backends. It persists detection-owned evidence and delivery state without
// replacing lifecycle, acknowledgement, or assignment fields owned by an
// operator racing the notification fan-out. An update with an empty
// NotifyStatus changes evidence and episode progress only; existing channel,
// notification, and on-call outcome fields remain unchanged.
type DetectionIncidentStore interface {
	SaveDetectionIncident(*IncidentRecord) error
}

type DetectionIdentity struct {
	Version      string `json:"version"`
	AgentKind    string `json:"agent_kind"`
	Source       string `json:"source"`
	Service      string `json:"service"`
	SignalKind   string `json:"signal_kind"`
	ConditionKey string `json:"condition_key"`
}

type DetectionOccurrence struct {
	Identity     DetectionIdentity
	Frequency    int64
	Severity     string
	ReceivedAt   time.Time
	QuietPeriod  time.Duration
	ClaimOwner   string
	ClaimLease   time.Duration
	ExistingOnly bool
}

type DetectionEpisodeAction string

const (
	DetectionEpisodeOpened    DetectionEpisodeAction = "opened"
	DetectionEpisodeCoalesced DetectionEpisodeAction = "coalesced"
	DetectionEpisodeEscalated DetectionEpisodeAction = "escalated"
	DetectionEpisodeReopened  DetectionEpisodeAction = "reopened"
)

type DetectionNotificationKind string

const (
	DetectionNotificationInitial    DetectionNotificationKind = "initial"
	DetectionNotificationEscalation DetectionNotificationKind = "escalation"
)

type DetectionEpisodeDecision struct {
	Fingerprint             string
	EpisodeID               string
	IncidentID              string
	OccurrenceDelta         int64
	OccurrenceCount         int64
	FirstSeen               time.Time
	LastSeen                time.Time
	HighestObservedSeverity string
	HighestNotifiedSeverity string
	Action                  DetectionEpisodeAction
	NotificationClaimed     bool
	NotificationKind        DetectionNotificationKind
	ClaimToken              string
	ClaimExpiresAt          time.Time
}

type DetectionNotificationCompletion struct {
	EpisodeID   string
	ClaimToken  string
	Outcome     string
	CompletedAt time.Time
}

type DetectionNotificationRenewal struct {
	EpisodeID  string
	ClaimToken string
	ClaimLease time.Duration
	RenewedAt  time.Time
}

func DetectionIdentityFingerprint(identity DetectionIdentity) (string, error) {
	identity = normalizeDetectionIdentity(identity)
	if identity.AgentKind == "" || identity.Source == "" || identity.Service == "" || identity.SignalKind == "" || identity.ConditionKey == "" {
		return "", ErrInvalidDetectionOccurrence
	}
	encoded, err := json.Marshal(identity)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

func normalizeDetectionIdentity(identity DetectionIdentity) DetectionIdentity {
	if strings.TrimSpace(identity.Version) == "" {
		identity.Version = DetectionIdentityVersion
	}
	identity.Version = strings.TrimSpace(identity.Version)
	identity.AgentKind = strings.ToLower(strings.TrimSpace(identity.AgentKind))
	identity.Source = strings.ToLower(strings.TrimSpace(identity.Source))
	identity.Service = strings.ToLower(strings.TrimSpace(identity.Service))
	identity.SignalKind = strings.ToLower(strings.TrimSpace(identity.SignalKind))
	identity.ConditionKey = strings.TrimSpace(identity.ConditionKey)
	return identity
}

func canonicalDetectionSeverity(severity string) string {
	return utils.NormalizeSeverity(severity)
}

func detectionSeverityHigher(candidate, current string) bool {
	return utils.SeverityRank(candidate) > utils.SeverityRank(current)
}

func canonicalDetectionIncidentSeverity(observed, notified string) string {
	severity := canonicalDetectionSeverity(observed)
	if detectionSeverityHigher(notified, severity) {
		severity = canonicalDetectionSeverity(notified)
	}
	return severity
}

func detectionNotificationDelivered(outcome string) bool {
	switch outcome {
	case "sent", "partial", "diverted":
		return true
	default:
		return false
	}
}

func detectionNotificationTerminal(outcome string) bool {
	switch outcome {
	case "sent", "partial", "suppressed", "grouped", "diverted":
		return true
	default:
		return false
	}
}

func applyDetectionEpisodeDecision(incident *IncidentRecord, decision DetectionEpisodeDecision) {
	firstSeen := decision.FirstSeen
	lastSeen := decision.LastSeen
	incident.DetectionFingerprint = decision.Fingerprint
	incident.DetectionEpisodeID = decision.EpisodeID
	incident.OccurrenceCount = decision.OccurrenceCount
	incident.DetectionFirstSeen = &firstSeen
	incident.DetectionLastSeen = &lastSeen
	incident.HighestObservedSeverity = decision.HighestObservedSeverity
	incident.HighestNotifiedSeverity = decision.HighestNotifiedSeverity
	if incident.Content == nil {
		incident.Content = make(map[string]interface{})
	}
	incident.Content["Frequency"] = decision.OccurrenceCount
	severity := canonicalDetectionIncidentSeverity(decision.HighestObservedSeverity, decision.HighestNotifiedSeverity)
	if severity != "" {
		incident.Content["Severity"] = severity
	}
}
