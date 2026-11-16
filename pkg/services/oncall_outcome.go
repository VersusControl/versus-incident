package services

import (
	"errors"
	"log"

	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/utils"
)

// Every binary that persists incidents links this package, so registering
// here writes delayed escalation outcomes back in OSS and Enterprise alike.
func init() {
	core.SetDelayedOnCallRecorder(RecordOnCallOutcome)
}

// RecordOnCallOutcome writes the result of an on-call escalation that fired
// after the acknowledgement wait onto the stored incident, through the same
// fields the immediate path sets: a failure clears OnCallTriggered and stores
// the provider error in OnCallError; a success marks it triggered and clears
// any error left by an earlier attempt. Without storage, or for an incident
// that is no longer stored, it does nothing.
func RecordOnCallOutcome(incidentID string, err error) {
	if store == nil || incidentID == "" {
		return
	}
	rec, getErr := store.GetIncident(incidentID)
	if errors.Is(getErr, storage.ErrNotFound) {
		return
	}
	if getErr != nil {
		log.Printf("incident: load %s to record on-call outcome: %v", incidentID, getErr)
		return
	}
	triggered, onCallErr := true, ""
	if err != nil {
		triggered, onCallErr = false, utils.SafeErrorText(err)
	}
	if rec.OnCallTriggered == triggered && rec.OnCallError == onCallErr {
		return
	}
	rec.OnCallTriggered = triggered
	rec.OnCallError = onCallErr
	if saveErr := saveIncidentRecord(rec, rec.DetectionEpisodeID != ""); saveErr != nil {
		log.Printf("incident: persist on-call outcome for %s: %v", incidentID, saveErr)
	}
}
