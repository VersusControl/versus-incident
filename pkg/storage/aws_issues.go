package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/VersusControl/versus-incident/pkg/redaction"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
	"github.com/VersusControl/versus-incident/pkg/utils"
)

// AWSIssueRecord is one stored AWS issue: the columns the queue filters and
// sorts on, and the rest of the issue as an opaque payload the issue
// package encodes. Times are stored at table precision.
type AWSIssueRecord struct {
	// Org is the organization the row was read from. Writes ignore it.
	Org        string    `json:"org,omitempty"`
	ID         string    `json:"id"`
	Rule       string    `json:"rule"`
	Kind       string    `json:"kind"`
	Severity   string    `json:"severity"`
	Tier       string    `json:"tier"`
	State      string    `json:"state"`
	Confirm    string    `json:"confirm"`
	AccountID  string    `json:"account_id"`
	Region     string    `json:"region,omitempty"`
	Owner      string    `json:"owner"`
	GroupID    string    `json:"group_id,omitempty"`
	Title      string    `json:"title,omitempty"`
	Since      time.Time `json:"since,omitzero"`
	FirstSeen  time.Time `json:"first_seen"`
	OpenedAt   time.Time `json:"opened_at"`
	ResolvedAt time.Time `json:"resolved_at,omitzero"`
	// RejectedAt is set while the issue carries a rejection, also one it
	// keeps through a resolve. Retention never deletes such a row.
	RejectedAt time.Time `json:"rejected_at,omitzero"`
	// ReviewOn is the rejection's review date, YYYY-MM-DD, or empty.
	ReviewOn  string          `json:"review_on,omitempty"`
	Version   int64           `json:"version"`
	UpdatedAt time.Time       `json:"updated_at"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

// AWSIssueEvent is one thing that happened to an issue, and why.
type AWSIssueEvent struct {
	IssueID string    `json:"issue_id"`
	Seq     int64     `json:"seq"`
	At      time.Time `json:"at"`
	Kind    string    `json:"kind"`
	Actor   string    `json:"actor,omitempty"`
	// Detail says why. For a rejection it is the user's reason, plain
	// text.
	Detail string `json:"detail,omitempty"`
}

// AWS issue event kinds.
const (
	AWSIssueEventOpened       = "opened"
	AWSIssueEventAcknowledged = "acknowledged"
	AWSIssueEventRejected     = "rejected"
	AWSIssueEventReopened     = "reopened"
	AWSIssueEventResolved     = "resolved"
	AWSIssueEventRegressed    = "regressed"
	AWSIssueEventAlerted      = "alerted"
	// AWSIssueEventAlertGivenUp is an alert dropped unsent after its sends
	// kept failing; Detail names the alert and the last channel error.
	AWSIssueEventAlertGivenUp = "alert_given_up"
	AWSIssueEventReread       = "reread"
)

// AWSIssueStateRejected is the state of a rejected issue.
const AWSIssueStateRejected = "rejected"

// AWSIssueWrite is one issue written under compare-and-swap. Read is the
// version the writer read, from organization ReadOrg; zero means the
// writer saw no such issue anywhere in the read scope. Issue.Version must be
// Read+1. Events are added in the same transaction.
type AWSIssueWrite struct {
	Issue   AWSIssueRecord
	Read    int64
	ReadOrg string
	Events  []AWSIssueEvent
}

// AWSIssueBatch is one transaction: the writes of one account's evaluation
// or one user action, and the baselines that evaluation took. A baseline
// already stored is kept as it is.
type AWSIssueBatch struct {
	AccountID string
	Writes    []AWSIssueWrite
	Baselines []AWSIssueBaseline
}

// AWSIssueBaseline is when one rule was first assessed on one account, in
// one Region; Region is empty for an account-wide rule. A row with an
// empty Rule is a reset: every baseline of the account (Region empty) or
// of that Region taken before it no longer counts, so the next assessment
// there is a baseline again. A write keeps the later of the stored and the
// written time, and orders by the store, not the writer's clock: a reset
// lands after every baseline it covers, and a baseline after every reset
// that covers it, read in the same transaction.
type AWSIssueBaseline struct {
	AccountID       string    `json:"account_id"`
	Rule            string    `json:"rule"`
	Region          string    `json:"region,omitempty"`
	FirstAssessedAt time.Time `json:"first_assessed_at"`
}

// AWSIssueBaselineKey names one baseline.
type AWSIssueBaselineKey struct {
	AccountID, Rule, Region string
}

// Key returns the baseline's key.
func (baseline AWSIssueBaseline) Key() AWSIssueBaselineKey {
	return AWSIssueBaselineKey{AccountID: baseline.AccountID, Rule: baseline.Rule, Region: baseline.Region}
}

// AWSIssueQuery narrows an issue read. Every empty field matches all.
type AWSIssueQuery struct {
	AccountID string
	State     string
	// States matches an issue in any of them; with State set both apply.
	States   []string
	Severity string
	Kind     string
	GroupID  string
	IDs      []string
	// QueueOrder orders the issues as the queue lists them: most severe
	// first, then the newest opened, then by ID. Otherwise they come in
	// account and ID order.
	QueueOrder bool
	// Limit bounds the issues read; zero reads every one.
	Limit int
}

// AWSIssueCount is how many issues a query selects in one state, severity
// and tier.
type AWSIssueCount struct {
	State    string `json:"state"`
	Severity string `json:"severity"`
	Tier     string `json:"tier"`
	Count    int    `json:"count"`
}

// DefaultAWSIssueRetention is how long a resolved issue and its events are
// kept. A rejected issue and its events are kept for as long as it is
// rejected.
const DefaultAWSIssueRetention = 180 * 24 * time.Hour

// Bounds on a rejection reason, in characters.
const (
	MinAWSIssueReason = 3
	MaxAWSIssueReason = 500
)

// ErrInvalidAWSIssue is a malformed issue, event or query.
var ErrInvalidAWSIssue = errors.New("storage: invalid aws issue")

// ErrInvalidAWSIssueReason is a rejection reason that is not plain text of
// MinAWSIssueReason to MaxAWSIssueReason characters.
var ErrInvalidAWSIssueReason = errors.New("storage: a rejection reason must be plain text of 3 to 500 characters")

// ErrAWSIssueConflict is a write whose issue another writer changed after
// it was read. Nothing in the batch was written; the caller reads again.
var ErrAWSIssueConflict = errors.New("storage: aws issue changed since it was read")

// AWSIssueStore keeps the AWS issues, their events and the evaluated
// accounts. Reads merge the read scope: per issue ID the first organization
// in read order wins, so an issue rejected under the open-source default
// organization stays rejected after a licensed upgrade until the licensed
// organization writes it. Writes land in the write organization, one batch
// per transaction, each issue under compare-and-swap on its version. It is
// optional beside AWSInventoryStore; every backend with the inventory
// tables has it.
type AWSIssueStore interface {
	AWSIssues(ctx context.Context, scope tenancy.OrgScope, query AWSIssueQuery) ([]AWSIssueRecord, error)
	// AWSIssueCounts counts the issues a query selects per state, severity
	// and tier, without reading them. Order and limit are ignored.
	AWSIssueCounts(ctx context.Context, scope tenancy.OrgScope, query AWSIssueQuery) ([]AWSIssueCount, error)
	// AWSIssueEvents lists one issue's events from every organization in
	// the read scope, oldest first.
	AWSIssueEvents(ctx context.Context, scope tenancy.OrgScope, issueID string) ([]AWSIssueEvent, error)
	// AWSIssueBaselines lists the baselines and resets of one account, or
	// of every account when accountID is empty, merged across the read
	// scope: per key the latest time wins.
	AWSIssueBaselines(ctx context.Context, scope tenancy.OrgScope, accountID string) ([]AWSIssueBaseline, error)
	// WriteAWSIssues writes one batch atomically. When any issue changed
	// since it was read it writes nothing and answers ErrAWSIssueConflict.
	WriteAWSIssues(ctx context.Context, scope tenancy.OrgScope, batch AWSIssueBatch) error
	// PruneAWSIssues deletes the write organization's issues resolved
	// before before, with their events, and events whose issue is gone. It
	// never deletes an open, acknowledged, regressed or rejected issue, an
	// issue that carries a rejection, or any event of one.
	PruneAWSIssues(ctx context.Context, scope tenancy.OrgScope, before time.Time) (int64, error)
}

var (
	_ AWSIssueStore = (*sqlTables)(nil)
	_ AWSIssueStore = (*memoryTables)(nil)
)

// AWSIssuesOf returns the provider's issue store, if it has one.
func AWSIssuesOf(provider Provider) (AWSIssueStore, bool) {
	store, ok := provider.(AWSIssueStore)
	return store, ok
}

// reasonSecretPatterns are secrets people paste into a reason that neither
// the URL and AWS key pass nor the model-egress redactor catches: GitHub
// tokens, PEM blocks (also one cut off before its END line), a password
// assigned to a prefixed name such as DB_PASSWORD, and a password told in
// words. Each match is replaced by <REDACTED:name>, which no pattern
// matches again, so a cleaned reason cleans to itself.
var reasonSecretPatterns = []struct {
	name string
	re   *regexp.Regexp
}{
	{"pem", regexp.MustCompile(`-----BEGIN [A-Z0-9 ]{1,40}-----[\s\S]*?(?:-----END [A-Z0-9 ]{1,40}-----|\z)`)},
	{"github_token", regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,})`)},
	{"password", regexp.MustCompile(`(?i)\b[A-Za-z0-9][A-Za-z0-9_.-]*[_.-](?:password|passwd|pwd|secret|token|api[_-]?key)\s*[=:]\s*\S+`)},
	{"password", regexp.MustCompile(`(?i)\b(?:password|passwd|passphrase|pwd)\s+(?:is|was)\s+\S+`)},
}

