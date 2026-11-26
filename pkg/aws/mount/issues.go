package mount

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	awscollect "github.com/VersusControl/versus-incident/pkg/aws/collect"
	"github.com/VersusControl/versus-incident/pkg/aws/issues"
	"github.com/VersusControl/versus-incident/pkg/aws/model"
	awssettings "github.com/VersusControl/versus-incident/pkg/aws/settings"
	awssnapshot "github.com/VersusControl/versus-incident/pkg/aws/snapshot"
	c "github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/controllers"
	"github.com/VersusControl/versus-incident/pkg/logging"
	"github.com/VersusControl/versus-incident/pkg/services"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

// writeHook forwards the snapshot store's unit writes to whoever listens.
// The store is built before its listener, so the listener is set later.
type writeHook struct {
	listener atomic.Pointer[func(model.Unit)]
}

func (hook *writeHook) notify(unit model.Unit) {
	if listener := hook.listener.Load(); listener != nil {
		(*listener)(unit)
	}
}

func (hook *writeHook) listen(fn func(model.Unit)) { hook.listener.Store(&fn) }

// changeHook forwards a settings change to whoever listens, set after the
// settings service is built.
type changeHook struct {
	listener atomic.Pointer[func()]
}

func (hook *changeHook) notify() {
	if listener := hook.listener.Load(); listener != nil {
		(*listener)()
	}
}

func (hook *changeHook) listen(fn func()) { hook.listener.Store(&fn) }

// mountIssues builds the issue evaluator over the snapshot store and the
// provider's issue tables, the alert pump it runs after each evaluation,
// and the issue routes. Every read the collectors commit or fail, except
// the change poller's, marks the evaluator due; it evaluates at most once a
// minute, on one replica at a time. Removing an account, or a Region from
// one, marks it due too. A provider without the issue tables
// mounts no evaluator, and its routes answer enabled=false.
func mountIssues(deps Deps, mounted *Mounted) {
	clock := deps.Clock
	if clock == nil {
		clock = time.Now
	}
	current := liveAWS(deps.Config)
	settingsStore := awssettings.NewStore(deps.Store, deps.Scope)
	source := issues.NewSnapshotSource(mounted.Snapshots)
	defer func() {
		controllers.NewAWSIssuesController(mounted.IssueActions, issueCoverage(deps, mounted, settingsStore, clock),
			func(ctx context.Context, accounts []string) ([]issues.AccountNotAssessed, error) {
				if !current().Enable {
					return []issues.AccountNotAssessed{}, nil
				}
				return issues.NotAssessedNow(ctx, source, issues.Default, accounts)
			}, clock).Register(deps.Router)
	}()
	options := []issues.Option{issues.WithLogger(logging.Warnf), issues.WithDebug(logging.Debugf)}
	options = append(options, issues.WithConnectedAccounts(connectedAccounts(settingsStore, deps.Scope, current, clock)))
	alerter, err := issues.NewAlerter(deps.Store, deps.Scope, issueAlertSink{org: deps.Scope.Write},
		issues.WithAlertLogger(logging.Warnf), issues.WithAlertClock(deps.Clock),
		issues.WithCauseFinder(issues.NewCauseFinder(mounted.Snapshots, func() time.Duration { return current().Collect.ChangeRetention() }, clock)),
		issues.WithUnreadFinder(issues.NewUnreadFinder(source, issues.Default)),
		issues.WithAccountNames(accountNames(settingsStore, deps.Scope.Write, current)))
	if err != nil {
		return
	}
	options = append(options, issues.WithAfterEvaluate(func(ctx context.Context, report issues.Report) {
		// Failures are logged by the pump, throttled; the next evaluation
		// sends what this one could not record.
		_ = alerter.Pump(ctx, report)
	}))
	evaluator, err := issues.NewEvaluator(deps.Store, deps.Scope, source, options...)
	if err != nil {
		return
	}
	actions, err := issues.NewActions(deps.Store, deps.Scope, deps.Clock)
	if err != nil {
		return
	}
	mounted.Issues, mounted.IssueActions = evaluator, actions
	if mounted.written != nil {
		mounted.written.listen(func(unit model.Unit) {
			if awscollect.KindOf(unit.Service) != awscollect.KindChanges {
				evaluator.Trigger()
			}
		})
	}
	// A removed account, or a Region no longer read, is resolved by the
	// next evaluation; nothing is collected for it to mark that due.
	if mounted.accountsChanged != nil {
		mounted.accountsChanged.listen(evaluator.Trigger)
	}
}

