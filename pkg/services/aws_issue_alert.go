package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/VersusControl/versus-incident/pkg/aws/issues"
	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/utils"
)

// awsIssueSource is the detection source of every AWS issue alert.
const awsIssueSource = "aws-issues"

// awsIssueAlertKind tells the channel templates an AWS issue alert apart.
const awsIssueAlertKind = "aws_issue"

// Bounds on what one alert carries.
const (
	awsAlertTextMax   = 512
	awsAlertLinesMax  = 5
	awsAlertDigestMax = 10
)

// CreateIncidentFromAWSIssue sends one AWS issue alert (an issue that
// opened or regressed, or a coverage notice) through the shared detection
// episode and every alert channel. The identity names the account, the
// rule and the issue; the condition key adds the transition, so a repeat
// of the same alert coalesces and the next regress is a new one. Every
// line of the message goes through the redactor.
func CreateIncidentFromAWSIssue(ctx context.Context, org string, alert issues.Alert) error {
	org = storage.NormalizeOrgID(org)
	if err := ctx.Err(); err != nil {
		return err
	}
	issue := alert.Issue
	if issue.ID == "" || issue.Account == "" || issue.Rule == "" || alert.Key == "" {
		return errors.New("invalid AWS issue alert")
	}
	switch alert.Kind {
	case issues.AlertOpened, issues.AlertRegressed, issues.AlertCoverage:
	default:
		return errors.New("invalid AWS issue alert kind")
	}
	content := awsIssueContent(alert, awsAlertPublicHost())
	severity, _ := content["Severity"].(string)
	params := map[string]string{"oncall_enable": "false"}
	if alert.Kind != issues.AlertCoverage && (severity == "critical" || severity == "high") {
		params = severeDetectionOnCallParams(ctx)
	}
	condition := issue.ID + ":" + alert.Key
	emission := &core.DetectionEmissionResult{}
	err := emitDetectionQuiet(awsIssueQuietPeriod, content, storage.DetectionIdentity{
		AgentKind: "aws", Source: awsIssueSource, Service: issue.Account, SignalKind: issue.Rule, ConditionKey: condition,
	}, severity, params, org, 1, emission, storage.DefaultDetectionNotificationLease, ctx)
	return awsDeliveryError(emission, err)
}

// awsIssueQuietPeriod keeps every attempt to send one transition in one
// detection episode, and so one incident, for as long as the alert path
// tries it: an hour past the give-up time. Each transition has its own
// condition key, so only the retries of one transition join.
var awsIssueQuietPeriod = issues.GiveUpAfter + time.Hour

// ErrAWSIssueAlertNotDelivered is a send of which nothing went out: no
// channel delivered and on-call did not page, or the outcome was never
// recorded. The alert path sends it again; every other outcome is final.
var ErrAWSIssueAlertNotDelivered = errors.New("aws issue alert not delivered")

// awsDeliveryError decides whether a send is tried again. Anything that
// went out (a channel, a partial fan-out, a diverted or grouped delivery,
// an on-call page this send started) is final, even with an error, so a
// retry never sends or pages twice. A send that joined an episode whose
// notification another attempt holds is final only once that attempt
// recorded its result: an attempt that died mid-send leaves the incident
// pending or unsaved, and its alert must be sent again.
func awsDeliveryError(emission *core.DetectionEmissionResult, err error) error {
	switch emission.NotificationOutcome {
	case "":
		if err == nil {
			return nil
		}
	case "failed":
		if emission.OnCallTriggered {
			return nil
		}
	case "not_applicable":
		if awsNotificationFinished(emission.CoalescedNotifyStatus) {
			return nil
		}
	default:
		return nil
	}
	if err == nil {
		err = ErrAWSIssueAlertNotDelivered
	}
	return fmt.Errorf("%w: %w", ErrAWSIssueAlertNotDelivered, err)
}

// awsNotificationFinished reports an incident delivery status that an
// earlier attempt recorded as its end. Empty (not saved yet), pending
// (the attempt is still sending, or died sending) and failed (nothing
// went out) are not.
func awsNotificationFinished(notifyStatus string) bool {
	switch notifyStatus {
	case "", "pending", "failed":
		return false
	default:
		return true
	}
}