// reasonRedactor is the redactor every text passes before it reaches a
// model; a reason is shown to the agent tools too.
var reasonRedactor = func() *redaction.Redactor {
	redactor, _ := redaction.NewRedactor(false, nil)
	return redactor
}()

// redactReason removes every secret a reason may carry: the reason's own
// patterns first, so a PEM block is taken whole, then the URL and AWS key
// pass, then the model-egress redactor.
func redactReason(reason string) string {
	for _, pattern := range reasonSecretPatterns {
		reason = pattern.re.ReplaceAllString(reason, "<REDACTED:"+pattern.name+">")
	}
	return reasonRedactor.Scrub(utils.RedactSecrets(reason))
}

// CleanAWSIssueReason returns the reason a user gave for a rejection as it
// is stored: secrets redacted, surrounding space trimmed. It must then be
// MinAWSIssueReason to MaxAWSIssueReason characters of plain text: no
// control or hidden format characters, and no line breaks other than a
// newline. The reason is shown as text everywhere, never as markup.
func CleanAWSIssueReason(reason string) (string, error) {
	if !utf8.ValidString(reason) {
		return "", ErrInvalidAWSIssueReason
	}
	reason = strings.TrimSpace(redactReason(strings.ReplaceAll(reason, "\r\n", "\n")))
	count := utf8.RuneCountInString(reason)
	if count < MinAWSIssueReason || count > MaxAWSIssueReason {
		return "", ErrInvalidAWSIssueReason
	}
	for _, line := range strings.Split(reason, "\n") {
		if !printableReason(line) {
			return "", ErrInvalidAWSIssueReason
		}
	}
	return reason, nil
}

// printableReason is printableName, except that it keeps the zero-width
// joiner (U+200D) and the variation selectors (U+FE0E, U+FE0F) that joined
// emoji such as 👩‍💻 are made of. Bidirectional overrides and isolates,
// other format characters and control characters stay refused.
func printableReason(line string) bool {
	for _, r := range line {
		if r == '\u200d' || r == '\ufe0e' || r == '\ufe0f' {
			continue
		}
		if unicode.In(r, unicode.Cc, unicode.Cf, unicode.Zl, unicode.Zp) {
			return false
		}
	}
	return true
}

