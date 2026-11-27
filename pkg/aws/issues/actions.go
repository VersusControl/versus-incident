package issues

import (
	"context"
	"errors"
	"time"

	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

// MaxBulkReject bounds the issues one bulk reject names.
const MaxBulkReject = 200

// maxActionRetries bounds how often a user action is applied again after
// an evaluation changed its issue first.
const maxActionRetries = 3

// Action errors.
var (
	// ErrConflict is an action whose issue kept changing under it.
	ErrConflict = errors.New("issues: the issue changed; read it again")
	// ErrTooMany is a bulk reject of more than MaxBulkReject issues.
	ErrTooMany = errors.New("issues: too many issues in one bulk reject")
)

// Bulk reject results, one per ID.
const (
	BulkRejected        = "rejected"
	BulkAlreadyRejected = "already_rejected"
	BulkNotFound        = "not_found"
	// BulkConflict is an issue that changed under the reject, or is no
	// longer open.
	BulkConflict = "conflict"
)

// BulkResult is one ID's outcome in a bulk reject.
type BulkResult struct {
	ID     string `json:"id"`
	Result string `json:"result"`
}

// Actions are the user's writes on issues: acknowledge, reject, reopen and
// bulk reject. Each writes the issue and its event in one transaction,
// under compare-and-swap, so it never silently overwrites another user or
// the evaluator.
type Actions struct {
	store     storage.AWSIssueStore
	scope     tenancy.OrgScope
	now       func() time.Time
	connected ConnectedSetFunc
}

// ConnectedSetFunc answers the accounts connected now. ok is false when
// the list cannot be read; reads then keep every account.
type ConnectedSetFunc func(ctx context.Context) (accounts map[string]bool, ok bool)

// ActionsOption configures Actions.
type ActionsOption func(*Actions)

// WithConnectedSet limits every summary and list the Actions read (the
// Issues page, its report, Overview and the Topology cards) to the
// accounts connected now.
func WithConnectedSet(read ConnectedSetFunc) ActionsOption {
	return func(a *Actions) { a.connected = read }
}

// NewActions returns the actions over provider's issue store.
func NewActions(provider storage.Provider, scope tenancy.OrgScope, now func() time.Time, options ...ActionsOption) (*Actions, error) {
	store, ok := storage.AWSIssuesOf(provider)
	if !ok {
		return nil, ErrNoStore
	}
	if now == nil {
		now = time.Now
	}
	actions := &Actions{store: store, scope: scope.Normalized(), now: now}
	for _, option := range options {
		option(actions)
	}
	return actions, nil
}

// Get reads one issue and its events.
func (a *Actions) Get(ctx context.Context, id string) (Issue, []Event, error) {
	issue, err := a.read(ctx, id)
	if err != nil {
		return Issue{}, nil, err
	}
	records, err := a.store.AWSIssueEvents(ctx, a.scope, id)
	if err != nil {
		return Issue{}, nil, err
	}
	events := make([]Event, 0, len(records))
	for _, record := range records {
		events = append(events, EventFromRecord(record))
	}
	return issue, events, nil
}

// List reads the issues a query selects, in account and ID order.
func (a *Actions) List(ctx context.Context, query storage.AWSIssueQuery) ([]Issue, error) {
	records, err := a.store.AWSIssues(ctx, a.scope, query)
	if err != nil {
		return nil, err
	}
	out := make([]Issue, 0, len(records))
	for _, record := range records {
		issue, err := FromRecord(record)
		if err != nil {
			return nil, err
		}
		out = append(out, issue)
	}
	sortIssues(out)
	return out, nil
}

// Acknowledge marks an issue as seen.
func (a *Actions) Acknowledge(ctx context.Context, id, actor string) (Issue, error) {
	return a.apply(ctx, id, func(issue Issue, now time.Time) (Issue, []Event, error) {
		return Acknowledge(issue, actor, now)
	})
}

// Reject rejects one issue with a reason and an optional review date.
func (a *Actions) Reject(ctx context.Context, id, actor, reason, reviewOn string) (Issue, error) {
	return a.apply(ctx, id, func(issue Issue, now time.Time) (Issue, []Event, error) {
		return Reject(issue, actor, reason, reviewOn, now)
	})
}

// Reopen undoes a rejection.
func (a *Actions) Reopen(ctx context.Context, id, actor, reason string) (Issue, error) {
	return a.apply(ctx, id, func(issue Issue, now time.Time) (Issue, []Event, error) {
		return Reopen(issue, actor, reason, now)
	})
}

// RejectMany applies one reason to the named issues only. Each issue is
// its own write: one that fails never undoes the others, and a repeat
// answers already_rejected without writing. A reason or review date that
// is not valid rejects nothing.
func (a *Actions) RejectMany(ctx context.Context, ids []string, actor, reason, reviewOn string) ([]BulkResult, error) {
	if len(ids) > MaxBulkReject {
		return nil, ErrTooMany
	}
	// Validate the shared input once, before any write.
	probe := Issue{State: StateOpen}
	if _, _, err := Reject(probe, actor, reason, reviewOn, a.now()); err != nil {
		return nil, err
	}
	out := make([]BulkResult, 0, len(ids))
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		_, err := a.Reject(ctx, id, actor, reason, reviewOn)
		result := BulkRejected
		switch {
		case err == nil:
		case errors.Is(err, ErrAlreadyRejected):
			result = BulkAlreadyRejected
		case errors.Is(err, ErrNotFound):
			result = BulkNotFound
		case errors.Is(err, ErrConflict), errors.Is(err, ErrNotActive):
			result = BulkConflict
		default:
			return out, err
		}
		out = append(out, BulkResult{ID: id, Result: result})
	}
	return out, nil
}

func (a *Actions) read(ctx context.Context, id string) (Issue, error) {
	records, err := a.store.AWSIssues(ctx, a.scope, storage.AWSIssueQuery{IDs: []string{id}})
	if errors.Is(err, storage.ErrInvalidAWSIssue) {
		return Issue{}, ErrNotFound
	}
	if err != nil {
		return Issue{}, err
	}
	if len(records) != 1 {
		return Issue{}, ErrNotFound
	}
	return FromRecord(records[0])
}

// apply reads an issue, applies a user action and writes it under
// compare-and-swap, reading again when an evaluation wrote it first.
func (a *Actions) apply(ctx context.Context, id string, action func(Issue, time.Time) (Issue, []Event, error)) (Issue, error) {
	for range maxActionRetries {
		before, err := a.read(ctx, id)
		if err != nil {
			return Issue{}, err
		}
		now := a.now().UTC()
		after, events, err := action(before, now)
		if err != nil {
			return Issue{}, err
		}
		if len(events) == 0 {
			return before, nil
		}
		after.Version, after.UpdatedAt = before.Version+1, now
		write, err := writeOf(Change{Issue: after, Before: &before, Events: events})
		if err != nil {
			return Issue{}, err
		}
		err = a.store.WriteAWSIssues(ctx, a.scope, storage.AWSIssueBatch{AccountID: after.Account, Writes: []storage.AWSIssueWrite{write}})
		if errors.Is(err, storage.ErrAWSIssueConflict) {
			continue
		}
		if err != nil {
			return Issue{}, err
		}
		after.Org = a.scope.Write
		return after, nil
	}
	return Issue{}, ErrConflict
}