// CreateIncidentFromAWSIssueDigest sends high issues as one message: more
// than three opened within an hour.
func CreateIncidentFromAWSIssueDigest(ctx context.Context, org string, alerts []issues.Alert) error {
	org = storage.NormalizeOrgID(org)
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(alerts) == 0 {
		return errors.New("empty AWS issue digest")
	}
	keys := make([]string, 0, len(alerts))
	for _, alert := range alerts {
		if alert.Issue.ID == "" || alert.Key == "" {
			return errors.New("invalid AWS issue alert in a digest")
		}
		keys = append(keys, alert.Issue.ID+":"+alert.Key)
	}
	sum := sha256.Sum256([]byte(strings.Join(keys, "\n")))
	condition := hex.EncodeToString(sum[:])
	if group := alerts[0].Group; group != "" {
		// Every attempt to send one digest carries its group key, so a
		// retry coalesces with an attempt that got through.
		condition = group
	}
	content := awsIssueDigestContent(alerts, awsAlertPublicHost())
	emission := &core.DetectionEmissionResult{}
	err := emitDetectionQuiet(awsIssueQuietPeriod, content, storage.DetectionIdentity{
		AgentKind: "aws", Source: awsIssueSource, Service: "digest", SignalKind: "high_digest", ConditionKey: condition,
	}, "high", severeDetectionOnCallParams(ctx), org, 1, emission, storage.DefaultDetectionNotificationLease, ctx)
	return awsDeliveryError(emission, err)
}

// awsScrub redacts one line: pasted secrets and AWS keys first, then the
// report redactor's patterns. It redacts before it folds whitespace, so a
// secret wrapped over a line break, which the redactor joins, is still
// found; and it bounds the length last, on a rune boundary, so a cut never
// turns a secret the redactor knows into a fragment it does not.
func awsScrub(text string) string {
	text = strings.Join(strings.Fields(reportScrubber().Scrub(utils.RedactSecrets(text))), " ")
	return cutRunes(text, awsAlertTextMax)
}

// cutRunes bounds text to at most limit bytes without splitting a rune.
func cutRunes(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut]
}

// awsIssueContent is the alert content: the issue's title, where and since,
// its evidence, the likely cause when one was found and what to do; or,
// for a coverage notice, what can no longer be read and how to fix it.
func awsIssueContent(alert issues.Alert, publicHost string) map[string]interface{} {
	issue := alert.Issue
	severity := string(issue.Severity)
	content := map[string]interface{}{
		"AlertKind": awsIssueAlertKind, "IssueAlert": string(alert.Kind), "Severity": severity,
		"Account": issue.Account, "Region": issue.Region, "Rule": issue.Rule,
		"ServiceName": issue.Account, "Service": issue.Account, "Source": awsIssueSource,
		"PatternID": "aws:" + issue.Rule, "Status": "firing",
	}
	utils.MarkInternalAlert(content)
	var lines []string
	if alert.Kind == issues.AlertCoverage {
		content["Severity"] = "medium"
		title, body := coverageLines(alert)
		content["AlertName"] = title
		content["Notes"] = scrubLines(body, 2*awsAlertLinesMax+3)
		lines = body
	} else {
		title := issue.Title
		if alert.Kind == issues.AlertRegressed {
			title = "Reopened: " + title
		}
		content["AlertName"] = title
		content["Where"] = awsScrub(issues.AlertWhere(alert))
		lines = append(lines, issues.AlertWhere(alert))
		if alert.Kind == issues.AlertRegressed && alert.Detail != "" {
			content["Reopened"] = awsScrub(alert.Detail)
			lines = append(lines, "Why it reopened: "+alert.Detail)
		}
		var evidence []string
		for _, item := range issue.Evidence {
			evidence = append(evidence, issues.EvidenceWord(item.Status)+": "+item.Text)
		}
		content["Evidence"] = scrubLines(evidence, awsAlertLinesMax)
		lines = append(lines, evidence[:min(len(evidence), awsAlertLinesMax)]...)
		if issue.Cause.Found() {
			content["LikelyCause"] = awsScrub(issue.Cause.Text)
			lines = append(lines, "Likely cause: "+issue.Cause.Text)
		}
		// Each step keeps its command apart from its text, so the channel
		// templates show the command as code that copies exactly.
		var steps []string
		shown := []map[string]string{}
		for _, step := range issue.Steps {
			steps = append(steps, step.Text)
			if step.Command != "" {
				steps = append(steps, step.Command)
			}
			if len(shown) < awsAlertLinesMax {
				shown = append(shown, map[string]string{"text": awsScrub(step.Text), "command": awsScrub(step.Command)})
			}
		}
		if len(shown) > 0 {
			content["Steps"] = shown
		}
		lines = append(lines, steps[:min(len(steps), awsAlertLinesMax)]...)
	}
	content["AlertName"] = awsScrub(content["AlertName"].(string))
	content["Lines"] = scrubLines(lines, 2*awsAlertLinesMax+3)
	content["Description"] = strings.Join(content["Lines"].([]string), "\n")
	if origin := awsAlertOrigin(publicHost); origin != "" {
		label, link := "Open issue", origin+"/aws/issues/"+url.PathEscape(issue.ID)
		if alert.Kind == issues.AlertCoverage {
			label, link = "Open coverage", origin+"/aws/coverage"
		}
		content["IssueURL"] = link
		content["Links"] = []map[string]string{{"id": "issue", "label": label, "url": link}}
	}
	return content
}