var (
	awsIssueIDPattern   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	awsIssueRulePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,127}$`)
	awsIssueDayPattern  = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`)
	awsIssueKinds       = map[string]bool{"reliability": true, "security": true}
	awsIssueSeverities  = map[string]bool{"critical": true, "high": true, "medium": true, "low": true}
	awsIssueTiers       = map[string]bool{"act_now": true, "worth_a_look": true}
	awsIssueStates      = map[string]bool{"open": true, "acknowledged": true, AWSIssueStateRejected: true, "resolved": true, "regressed": true}
	awsIssueConfirms    = map[string]bool{"confirmed": true, "cant_confirm": true}
	awsIssueEventKinds  = map[string]bool{
		AWSIssueEventOpened: true, AWSIssueEventAcknowledged: true, AWSIssueEventRejected: true, AWSIssueEventReopened: true,
		AWSIssueEventResolved: true, AWSIssueEventRegressed: true, AWSIssueEventAlerted: true, AWSIssueEventAlertGivenUp: true, AWSIssueEventReread: true,
	}
)

// maxAWSIssuePayload bounds an issue's encoded payload.
const maxAWSIssuePayload = 256 << 10

// maxAWSIssueEventsPerWrite bounds the events one write adds to an issue;
// it also spaces the event sequence numbers of successive versions.
const maxAWSIssueEventsPerWrite = 1000

func validAWSIssueDay(day string) bool {
	if day == "" {
		return true
	}
	if !awsIssueDayPattern.MatchString(day) {
		return false
	}
	_, err := time.Parse(time.DateOnly, day)
	return err == nil
}

// cleanAWSIssueRecord validates an issue for writing and stores its times
// at table precision.
func cleanAWSIssueRecord(record AWSIssueRecord) (AWSIssueRecord, error) {
	// Each refusal names its cause, so a writer that drops a bad issue
	// can say why.
	refuse := func(why string) (AWSIssueRecord, error) {
		return AWSIssueRecord{}, fmt.Errorf("%w: %s", ErrInvalidAWSIssue, why)
	}
	switch {
	case !awsIssueIDPattern.MatchString(record.ID):
		return refuse("malformed issue ID")
	case !awsIssueRulePattern.MatchString(record.Rule):
		return refuse("malformed rule ID")
	case !awsIssueKinds[record.Kind], !awsIssueSeverities[record.Severity], !awsIssueTiers[record.Tier],
		!awsIssueStates[record.State], !awsIssueConfirms[record.Confirm]:
		return refuse("unknown kind, severity, tier, state or confirm value")
	case !awsAccountIDPattern.MatchString(record.AccountID):
		return refuse("malformed account ID")
	case record.Region != "" && (!validTableKey(record.Region) || len(record.Region) > 64):
		return refuse("malformed Region")
	case !validTableKey(record.Owner):
		return refuse("malformed owner")
	case record.GroupID != "" && !validAWSGroupID(record.GroupID):
		return refuse("malformed group ID")
	case !validTableStrings(record.Title) || utf8.RuneCountInString(record.Title) > 1000:
		return refuse("title over 1000 characters or not text")
	case record.FirstSeen.IsZero(), record.OpenedAt.IsZero(), record.UpdatedAt.IsZero(), record.Version <= 0:
		return refuse("missing time or version")
	case !validAWSIssueDay(record.ReviewOn), record.State == AWSIssueStateRejected && record.RejectedAt.IsZero(),
		record.ReviewOn != "" && record.RejectedAt.IsZero():
		return refuse("malformed rejection")
	case len(record.Payload) > maxAWSIssuePayload:
		return refuse("evidence over the size limit")
	case !validTableJSON(record.Payload):
		return refuse("evidence is not valid JSON text")
	}
	record.Org = ""
	record.Since = tableTime(record.Since)
	record.FirstSeen = tableTime(record.FirstSeen)
	record.OpenedAt = tableTime(record.OpenedAt)
	record.ResolvedAt = tableTime(record.ResolvedAt)
	record.RejectedAt = tableTime(record.RejectedAt)
	record.UpdatedAt = tableTime(record.UpdatedAt)
	if emptyJSON(record.Payload) {
		record.Payload = nil
	}
	return record, nil
}

// cleanAWSIssueEvents validates one write's events and numbers them after
// the issue's version.
func cleanAWSIssueEvents(issue AWSIssueRecord, events []AWSIssueEvent) ([]AWSIssueEvent, error) {
	if len(events) >= maxAWSIssueEventsPerWrite {
		return nil, ErrInvalidAWSIssue
	}
	out := make([]AWSIssueEvent, 0, len(events))
	for index, event := range events {
		if !awsIssueEventKinds[event.Kind] || event.At.IsZero() || !validTableStrings(event.Actor, event.Detail) ||
			len(event.Actor) > 512 || utf8.RuneCountInString(event.Detail) > 4000 {
			return nil, ErrInvalidAWSIssue
		}
		if event.Kind == AWSIssueEventRejected {
			// The stored reason is exactly what CleanAWSIssueReason
			// returns, so nothing unredacted or unbounded reaches the
			// table by another path.
			clean, err := CleanAWSIssueReason(event.Detail)
			if err != nil {
				return nil, err
			}
			if clean != event.Detail {
				return nil, ErrInvalidAWSIssueReason
			}
		}
		event.IssueID = issue.ID
		event.Seq = issue.Version*maxAWSIssueEventsPerWrite + int64(index)
		event.At = tableTime(event.At)
		out = append(out, event)
	}
	return out, nil
}

