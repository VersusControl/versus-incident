package services

import (
	"github.com/VersusControl/versus-incident/pkg/storage"
	"time"
)

type scopedEpisodeStore struct {
	storage.ScopedDetectionEpisodeStore
	org string
}

func (s scopedEpisodeStore) RecordDetectionOccurrence(o storage.DetectionOccurrence) (storage.DetectionEpisodeDecision, error) {
	return s.RecordDetectionOccurrenceForOrg(s.org, o)
}
func (s scopedEpisodeStore) RenewDetectionNotificationClaim(r storage.DetectionNotificationRenewal) error {
	return s.RenewDetectionNotificationClaimForOrg(s.org, r)
}
func (s scopedEpisodeStore) CompleteDetectionNotification(c storage.DetectionNotificationCompletion) error {
	return s.CompleteDetectionNotificationForOrg(s.org, c)
}
func (s scopedEpisodeStore) CloseDetectionEpisodeByIncident(id string, at time.Time) error {
	return s.CloseDetectionEpisodeByIncidentForOrg(s.org, id, at)
}