// coverageLines are the notice that a source can no longer be read: its
// title, the account and last good read, the issue that can't be confirmed
// and the fix.
func coverageLines(alert issues.Alert) (string, []string) {
	issue := alert.Issue
	account := issue.Account
	if alert.AccountName != "" {
		account = alert.AccountName
	}
	title := "Versus can no longer read an input of an open issue"
	var lines []string
	if len(alert.Unread) > 0 {
		first := alert.Unread[0]
		title = "Versus can no longer read " + first.Source + ": " + issues.ReasonWords(first.Reason)
	}
	for _, unread := range alert.Unread {
		where := account
		if !unread.LastGoodAt.IsZero() {
			where += " · last good read " + unread.LastGoodAt.UTC().Format("2006-01-02 15:04 UTC")
		}
		source := unread.Source
		if unread.Operation != "" {
			source += " " + unread.Operation
		}
		lines = append(lines, "Not read: "+source+" ("+issues.ReasonWords(unread.Reason)+") · "+where)
	}
	if len(alert.Unread) == 0 {
		lines = append(lines, account)
	}
	lines = append(lines, "1 open issue can't be confirmed and stays open: "+issue.Title)
	for _, unread := range alert.Unread {
		if len(unread.Fix) > 0 {
			lines = append(lines, "Add "+strings.Join(unread.Fix, ", ")+" back to the Versus read role")
		}
	}
	return title, lines
}

// awsIssueDigestContent lists high issues in one message.
func awsIssueDigestContent(alerts []issues.Alert, publicHost string) map[string]interface{} {
	lines := []string{}
	for index, alert := range alerts {
		if index == awsAlertDigestMax {
			lines = append(lines, fmt.Sprintf("and %d more", len(alerts)-awsAlertDigestMax))
			break
		}
		lines = append(lines, alert.Issue.Title+" · "+issues.AlertWhere(alert))
	}
	content := map[string]interface{}{
		"AlertKind": awsIssueAlertKind, "IssueAlert": "digest", "Severity": "high",
		"AlertName":   awsScrub(digestTitle(len(alerts))),
		"ServiceName": "aws", "Service": "aws", "Source": awsIssueSource, "PatternID": "aws:high_digest", "Status": "firing",
	}
	utils.MarkInternalAlert(content)
	content["Lines"] = scrubLines(lines, awsAlertDigestMax+1)
	content["Notes"] = content["Lines"]
	content["Description"] = strings.Join(content["Lines"].([]string), "\n")
	if origin := awsAlertOrigin(publicHost); origin != "" {
		link := origin + "/aws/issues"
		content["IssueURL"] = link
		content["Links"] = []map[string]string{{"id": "issues", "label": "Open issues", "url": link}}
	}
	return content
}

// digestTitle counts the issues the digest holds: the ones that opened
// since the last alert, not every high issue of the hour.
func digestTitle(count int) string {
	if count == 1 {
		return "1 high issue opened"
	}
	return fmt.Sprintf("%d high issues opened", count)
}

func scrubLines(lines []string, limit int) []string {
	out := []string{}
	for _, line := range lines[:min(len(lines), limit)] {
		out = append(out, awsScrub(line))
	}
	return out
}

// awsAlertPublicHost is the configured public host; empty before the
// configuration is loaded, which leaves the alert without links rather
// than stopping the evaluation that sends it.
func awsAlertPublicHost() string {
	if cfg := config.GetConfigOrNil(); cfg != nil {
		return cfg.PublicHost
	}
	return ""
}

// awsAlertOrigin is the public origin links point at; empty when it is not
// a plain http(s) origin.
func awsAlertOrigin(publicHost string) string {
	u, err := url.Parse(publicHost)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil {
		return ""
	}
	return u.Scheme + "://" + u.Host
}