// cleanAWSIssueBatch validates a batch. Every issue belongs to the batch's
// account and appears once.
func cleanAWSIssueBatch(scope tenancy.OrgScope, batch AWSIssueBatch) (AWSIssueBatch, error) {
	if !awsAccountIDPattern.MatchString(batch.AccountID) {
		return AWSIssueBatch{}, ErrInvalidAWSIssue
	}
	baselines := make([]AWSIssueBaseline, 0, len(batch.Baselines))
	for _, baseline := range batch.Baselines {
		if baseline.AccountID != batch.AccountID || baseline.Rule != "" && !awsIssueRulePattern.MatchString(baseline.Rule) ||
			baseline.Region != "" && (!validTableKey(baseline.Region) || len(baseline.Region) > 64) || baseline.FirstAssessedAt.IsZero() {
			return AWSIssueBatch{}, ErrInvalidAWSIssue
		}
		baseline.FirstAssessedAt = tableTime(baseline.FirstAssessedAt)
		baselines = append(baselines, baseline)
	}
	batch.Baselines = baselines
	seen := map[string]bool{}
	writes := make([]AWSIssueWrite, 0, len(batch.Writes))
	for _, write := range batch.Writes {
		issue, err := cleanAWSIssueRecord(write.Issue)
		if err != nil {
			return AWSIssueBatch{}, err
		}
		if issue.AccountID != batch.AccountID || seen[issue.ID] || write.Read < 0 || issue.Version != write.Read+1 {
			return AWSIssueBatch{}, ErrInvalidAWSIssue
		}
		if write.Read == 0 {
			write.ReadOrg = ""
		} else if !inScope(scope, write.ReadOrg) {
			return AWSIssueBatch{}, ErrInvalidAWSIssue
		}
		seen[issue.ID] = true
		events, err := cleanAWSIssueEvents(issue, write.Events)
		if err != nil {
			return AWSIssueBatch{}, err
		}
		write.Issue, write.Events = issue, events
		writes = append(writes, write)
	}
	batch.Writes = writes
	return batch, nil
}

func inScope(scope tenancy.OrgScope, org string) bool {
	for _, read := range scope.Read {
		if read == org {
			return true
		}
	}
	return false
}

// ValidAWSIssueQuery reports whether the store accepts a query's values.
func ValidAWSIssueQuery(query AWSIssueQuery) bool { return validAWSIssueQuery(query) }

func validAWSIssueQuery(query AWSIssueQuery) bool {
	if query.AccountID != "" && !awsAccountIDPattern.MatchString(query.AccountID) ||
		query.State != "" && !awsIssueStates[query.State] || query.Severity != "" && !awsIssueSeverities[query.Severity] ||
		query.Kind != "" && !awsIssueKinds[query.Kind] || query.GroupID != "" && !validAWSGroupID(query.GroupID) || query.Limit < 0 {
		return false
	}
	for _, state := range query.States {
		if !awsIssueStates[state] {
			return false
		}
	}
	for _, id := range query.IDs {
		if !awsIssueIDPattern.MatchString(id) {
			return false
		}
	}
	return true
}

func (query AWSIssueQuery) matches(record AWSIssueRecord) bool {
	if query.AccountID != "" && record.AccountID != query.AccountID || query.State != "" && record.State != query.State ||
		query.Severity != "" && record.Severity != query.Severity || query.Kind != "" && record.Kind != query.Kind ||
		query.GroupID != "" && record.GroupID != query.GroupID {
		return false
	}
	if len(query.States) > 0 && !slices.Contains(query.States, record.State) {
		return false
	}
	if len(query.IDs) == 0 {
		return true
	}
	for _, id := range query.IDs {
		if id == record.ID {
			return true
		}
	}
	return false
}

// sortAWSIssues orders issues by account, then ID, on every backend.
func sortAWSIssues(records []AWSIssueRecord) {
	sort.Slice(records, func(left, right int) bool {
		a, b := records[left], records[right]
		if a.AccountID != b.AccountID {
			return a.AccountID < b.AccountID
		}
		return a.ID < b.ID
	})
}

// awsIssueSeverityRank orders severities, most severe highest.
var awsIssueSeverityRank = map[string]int{"critical": 4, "high": 3, "medium": 2, "low": 1}

// sortAWSIssuesQueue orders issues as the queue lists them: most severe
// first, then the newest opened, then by ID.
func sortAWSIssuesQueue(records []AWSIssueRecord) {
	sort.Slice(records, func(left, right int) bool {
		a, b := records[left], records[right]
		if awsIssueSeverityRank[a.Severity] != awsIssueSeverityRank[b.Severity] {
			return awsIssueSeverityRank[a.Severity] > awsIssueSeverityRank[b.Severity]
		}
		if !a.OpenedAt.Equal(b.OpenedAt) {
			return a.OpenedAt.After(b.OpenedAt)
		}
		return a.ID < b.ID
	})
}

// countAWSIssues folds records into counts per state, severity and tier,
// in a fixed order.
func countAWSIssues(records []AWSIssueRecord) []AWSIssueCount {
	type key struct{ state, severity, tier string }
	counts := map[key]int{}
	for _, record := range records {
		counts[key{record.State, record.Severity, record.Tier}]++
	}
	out := make([]AWSIssueCount, 0, len(counts))
	for k, count := range counts {
		out = append(out, AWSIssueCount{State: k.state, Severity: k.severity, Tier: k.tier, Count: count})
	}
	sortAWSIssueCounts(out)
	return out
}

func sortAWSIssueCounts(counts []AWSIssueCount) {
	sort.Slice(counts, func(left, right int) bool {
		a, b := counts[left], counts[right]
		if a.State != b.State {
			return a.State < b.State
		}
		if a.Severity != b.Severity {
			return a.Severity < b.Severity
		}
		return a.Tier < b.Tier
	})
}

func sortAWSIssueEvents(events []AWSIssueEvent) {
	sort.SliceStable(events, func(left, right int) bool {
		a, b := events[left], events[right]
		if !a.At.Equal(b.At) {
			return a.At.Before(b.At)
		}
		return a.Seq < b.Seq
	})
}

