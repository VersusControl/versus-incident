package issues

import (
	"encoding/json"

	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/utils"
)

// Record encodes an issue as a stored row: the queue's columns, and the
// whole issue as the payload.
func Record(issue Issue) (storage.AWSIssueRecord, error) {
	redactIssueText(&issue)
	payload, err := json.Marshal(issue)
	if err != nil {
		return storage.AWSIssueRecord{}, err
	}
	record := storage.AWSIssueRecord{
		ID: issue.ID, Rule: issue.Rule, Kind: string(issue.Kind), Severity: string(issue.Severity), Tier: string(issue.Tier),
		State: string(issue.State), Confirm: string(issue.Confirm), AccountID: issue.Account, Region: issue.Region,
		Owner: issue.Owner, GroupID: issue.GroupID, Title: issue.Title, FirstSeen: issue.FirstSeen, OpenedAt: issue.OpenedAt,
		ResolvedAt: issue.ResolvedAt, Version: issue.Version, UpdatedAt: issue.UpdatedAt, Payload: payload,
	}
	if issue.SinceKnown {
		record.Since = issue.Since
	}
	if issue.Rejection != nil {
		record.RejectedAt = issue.Rejection.At
		record.ReviewOn = issue.Rejection.ReviewOn
	}
	return record, nil
}

// redactIssueText passes the text a rule or the likely-cause lookup wrote
// through the shared secret redactor: the title, evidence, steps and
// cause. It runs where an issue is recorded, like a rejection reason's
// cleaning, so every reader (the pages, the report, alerts, agent tools)
// gets redacted text. Applied again it changes nothing.
func redactIssueText(issue *Issue) {
	issue.Title = utils.RedactSecrets(issue.Title)
	if len(issue.Evidence) > 0 {
		evidence := make([]Evidence, len(issue.Evidence))
		for index, item := range issue.Evidence {
			item.Text, item.Reason = utils.RedactSecrets(item.Text), utils.RedactSecrets(item.Reason)
			evidence[index] = item
		}
		issue.Evidence = evidence
	}
	if len(issue.Steps) > 0 {
		steps := make([]Step, len(issue.Steps))
		for index, step := range issue.Steps {
			step.Text, step.Console, step.Command = utils.RedactSecrets(step.Text), utils.RedactSecrets(step.Console), utils.RedactSecrets(step.Command)
			steps[index] = step
		}
		issue.Steps = steps
	}
	if issue.Cause != nil {
		cause := *issue.Cause
		cause.Text = utils.RedactSecrets(cause.Text)
		issue.Cause = &cause
	}
}

// FromRecord decodes a stored row. The columns win over the payload, so a
// row is read as the queue filtered it.
func FromRecord(record storage.AWSIssueRecord) (Issue, error) {
	var issue Issue
	if len(record.Payload) > 0 {
		if err := json.Unmarshal(record.Payload, &issue); err != nil {
			return Issue{}, err
		}
	}
	issue.ID, issue.Rule, issue.Kind = record.ID, record.Rule, Kind(record.Kind)
	issue.Severity, issue.Tier, issue.State, issue.Confirm = Severity(record.Severity), Tier(record.Tier), State(record.State), Confirm(record.Confirm)
	issue.Account, issue.Region, issue.Owner, issue.GroupID, issue.Title = record.AccountID, record.Region, record.Owner, record.GroupID, record.Title
	issue.FirstSeen, issue.OpenedAt, issue.ResolvedAt = record.FirstSeen, record.OpenedAt, record.ResolvedAt
	issue.Version, issue.UpdatedAt, issue.Org = record.Version, record.UpdatedAt, record.Org
	if issue.SinceKnown {
		issue.Since = record.Since
	}
	if record.RejectedAt.IsZero() {
		issue.Rejection = nil
	}
	return issue, nil
}

func eventRecords(events []Event) []storage.AWSIssueEvent {
	out := make([]storage.AWSIssueEvent, 0, len(events))
	for _, event := range events {
		out = append(out, storage.AWSIssueEvent{IssueID: event.IssueID, At: event.At, Kind: string(event.Kind), Actor: event.Actor, Detail: event.Detail})
	}
	return out
}

// EventFromRecord decodes a stored event.
func EventFromRecord(record storage.AWSIssueEvent) Event {
	return Event{IssueID: record.IssueID, Seq: record.Seq, At: record.At, Kind: EventKind(record.Kind), Actor: record.Actor, Detail: record.Detail}
}
