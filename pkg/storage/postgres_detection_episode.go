package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

type postgresDetectionEpisode struct {
	EpisodeID               string
	Fingerprint             string
	IncidentID              string
	OccurrenceCount         int64
	FirstSeen               time.Time
	LastSeen                time.Time
	HighestObservedSeverity string
	HighestHandledSeverity  string
	HighestNotifiedSeverity string
	PendingKind             DetectionNotificationKind
	PendingSeverity         string
	PendingToken            string
	PendingOwner            string
	PendingExpiresAt        time.Time
	LastCompletedToken      string
	LastNotificationOutcome string
}

func (p *postgresProvider) RecordDetectionOccurrence(occurrence DetectionOccurrence) (DetectionEpisodeDecision, error) {
	return p.RecordDetectionOccurrenceForOrg(DefaultOrgID, occurrence)
}

func (p *postgresProvider) RecordDetectionOccurrenceForOrg(orgID string, occurrence DetectionOccurrence) (DetectionEpisodeDecision, error) {
	identity := normalizeDetectionIdentity(occurrence.Identity)
	fingerprint, err := DetectionIdentityFingerprint(identity)
	if err != nil || occurrence.Frequency <= 0 {
		return DetectionEpisodeDecision{}, ErrInvalidDetectionOccurrence
	}
	orgID = NormalizeOrgID(orgID)
	now := occurrence.ReceivedAt.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	quietPeriod := occurrence.QuietPeriod
	if quietPeriod <= 0 {
		quietPeriod = DefaultDetectionEpisodeQuietPeriod
	}
	lease := occurrence.ClaimLease
	if lease <= 0 {
		lease = DefaultDetectionNotificationLease
	}
	severity := canonicalDetectionSeverity(occurrence.Severity)

	tx, err := p.db.BeginTx(context.Background(), nil)
	if err != nil {
		return DetectionEpisodeDecision{}, fmt.Errorf("storage: begin detection occurrence: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, orgID+":"+fingerprint); err != nil {
		return DetectionEpisodeDecision{}, fmt.Errorf("storage: lock detection identity: %w", err)
	}

	episode, err := scanPostgresDetectionEpisode(tx.QueryRow(`
		SELECT episode_id, fingerprint, incident_id, occurrence_count, first_seen, last_seen,
		       highest_observed_severity, highest_handled_severity, highest_notified_severity,
		       pending_kind, pending_severity, pending_token, pending_owner,
		       pending_expires_at, last_completed_token, last_notification_outcome
		FROM vs_detection_episodes
		WHERE org_id = $1 AND fingerprint = $2 AND closed_at IS NULL
		FOR UPDATE`, orgID, fingerprint))
	legacyIdentity := false
	if errors.Is(err, sql.ErrNoRows) {
		episode, err = scanPostgresDetectionEpisode(tx.QueryRow(`
			SELECT episode_id, fingerprint, incident_id, occurrence_count, first_seen, last_seen,
			       highest_observed_severity, highest_handled_severity, highest_notified_severity,
			       pending_kind, pending_severity, pending_token, pending_owner,
			       pending_expires_at, last_completed_token, last_notification_outcome
			FROM vs_detection_episodes
			WHERE org_id = $1 AND identity_version = $2
			  AND lower(btrim(agent_kind)) = $3 AND lower(btrim(source)) = $4
			  AND lower(btrim(service)) = $5 AND lower(btrim(signal_kind)) = $6
			  AND btrim(condition_key) = $7 AND closed_at IS NULL
			FOR UPDATE`, orgID, identity.Version, identity.AgentKind, identity.Source,
			identity.Service, identity.SignalKind, identity.ConditionKey))
		legacyIdentity = err == nil
	}
	reopened := false
	if errors.Is(err, sql.ErrNoRows) {
		episode = nil
		if occurrence.ExistingOnly {
			return DetectionEpisodeDecision{}, ErrNotFound
		}
		if err := tx.QueryRow(`SELECT EXISTS (
			SELECT 1 FROM vs_detection_episodes WHERE org_id = $1 AND fingerprint = $2
		)`, orgID, fingerprint).Scan(&reopened); err != nil {
			return DetectionEpisodeDecision{}, fmt.Errorf("storage: inspect detection history: %w", err)
		}
	} else if err != nil {
		return DetectionEpisodeDecision{}, fmt.Errorf("storage: read detection episode: %w", err)
	}
	if legacyIdentity {
		if _, err := tx.Exec(`UPDATE vs_detection_episodes SET
			fingerprint = $3, agent_kind = $4, source = $5, service = $6, signal_kind = $7
			WHERE org_id = $1 AND episode_id = $2`, orgID, episode.EpisodeID, fingerprint,
			identity.AgentKind, identity.Source, identity.Service, identity.SignalKind); err != nil {
			return DetectionEpisodeDecision{}, fmt.Errorf("storage: canonicalize detection identity: %w", err)
		}
		episode.Fingerprint = fingerprint
	}
	if episode != nil && !now.Before(episode.LastSeen.Add(quietPeriod)) {
		if _, err := tx.Exec(`UPDATE vs_detection_episodes SET closed_at = $3
			WHERE org_id = $1 AND episode_id = $2`, orgID, episode.EpisodeID, now); err != nil {
			return DetectionEpisodeDecision{}, fmt.Errorf("storage: close quiet detection episode: %w", err)
		}
		reopened = true
		episode = nil
		if occurrence.ExistingOnly {
			if err := tx.Commit(); err != nil {
				return DetectionEpisodeDecision{}, fmt.Errorf("storage: commit quiet detection close: %w", err)
			}
			return DetectionEpisodeDecision{}, ErrNotFound
		}
	}
	if episode != nil {
		var incidentResolved bool
		incidentErr := tx.QueryRow(`SELECT resolved FROM vs_incidents WHERE org_id = $1 AND id = $2`, orgID, episode.IncidentID).Scan(&incidentResolved)
		if incidentErr != nil && !errors.Is(incidentErr, sql.ErrNoRows) {
			return DetectionEpisodeDecision{}, fmt.Errorf("storage: inspect linked detection incident: %w", incidentErr)
		}
		orphaned := errors.Is(incidentErr, sql.ErrNoRows) && episode.PendingToken == "" && detectionNotificationDelivered(episode.LastNotificationOutcome)
		if incidentResolved || orphaned {
			if _, err := tx.Exec(`UPDATE vs_detection_episodes SET closed_at = $3 WHERE org_id = $1 AND episode_id = $2`, orgID, episode.EpisodeID, now); err != nil {
				return DetectionEpisodeDecision{}, fmt.Errorf("storage: close inactive detection episode: %w", err)
			}
			reopened = true
			episode = nil
			if occurrence.ExistingOnly {
				if err := tx.Commit(); err != nil {
					return DetectionEpisodeDecision{}, fmt.Errorf("storage: commit orphaned detection close: %w", err)
				}
				return DetectionEpisodeDecision{}, ErrNotFound
			}
		}
	}

	if episode == nil {
		owner := occurrence.ClaimOwner
		if owner == "" {
			owner = uuid.NewString()
		}
		episode = &postgresDetectionEpisode{
			EpisodeID: uuid.NewString(), IncidentID: uuid.NewString(), OccurrenceCount: occurrence.Frequency,
			FirstSeen: now, LastSeen: now, HighestObservedSeverity: severity,
			PendingKind: DetectionNotificationInitial, PendingSeverity: severity,
			PendingToken: uuid.NewString(), PendingOwner: owner, PendingExpiresAt: now.Add(lease),
		}
		_, err := tx.Exec(`INSERT INTO vs_detection_episodes (
			episode_id, org_id, identity_version, agent_kind, source, service, signal_kind,
			condition_key, fingerprint, incident_id, occurrence_count, first_seen, last_seen,
			highest_observed_severity, pending_kind, pending_severity, pending_token,
			pending_owner, pending_expires_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19)`,
			episode.EpisodeID, orgID, identity.Version, identity.AgentKind, identity.Source,
			identity.Service, identity.SignalKind, identity.ConditionKey, fingerprint,
			episode.IncidentID, episode.OccurrenceCount, now, now, severity,
			episode.PendingKind, episode.PendingSeverity, episode.PendingToken,
			episode.PendingOwner, episode.PendingExpiresAt)
		if err != nil {
			return DetectionEpisodeDecision{}, fmt.Errorf("storage: insert detection episode: %w", err)
		}
		decision := postgresDetectionDecision(fingerprint, episode)
		decision.Action = DetectionEpisodeOpened
		if reopened {
			decision.Action = DetectionEpisodeReopened
		}
		decision.OccurrenceDelta = occurrence.Frequency
		decision.NotificationClaimed = true
		decision.NotificationKind = DetectionNotificationInitial
		decision.ClaimToken = episode.PendingToken
		decision.ClaimExpiresAt = episode.PendingExpiresAt
		if err := tx.Commit(); err != nil {
			return DetectionEpisodeDecision{}, fmt.Errorf("storage: commit detection episode open: %w", err)
		}
		return decision, nil
	}

	episode.OccurrenceCount += occurrence.Frequency
	episode.LastSeen = now
	if detectionSeverityHigher(severity, episode.HighestObservedSeverity) {
		episode.HighestObservedSeverity = severity
	}
	action := DetectionEpisodeCoalesced
	claimKind := DetectionNotificationKind("")
	if !occurrence.ExistingOnly {
		if episode.PendingToken != "" && !now.Before(episode.PendingExpiresAt) {
			claimKind = episode.PendingKind
			if claimKind == DetectionNotificationEscalation {
				action = DetectionEpisodeEscalated
			}
			episode.PendingToken = ""
		} else if episode.PendingToken == "" {
			switch {
			case episode.HighestHandledSeverity == "":
				claimKind = DetectionNotificationInitial
			case detectionSeverityHigher(episode.HighestObservedSeverity, episode.HighestHandledSeverity):
				claimKind = DetectionNotificationEscalation
				action = DetectionEpisodeEscalated
			}
		}
	}
	if claimKind != "" {
		owner := occurrence.ClaimOwner
		if owner == "" {
			owner = uuid.NewString()
		}
		episode.PendingKind = claimKind
		episode.PendingSeverity = episode.HighestObservedSeverity
		episode.PendingToken = uuid.NewString()
		episode.PendingOwner = owner
		episode.PendingExpiresAt = now.Add(lease)
	}
	_, err = tx.Exec(`UPDATE vs_detection_episodes SET
		occurrence_count = $3, last_seen = $4, highest_observed_severity = $5,
		pending_kind = NULLIF($6, ''), pending_severity = NULLIF($7, ''),
		pending_token = NULLIF($8, ''), pending_owner = NULLIF($9, ''),
		pending_expires_at = $10
		WHERE org_id = $1 AND episode_id = $2`, orgID, episode.EpisodeID,
		episode.OccurrenceCount, episode.LastSeen, episode.HighestObservedSeverity,
		episode.PendingKind, episode.PendingSeverity, episode.PendingToken,
		episode.PendingOwner, nullTime(episode.PendingExpiresAt))
	if err != nil {
		return DetectionEpisodeDecision{}, fmt.Errorf("storage: update detection episode: %w", err)
	}
	if occurrence.ExistingOnly {
		_, err = tx.Exec(`UPDATE vs_incidents SET
			detection_fingerprint = $3, detection_episode_id = $2,
			occurrence_count = $4, detection_first_seen = $5, detection_last_seen = $6,
			highest_observed_severity = $7, highest_notified_severity = NULLIF($8, ''),
			content = jsonb_set(
				jsonb_set(COALESCE(content, '{}'::jsonb), '{Frequency}', to_jsonb($4::bigint), true),
				'{Severity}', to_jsonb($10::text), true)
			WHERE org_id = $1 AND id = $9`, orgID, episode.EpisodeID, fingerprint,
			episode.OccurrenceCount, episode.FirstSeen, episode.LastSeen,
			episode.HighestObservedSeverity, episode.HighestNotifiedSeverity, episode.IncidentID,
			canonicalDetectionIncidentSeverity(episode.HighestObservedSeverity, episode.HighestNotifiedSeverity))
		if err != nil {
			return DetectionEpisodeDecision{}, fmt.Errorf("storage: reconcile known detection continuation: %w", err)
		}
	}
	decision := postgresDetectionDecision(fingerprint, episode)
	decision.Action = action
	decision.OccurrenceDelta = occurrence.Frequency
	if claimKind != "" {
		decision.NotificationClaimed = true
		decision.NotificationKind = claimKind
		decision.ClaimToken = episode.PendingToken
		decision.ClaimExpiresAt = episode.PendingExpiresAt
	}
	if err := tx.Commit(); err != nil {
		return DetectionEpisodeDecision{}, fmt.Errorf("storage: commit detection occurrence: %w", err)
	}
	return decision, nil
}

func (p *postgresProvider) RenewDetectionNotificationClaim(renewal DetectionNotificationRenewal) error {
	return p.RenewDetectionNotificationClaimForOrg(DefaultOrgID, renewal)
}

func (p *postgresProvider) RenewDetectionNotificationClaimForOrg(orgID string, renewal DetectionNotificationRenewal) error {
	now := renewal.RenewedAt.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	lease := renewal.ClaimLease
	if lease <= 0 {
		lease = DefaultDetectionNotificationLease
	}
	if renewal.ClaimToken == "" {
		return ErrDetectionClaimLost
	}
	orgID = NormalizeOrgID(orgID)
	result, err := p.db.Exec(`UPDATE vs_detection_episodes
		SET pending_expires_at = $4
		WHERE org_id = $1 AND episode_id = $2 AND pending_token = $3
		  AND pending_expires_at > $5`, orgID, renewal.EpisodeID, renewal.ClaimToken, now.Add(lease), now)
	if err != nil {
		return fmt.Errorf("storage: renew detection claim: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("storage: inspect detection claim renewal: %w", err)
	}
	if rows == 1 {
		return nil
	}
	var exists bool
	if err := p.db.QueryRow(`SELECT EXISTS (
		SELECT 1 FROM vs_detection_episodes WHERE org_id = $1 AND episode_id = $2
	)`, orgID, renewal.EpisodeID).Scan(&exists); err != nil {
		return fmt.Errorf("storage: inspect detection claim: %w", err)
	}
	if !exists {
		return ErrNotFound
	}
	return ErrDetectionClaimLost
}

func (p *postgresProvider) CompleteDetectionNotification(completion DetectionNotificationCompletion) error {
	return p.CompleteDetectionNotificationForOrg(DefaultOrgID, completion)
}

func (p *postgresProvider) CompleteDetectionNotificationForOrg(orgID string, completion DetectionNotificationCompletion) error {
	orgID = NormalizeOrgID(orgID)
	tx, err := p.db.BeginTx(context.Background(), nil)
	if err != nil {
		return fmt.Errorf("storage: begin detection completion: %w", err)
	}
	defer tx.Rollback()
	episode, err := scanPostgresDetectionEpisode(tx.QueryRow(`
		SELECT episode_id, fingerprint, incident_id, occurrence_count, first_seen, last_seen,
		       highest_observed_severity, highest_handled_severity, highest_notified_severity,
		       pending_kind, pending_severity, pending_token, pending_owner,
		       pending_expires_at, last_completed_token, last_notification_outcome
		FROM vs_detection_episodes WHERE org_id = $1 AND episode_id = $2 FOR UPDATE`, orgID, completion.EpisodeID))
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("storage: read detection completion: %w", err)
	}
	if episode.LastCompletedToken == completion.ClaimToken && completion.ClaimToken != "" {
		return tx.Commit()
	}
	if completion.ClaimToken == "" || completion.ClaimToken != episode.PendingToken {
		return ErrDetectionClaimLost
	}
	if detectionNotificationTerminal(completion.Outcome) {
		episode.HighestHandledSeverity = episode.PendingSeverity
		if detectionNotificationDelivered(completion.Outcome) {
			episode.HighestNotifiedSeverity = episode.PendingSeverity
		}
	}
	_, err = tx.Exec(`UPDATE vs_detection_episodes SET
		highest_handled_severity = NULLIF($3, ''), highest_notified_severity = NULLIF($4, ''),
		last_completed_token = $5, last_notification_outcome = $6, pending_kind = NULL, pending_severity = NULL,
		pending_token = NULL, pending_owner = NULL, pending_expires_at = NULL
		WHERE org_id = $1 AND episode_id = $2`, orgID, episode.EpisodeID,
		episode.HighestHandledSeverity, episode.HighestNotifiedSeverity, completion.ClaimToken, completion.Outcome)
	if err != nil {
		return fmt.Errorf("storage: update detection completion: %w", err)
	}
	_, err = tx.Exec(`UPDATE vs_incidents SET
		detection_fingerprint = $3, detection_episode_id = $2,
		occurrence_count = $4, detection_first_seen = $5, detection_last_seen = $6,
		highest_observed_severity = $7, highest_notified_severity = NULLIF($8, ''),
		content = jsonb_set(
			jsonb_set(COALESCE(content, '{}'::jsonb), '{Frequency}', to_jsonb($4::bigint), true),
			'{Severity}', to_jsonb($10::text), true)
		WHERE org_id = $1 AND id = $9`, orgID, episode.EpisodeID, episode.Fingerprint, episode.OccurrenceCount,
		episode.FirstSeen, episode.LastSeen, episode.HighestObservedSeverity,
		episode.HighestNotifiedSeverity, episode.IncidentID,
		canonicalDetectionIncidentSeverity(episode.HighestObservedSeverity, episode.HighestNotifiedSeverity))
	if err != nil {
		return fmt.Errorf("storage: reconcile detection incident: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("storage: commit detection completion: %w", err)
	}
	return nil
}

func (p *postgresProvider) CloseDetectionEpisodeByIncident(incidentID string, closedAt time.Time) error {
	return p.CloseDetectionEpisodeByIncidentForOrg(DefaultOrgID, incidentID, closedAt)
}

func (p *postgresProvider) CloseDetectionEpisodeByIncidentForOrg(orgID, incidentID string, closedAt time.Time) error {
	closedAt = closedAt.UTC()
	if closedAt.IsZero() {
		closedAt = time.Now().UTC()
	}
	result, err := p.db.Exec(`UPDATE vs_detection_episodes SET closed_at = $3
		WHERE org_id = $1 AND incident_id = $2 AND closed_at IS NULL`, NormalizeOrgID(orgID), incidentID, closedAt)
	if err != nil {
		return fmt.Errorf("storage: close detection episode: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("storage: inspect closed detection episode: %w", err)
	}
	if rows == 0 {
		return ErrNotFound
	}
	return nil
}

func scanPostgresDetectionEpisode(row rowScanner) (*postgresDetectionEpisode, error) {
	var episode postgresDetectionEpisode
	var handled, notified, pendingKind, pendingSeverity, pendingToken, pendingOwner, completed, notificationOutcome sql.NullString
	var pendingExpires sql.NullTime
	if err := row.Scan(&episode.EpisodeID, &episode.Fingerprint, &episode.IncidentID, &episode.OccurrenceCount,
		&episode.FirstSeen, &episode.LastSeen, &episode.HighestObservedSeverity,
		&handled, &notified, &pendingKind, &pendingSeverity, &pendingToken, &pendingOwner,
		&pendingExpires, &completed, &notificationOutcome); err != nil {
		return nil, err
	}
	episode.FirstSeen = episode.FirstSeen.UTC()
	episode.LastSeen = episode.LastSeen.UTC()
	episode.HighestHandledSeverity = handled.String
	episode.HighestNotifiedSeverity = notified.String
	episode.PendingKind = DetectionNotificationKind(pendingKind.String)
	episode.PendingSeverity = pendingSeverity.String
	episode.PendingToken = pendingToken.String
	episode.PendingOwner = pendingOwner.String
	if pendingExpires.Valid {
		episode.PendingExpiresAt = pendingExpires.Time.UTC()
	}
	episode.LastCompletedToken = completed.String
	episode.LastNotificationOutcome = notificationOutcome.String
	return &episode, nil
}

func postgresDetectionDecision(fingerprint string, episode *postgresDetectionEpisode) DetectionEpisodeDecision {
	return DetectionEpisodeDecision{
		Fingerprint: fingerprint, EpisodeID: episode.EpisodeID, IncidentID: episode.IncidentID,
		OccurrenceCount: episode.OccurrenceCount, FirstSeen: episode.FirstSeen, LastSeen: episode.LastSeen,
		HighestObservedSeverity: episode.HighestObservedSeverity,
		HighestNotifiedSeverity: episode.HighestNotifiedSeverity,
	}
}

func nullTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value.UTC()
}