// mergeAWSIssueBaselines folds the baselines of every read organization:
// per key the latest time, so a reset or a baseline taken after a reset
// in the write organization wins over an older one inherited.
func mergeAWSIssueBaselines(baselines []AWSIssueBaseline) []AWSIssueBaseline {
	folded := map[AWSIssueBaselineKey]AWSIssueBaseline{}
	for _, baseline := range baselines {
		if kept, ok := folded[baseline.Key()]; ok && kept.FirstAssessedAt.After(baseline.FirstAssessedAt) {
			continue
		}
		folded[baseline.Key()] = baseline
	}
	out := make([]AWSIssueBaseline, 0, len(folded))
	for _, baseline := range folded {
		out = append(out, baseline)
	}
	sort.Slice(out, func(left, right int) bool {
		a, b := out[left], out[right]
		if a.AccountID != b.AccountID {
			return a.AccountID < b.AccountID
		}
		if a.Rule != b.Rule {
			return a.Rule < b.Rule
		}
		return a.Region < b.Region
	})
	return out
}

// CheckAWSIssueRecord reports whether the store would accept record, so a
// writer can drop one bad issue instead of failing the whole batch.
func CheckAWSIssueRecord(record AWSIssueRecord) error {
	_, err := cleanAWSIssueRecord(record)
	return err
}

// ---- SQL (Postgres and the local SQLite tables) ----

// awsIssueColumns are the issue columns in scan order.
var awsIssueColumns = []string{"org_id", "id", "rule", "kind", "severity", "tier", "state", "confirm", "account_id", "region", "owner",
	"group_id", "title", "since", "first_seen", "opened_at", "resolved_at", "rejected_at", "review_on", "version", "updated_at", "payload"}

// awsIssueSelect renders the issue columns of alias, with the review date
// read as text on every backend.
func (t *sqlTables) awsIssueSelect(alias string) string {
	fields := make([]string, len(awsIssueColumns))
	for index, column := range awsIssueColumns {
		fields[index] = alias + "." + column
		if column == "review_on" {
			fields[index] = t.dayColumn(fields[index])
		}
	}
	return strings.Join(fields, ", ")
}

func scanAWSIssue(rows *sql.Rows) (AWSIssueRecord, error) {
	var record AWSIssueRecord
	var since, firstSeen, opened, resolved, rejected, updated tableTimeValue
	var reviewOn, payload sql.NullString
	if err := rows.Scan(&record.Org, &record.ID, &record.Rule, &record.Kind, &record.Severity, &record.Tier, &record.State,
		&record.Confirm, &record.AccountID, &record.Region, &record.Owner, &record.GroupID, &record.Title,
		&since, &firstSeen, &opened, &resolved, &rejected, &reviewOn, &record.Version, &updated, &payload); err != nil {
		return AWSIssueRecord{}, err
	}
	record.Since, record.FirstSeen, record.OpenedAt = since.Time, firstSeen.Time, opened.Time
	record.ResolvedAt, record.RejectedAt, record.UpdatedAt = resolved.Time, rejected.Time, updated.Time
	if reviewOn.Valid {
		record.ReviewOn = reviewOn.String
	}
	if payload.Valid && payload.String != "" {
		record.Payload = json.RawMessage(payload.String)
	}
	return record, nil
}