// issueAlertSink sends issue alerts through the deployment's alert path,
// under the write organization.
type issueAlertSink struct{ org string }

func (sink issueAlertSink) Send(ctx context.Context, alert issues.Alert) error {
	return services.CreateIncidentFromAWSIssue(ctx, sink.org, alert)
}

func (sink issueAlertSink) SendDigest(ctx context.Context, alerts []issues.Alert) error {
	return services.CreateIncidentFromAWSIssueDigest(ctx, sink.org, alerts)
}

// accountNames reads the connected accounts' display names for alerts. A
// failed read names accounts by their IDs.
func accountNames(store *awssettings.Store, org string, current func() c.AWSConnectorConfig) func(context.Context) map[string]string {
	return func(context.Context) map[string]string {
		names := map[string]string{}
		stored, err := store.ListAccounts(org)
		if err != nil {
			return names
		}
		for _, account := range awssettings.MergeAccounts(stored, current().Accounts) {
			if account.DisplayName != "" {
				names[account.AccountID] = account.DisplayName
			}
		}
		return names
	}
}

// issueCoverage reads the coverage the issue summary and report show from
// the stored reads of the connected accounts, by the same rule as the
// coverage page.
func issueCoverage(deps Deps, mounted *Mounted, store *awssettings.Store, clock func() time.Time) controllers.AWSIssuesCoverage {
	current := liveAWS(deps.Config)
	return func(ctx context.Context, account string) (issues.CoverageRead, bool, error) {
		conn := current()
		if !conn.Enable || mounted.Snapshots == nil {
			return issues.CoverageRead{}, false, nil
		}
		expected, _, err := coverageScope(store, deps.Scope.Write, conn, clock)
		if err != nil {
			return issues.CoverageRead{}, true, err
		}
		view, err := mounted.Snapshots.Coverage(ctx, awssnapshot.CoverageScope{Account: account, Expected: expected, ExpectedKnown: true})
		if err != nil {
			return issues.CoverageRead{}, true, err
		}
		return issues.CoverageRead{AccountsRead: view.Summary.AccountsRead, AccountsTotal: view.Summary.AccountsTotal, NotRead: view.NotRead}, true, nil
	}
}

// connectedAccounts reads the accounts the connector holds and the Regions
// each is read in, by the collectors' Region rule. The settings store reads
// the write organization's whole read scope, so an account connected through
// the setup wizard before a licence (stored under the default organization)
// is still connected after it, and one removed after it is not; its health
// is the one the health checks keep for the write organization. An account
// whose Regions are not known yet (none selected, none found by a health
// check) has nil Regions, so none of its Regions counts as removed. With the
// connector off the list is not known, and nothing is resolved as removed.
func connectedAccounts(store *awssettings.Store, scope tenancy.OrgScope, current func() c.AWSConnectorConfig, now func() time.Time) func(context.Context) ([]issues.ConnectedAccount, error) {
	org := scope.Normalized().Write
	return func(context.Context) ([]issues.ConnectedAccount, error) {
		conn := current()
		if !conn.Enable {
			return nil, issues.ErrConnectedUnknown
		}
		stored, err := store.ListAccountsStrict(org)
		if err != nil {
			return nil, err
		}
		var out []issues.ConnectedAccount
		for _, account := range awssettings.MergeAccounts(stored, conn.Accounts) {
			health, err := store.GetHealth(org, account.AccountID)
			if err != nil && !errors.Is(err, awssettings.ErrNotFound) {
				return nil, err
			}
			entry := issues.ConnectedAccount{Account: account.AccountID}
			if regions, _ := awssettings.CollectRegions(account, health, now().UTC()); len(regions) > 0 {
				entry.Regions = regions
			}
			out = append(out, entry)
		}
		return out, nil
	}
}