func (t *sqlTables) AWSIssues(ctx context.Context, scope tenancy.OrgScope, query AWSIssueQuery) ([]AWSIssueRecord, error) {
	if !validAWSIssueQuery(query) {
		return nil, ErrInvalidAWSIssue
	}
	db, err := t.tablesDB()
	if err != nil {
		return nil, err
	}
	where := awsIssueWhere(inventoryScope(scope), query)
	order := ` ORDER BY i.account_id, i.id`
	if query.QueueOrder {
		order = ` ORDER BY CASE i.severity WHEN 'critical' THEN 4 WHEN 'high' THEN 3 WHEN 'medium' THEN 2 WHEN 'low' THEN 1 ELSE 0 END DESC,` +
			` i.opened_at DESC, i.id`
	}
	if query.Limit > 0 {
		order += ` LIMIT ` + strconv.Itoa(query.Limit)
	}
	rows, err := db.QueryContext(ctx, t.q(`SELECT `+t.awsIssueSelect("i")+` FROM aws_issues i WHERE `+where.String()+order), where.args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]AWSIssueRecord, 0)
	for rows.Next() {
		record, err := scanAWSIssue(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, record)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// awsIssueWhere renders a query's filters over the issues visible in the
// read scope, aliased i.
func awsIssueWhere(scope tenancy.OrgScope, query AWSIssueQuery) sqlWhere {
	visibleRow, visibleArgs := visible(scope.Read, "i", "aws_issues", "id")
	var where sqlWhere
	where.add(visibleRow, visibleArgs...)
	where.equal("i.account_id", query.AccountID)
	where.equal("i.state", query.State)
	where.in("i.state", query.States)
	where.equal("i.severity", query.Severity)
	where.equal("i.kind", query.Kind)
	where.equal("i.group_id", query.GroupID)
	where.in("i.id", query.IDs)
	return where
}

func (t *sqlTables) AWSIssueCounts(ctx context.Context, scope tenancy.OrgScope, query AWSIssueQuery) ([]AWSIssueCount, error) {
	if !validAWSIssueQuery(query) {
		return nil, ErrInvalidAWSIssue
	}
	db, err := t.tablesDB()
	if err != nil {
		return nil, err
	}
	where := awsIssueWhere(inventoryScope(scope), query)
	rows, err := db.QueryContext(ctx, t.q(`SELECT i.state, i.severity, i.tier, COUNT(*) FROM aws_issues i WHERE `+where.String()+
		` GROUP BY i.state, i.severity, i.tier`), where.args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]AWSIssueCount, 0)
	for rows.Next() {
		var count AWSIssueCount
		if err := rows.Scan(&count.State, &count.Severity, &count.Tier, &count.Count); err != nil {
			return nil, err
		}
		out = append(out, count)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sortAWSIssueCounts(out)
	return out, nil
}

func (t *sqlTables) AWSIssueEvents(ctx context.Context, scope tenancy.OrgScope, issueID string) ([]AWSIssueEvent, error) {
	if !awsIssueIDPattern.MatchString(issueID) {
		return nil, ErrInvalidAWSIssue
	}
	db, err := t.tablesDB()
	if err != nil {
		return nil, err
	}
	scope = inventoryScope(scope)
	rows, err := db.QueryContext(ctx, t.q(`SELECT issue_id, seq, at, kind, actor, detail FROM aws_issue_events
 WHERE org_id IN (`+placeholders(len(scope.Read))+`) AND issue_id = ? ORDER BY at, seq`), append(stringArgs(scope.Read), issueID)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]AWSIssueEvent, 0)
	for rows.Next() {
		var event AWSIssueEvent
		var at tableTimeValue
		if err := rows.Scan(&event.IssueID, &event.Seq, &at, &event.Kind, &event.Actor, &event.Detail); err != nil {
			return nil, err
		}
		event.At = at.Time
		out = append(out, event)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sortAWSIssueEvents(out)
	return out, nil
}

func (t *sqlTables) AWSIssueBaselines(ctx context.Context, scope tenancy.OrgScope, accountID string) ([]AWSIssueBaseline, error) {
	if accountID != "" && !awsAccountIDPattern.MatchString(accountID) {
		return nil, ErrInvalidAWSIssue
	}
	db, err := t.tablesDB()
	if err != nil {
		return nil, err
	}
	scope = inventoryScope(scope)
	var where sqlWhere
	where.add(`org_id IN (`+placeholders(len(scope.Read))+`)`, stringArgs(scope.Read)...)
	where.equal("account_id", accountID)
	rows, err := db.QueryContext(ctx, t.q(`SELECT account_id, rule, region, first_assessed_at FROM aws_issue_baselines WHERE `+where.String()), where.args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var baselines []AWSIssueBaseline
	for rows.Next() {
		var baseline AWSIssueBaseline
		var first tableTimeValue
		if err := rows.Scan(&baseline.AccountID, &baseline.Rule, &baseline.Region, &first); err != nil {
			return nil, err
		}
		baseline.FirstAssessedAt = first.Time
		baselines = append(baselines, baseline)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return mergeAWSIssueBaselines(baselines), nil
}

func (t *sqlTables) WriteAWSIssues(ctx context.Context, scope tenancy.OrgScope, batch AWSIssueBatch) error {
	scope = inventoryScope(scope)
	batch, err := cleanAWSIssueBatch(scope, batch)
	if err != nil {
		return err
	}
	return t.write(ctx, func(tx *sql.Tx) error {
		for _, write := range batch.Writes {
			if err := t.writeAWSIssueTx(ctx, tx, scope, write); err != nil {
				return err
			}
		}
		statement := t.q(`INSERT INTO aws_issue_baselines (org_id, account_id, rule, region, first_assessed_at)
 VALUES (?, ?, ?, ?, ?) ON CONFLICT (org_id, account_id, rule, region) DO UPDATE
 SET first_assessed_at = CASE WHEN excluded.first_assessed_at > aws_issue_baselines.first_assessed_at
 THEN excluded.first_assessed_at ELSE aws_issue_baselines.first_assessed_at END`)
		for _, baseline := range batch.Baselines {
			newest, err := t.newestOpposingBaselineTx(ctx, tx, scope, baseline)
			if err != nil {
				return err
			}
			at := orderedBaselineTime(baseline.FirstAssessedAt, newest)
			if _, err := tx.ExecContext(ctx, statement, scope.Write, baseline.AccountID, baseline.Rule, baseline.Region, t.at(at)); err != nil {
				return err
			}
		}
		return nil
	})
}

// newestOpposingBaselineTx reads, in the write transaction, the newest
// row a baseline must be ordered after: for a reset, the newest baseline it
// covers (the account's, or its Region's); for a baseline, the newest reset
// that covers it (the account's or its Region's).
func (t *sqlTables) newestOpposingBaselineTx(ctx context.Context, tx *sql.Tx, scope tenancy.OrgScope, baseline AWSIssueBaseline) (time.Time, error) {
	var where sqlWhere
	where.add(`org_id IN (`+placeholders(len(scope.Read))+`)`, stringArgs(scope.Read)...)
	where.add("account_id = ?", baseline.AccountID)
	if baseline.Rule == "" {
		where.add("rule <> ''")
		where.equal("region", baseline.Region)
	} else {
		where.add("rule = ''")
		where.add("(region = '' OR region = ?)", baseline.Region)
	}
	var newest tableTimeValue
	err := tx.QueryRowContext(ctx, t.q(`SELECT MAX(first_assessed_at) FROM aws_issue_baselines WHERE `+where.String()), where.args...).Scan(&newest)
	return newest.Time, err
}

func (t *sqlTables) writeAWSIssueTx(ctx context.Context, tx *sql.Tx, scope tenancy.OrgScope, write AWSIssueWrite) error {
	issue := write.Issue
	switch {
	case write.Read == 0:
		// The writer saw no such issue: none may exist in any organization
		// it reads.
		var held int
		if err := tx.QueryRowContext(ctx, t.q(`SELECT COUNT(*) FROM aws_issues WHERE org_id IN (`+placeholders(len(scope.Read))+`) AND id = ?`),
			append(stringArgs(scope.Read), issue.ID)...).Scan(&held); err != nil {
			return err
		}
		if held > 0 {
			return ErrAWSIssueConflict
		}
	case write.ReadOrg != scope.Write:
		// The writer read an inherited issue: the licensed copy it makes
		// must start from the version it read, and none may exist yet.
		var version int64
		err := tx.QueryRowContext(ctx, t.q(`SELECT version FROM aws_issues WHERE org_id = ? AND id = ?`), write.ReadOrg, issue.ID).Scan(&version)
		if errors.Is(err, sql.ErrNoRows) || err == nil && version != write.Read {
			return ErrAWSIssueConflict
		}
		if err != nil {
			return err
		}
	default:
		result, err := tx.ExecContext(ctx, t.q(`UPDATE aws_issues SET rule = ?, kind = ?, severity = ?, tier = ?, state = ?, confirm = ?,
 region = ?, owner = ?, group_id = ?, title = ?, since = ?, first_seen = ?, opened_at = ?, resolved_at = ?, rejected_at = ?, review_on = ?,
 version = ?, updated_at = ?, payload = ? WHERE org_id = ? AND id = ? AND account_id = ? AND version = ?`),
			issue.Rule, issue.Kind, issue.Severity, issue.Tier, issue.State, issue.Confirm, issue.Region, issue.Owner, issue.GroupID,
			issue.Title, t.at(issue.Since), t.at(issue.FirstSeen), t.at(issue.OpenedAt), t.at(issue.ResolvedAt), t.at(issue.RejectedAt),
			optionalDay(issue.ReviewOn), issue.Version, t.at(issue.UpdatedAt), jsonParam(issue.Payload),
			scope.Write, issue.ID, issue.AccountID, write.Read)
		if err != nil {
			return err
		}
		if count, err := result.RowsAffected(); err != nil || count != 1 {
			if err != nil {
				return err
			}
			return ErrAWSIssueConflict
		}
		return t.insertAWSIssueEventsTx(ctx, tx, scope.Write, write.Events)
	}
	result, err := tx.ExecContext(ctx, t.q(`INSERT INTO aws_issues (org_id, id, rule, kind, severity, tier, state, confirm, account_id, region,
 owner, group_id, title, since, first_seen, opened_at, resolved_at, rejected_at, review_on, version, updated_at, payload)
 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (org_id, id) DO NOTHING`),
		scope.Write, issue.ID, issue.Rule, issue.Kind, issue.Severity, issue.Tier, issue.State, issue.Confirm, issue.AccountID,
		issue.Region, issue.Owner, issue.GroupID, issue.Title, t.at(issue.Since), t.at(issue.FirstSeen), t.at(issue.OpenedAt),
		t.at(issue.ResolvedAt), t.at(issue.RejectedAt), optionalDay(issue.ReviewOn), issue.Version, t.at(issue.UpdatedAt),
		jsonParam(issue.Payload))
	if err != nil {
		return err
	}
	if count, err := result.RowsAffected(); err != nil || count != 1 {
		if err != nil {
			return err
		}
		return ErrAWSIssueConflict
	}
	return t.insertAWSIssueEventsTx(ctx, tx, scope.Write, write.Events)
}

func (t *sqlTables) insertAWSIssueEventsTx(ctx context.Context, tx *sql.Tx, org string, events []AWSIssueEvent) error {
	statement := t.q(`INSERT INTO aws_issue_events (org_id, issue_id, seq, at, kind, actor, detail) VALUES (?, ?, ?, ?, ?, ?, ?)`)
	for _, event := range events {
		if _, err := tx.ExecContext(ctx, statement, org, event.IssueID, event.Seq, t.at(event.At), event.Kind, event.Actor, event.Detail); err != nil {
			return err
		}
	}
	return nil
}

func (t *sqlTables) PruneAWSIssues(ctx context.Context, scope tenancy.OrgScope, before time.Time) (int64, error) {
	if before.IsZero() {
		return 0, nil
	}
	scope = inventoryScope(scope)
	cutoff := t.at(before)
	var removed int64
	for {
		// An event goes only with its issue: one whose row is gone, or
		// that resolved before the cutoff with no rejection. An open,
		// acknowledged, regressed or rejected issue keeps every event.
		batch, err := t.sweepDelete(ctx, "aws_issue_events", `org_id = ? AND at < ? AND NOT EXISTS (SELECT 1 FROM aws_issues i
 WHERE i.org_id = aws_issue_events.org_id AND i.id = aws_issue_events.issue_id
 AND NOT (i.state = 'resolved' AND i.rejected_at IS NULL AND i.resolved_at < ?))`,
			[]any{scope.Write, cutoff, cutoff}, DefaultAWSRetentionBatch)
		if err != nil {
			return removed, err
		}
		removed += batch
		if batch < DefaultAWSRetentionBatch {
			break
		}
	}
	for {
		batch, err := t.sweepDelete(ctx, "aws_issues", `org_id = ? AND state = 'resolved' AND rejected_at IS NULL AND resolved_at < ?`,
			[]any{scope.Write, cutoff}, DefaultAWSRetentionBatch)
		if err != nil {
			return removed, err
		}
		removed += batch
		if batch < DefaultAWSRetentionBatch {
			return removed, nil
		}
	}
}

// ---- Memory ----

func (t *memoryTables) AWSIssues(_ context.Context, scope tenancy.OrgScope, query AWSIssueQuery) ([]AWSIssueRecord, error) {
	if !validAWSIssueQuery(query) {
		return nil, ErrInvalidAWSIssue
	}
	out := t.matchingAWSIssues(inventoryScope(scope), query, true)
	if query.QueueOrder {
		sortAWSIssuesQueue(out)
	} else {
		sortAWSIssues(out)
	}
	if query.Limit > 0 && len(out) > query.Limit {
		out = out[:query.Limit]
	}
	return out, nil
}

func (t *memoryTables) AWSIssueCounts(_ context.Context, scope tenancy.OrgScope, query AWSIssueQuery) ([]AWSIssueCount, error) {
	if !validAWSIssueQuery(query) {
		return nil, ErrInvalidAWSIssue
	}
	return countAWSIssues(t.matchingAWSIssues(inventoryScope(scope), query, false)), nil
}

// matchingAWSIssues lists the visible issues a query selects, unordered;
// withPayload copies each payload out, otherwise it is left off.
func (t *memoryTables) matchingAWSIssues(scope tenancy.OrgScope, query AWSIssueQuery, withPayload bool) []AWSIssueRecord {
	t.mu.RLock()
	defer t.mu.RUnlock()
	seen := map[string]bool{}
	out := make([]AWSIssueRecord, 0)
	for _, org := range scope.Read {
		for id, record := range t.issues[org] {
			if seen[id] {
				continue
			}
			seen[id] = true
			if query.matches(record) {
				record.Org = org
				record.Payload = nil
				if payload := t.issues[org][id].Payload; withPayload && len(payload) > 0 {
					record.Payload = append(json.RawMessage(nil), payload...)
				}
				out = append(out, record)
			}
		}
	}
	return out
}

func (t *memoryTables) AWSIssueEvents(_ context.Context, scope tenancy.OrgScope, issueID string) ([]AWSIssueEvent, error) {
	if !awsIssueIDPattern.MatchString(issueID) {
		return nil, ErrInvalidAWSIssue
	}
	scope = inventoryScope(scope)
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]AWSIssueEvent, 0)
	for _, org := range scope.Read {
		out = append(out, t.issueEvents[org][issueID]...)
	}
	sortAWSIssueEvents(out)
	return out, nil
}

func (t *memoryTables) AWSIssueBaselines(_ context.Context, scope tenancy.OrgScope, accountID string) ([]AWSIssueBaseline, error) {
	if accountID != "" && !awsAccountIDPattern.MatchString(accountID) {
		return nil, ErrInvalidAWSIssue
	}
	scope = inventoryScope(scope)
	t.mu.RLock()
	defer t.mu.RUnlock()
	var baselines []AWSIssueBaseline
	for _, org := range scope.Read {
		for _, baseline := range t.issueBaselines[org] {
			if accountID == "" || baseline.AccountID == accountID {
				baselines = append(baselines, baseline)
			}
		}
	}
	return mergeAWSIssueBaselines(baselines), nil
}

func (t *memoryTables) WriteAWSIssues(_ context.Context, scope tenancy.OrgScope, batch AWSIssueBatch) error {
	scope = inventoryScope(scope)
	batch, err := cleanAWSIssueBatch(scope, batch)
	if err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	// Check every write before changing anything, so a conflict writes
	// nothing.
	for _, write := range batch.Writes {
		id := write.Issue.ID
		own, owned := t.issues[scope.Write][id]
		switch {
		case write.Read == 0:
			for _, org := range scope.Read {
				if _, ok := t.issues[org][id]; ok {
					return ErrAWSIssueConflict
				}
			}
		case write.ReadOrg != scope.Write:
			inherited, ok := t.issues[write.ReadOrg][id]
			if owned || !ok || inherited.Version != write.Read {
				return ErrAWSIssueConflict
			}
		default:
			if !owned || own.Version != write.Read || own.AccountID != write.Issue.AccountID {
				return ErrAWSIssueConflict
			}
		}
	}
	if t.issues == nil {
		t.issues = map[string]map[string]AWSIssueRecord{}
		t.issueEvents = map[string]map[string][]AWSIssueEvent{}
		t.issueBaselines = map[string]map[AWSIssueBaselineKey]AWSIssueBaseline{}
	}
	if t.issues[scope.Write] == nil {
		t.issues[scope.Write] = map[string]AWSIssueRecord{}
		t.issueEvents[scope.Write] = map[string][]AWSIssueEvent{}
		t.issueBaselines[scope.Write] = map[AWSIssueBaselineKey]AWSIssueBaseline{}
	}
	for _, write := range batch.Writes {
		record := write.Issue
		record.Payload = append(json.RawMessage(nil), record.Payload...)
		t.issues[scope.Write][record.ID] = record
		t.issueEvents[scope.Write][record.ID] = append(t.issueEvents[scope.Write][record.ID], write.Events...)
	}
	for _, baseline := range batch.Baselines {
		var newest time.Time
		for _, org := range scope.Read {
			for key, held := range t.issueBaselines[org] {
				if key.AccountID == baseline.AccountID && opposes(baseline, key) && held.FirstAssessedAt.After(newest) {
					newest = held.FirstAssessedAt
				}
			}
		}
		baseline.FirstAssessedAt = orderedBaselineTime(baseline.FirstAssessedAt, newest)
		if kept, ok := t.issueBaselines[scope.Write][baseline.Key()]; !ok || baseline.FirstAssessedAt.After(kept.FirstAssessedAt) {
			t.issueBaselines[scope.Write][baseline.Key()] = baseline
		}
	}
	return nil
}

func (t *memoryTables) PruneAWSIssues(_ context.Context, scope tenancy.OrgScope, before time.Time) (int64, error) {
	if before.IsZero() {
		return 0, nil
	}
	scope = inventoryScope(scope)
	before = tableTime(before)
	t.mu.Lock()
	defer t.mu.Unlock()
	var removed int64
	for id, events := range t.issueEvents[scope.Write] {
		if record, ok := t.issues[scope.Write][id]; ok && !prunableAWSIssue(record, before) {
			continue
		}
		kept := events[:0]
		for _, event := range events {
			if event.At.Before(before) {
				removed++
				continue
			}
			kept = append(kept, event)
		}
		if len(kept) == 0 {
			delete(t.issueEvents[scope.Write], id)
		} else {
			t.issueEvents[scope.Write][id] = kept
		}
	}
	for id, record := range t.issues[scope.Write] {
		if prunableAWSIssue(record, before) {
			delete(t.issues[scope.Write], id)
			removed++
		}
	}
	return removed, nil
}

// prunableAWSIssue reports whether retention deletes an issue: resolved
// before the cutoff and carrying no rejection.
func prunableAWSIssue(record AWSIssueRecord, before time.Time) bool {
	return record.State == "resolved" && record.RejectedAt.IsZero() && !record.ResolvedAt.IsZero() && record.ResolvedAt.Before(before)
}

// opposes reports whether a stored row is one baseline must be ordered
// after: a covered baseline for a reset, a covering reset for a baseline.
func opposes(baseline AWSIssueBaseline, held AWSIssueBaselineKey) bool {
	if baseline.Rule == "" {
		return held.Rule != "" && (baseline.Region == "" || held.Region == baseline.Region)
	}
	return held.Rule == "" && (held.Region == "" || held.Region == baseline.Region)
}

// orderedBaselineTime orders a reset after every baseline it covers, and a
// baseline after every reset that covers it, whatever the writing
// replica's clock says: at least a microsecond after the newest opposing
// row.
func orderedBaselineTime(at, newest time.Time) time.Time {
	if newest.IsZero() || at.After(newest) {
		return at
	}
	return tableTime(newest.Add(time.Microsecond))
}
