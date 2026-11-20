// Package mount wires the AWS connector, its collectors, the Changes page
// and FinOps onto one router, over one storage provider and one org scope.
// Every binary that serves the AWS pages calls Mount, so the open-source
// build and a licensed build run the same setup and serve the same routes.
package mount

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/gofiber/fiber/v2"

	"github.com/VersusControl/versus-incident/pkg/agent"
	awscollect "github.com/VersusControl/versus-incident/pkg/aws/collect"
	awscost "github.com/VersusControl/versus-incident/pkg/aws/cost"
	awslimit "github.com/VersusControl/versus-incident/pkg/aws/limit"
	awssettings "github.com/VersusControl/versus-incident/pkg/aws/settings"
	awssnapshot "github.com/VersusControl/versus-incident/pkg/aws/snapshot"
	"github.com/VersusControl/versus-incident/pkg/aws/snapshot/alarmsuspect"
	c "github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/controllers"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/finops"
	"github.com/VersusControl/versus-incident/pkg/services"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

// DefaultDeployment names the deployment's cost signal source.
const DefaultDeployment = "default"

// ErrInvalidCostOptions is a cost adapter that refused its boot options.
var ErrInvalidCostOptions = errors.New("aws cost: invalid options")

// Deps are what the AWS surface needs from the binary that mounts it.
type Deps struct {
	// Store holds every AWS and FinOps record. A licensed build passes its
	// org-scoped provider; the typed stores it must forward are listed in
	// the package documentation of Mount.
	Store storage.Provider
	// Scope is where records are written and read from. The zero value is
	// the open-source default org.
	Scope tenancy.OrgScope
	// Config is the boot configuration. The live configuration, once
	// loaded, wins on every read.
	Config *c.Config
	// Router is the /api group the admin routes mount under.
	Router fiber.Router
	// HTTPClient overrides the AWS transport for every AWS call. Nil keeps
	// the hardened default; only a test build sets it.
	HTTPClient aws.HTTPClient
	// Deployment names the cost signal source; empty is DefaultDeployment.
	Deployment string
	// ChangesLimits returns the Changes window and compare limits, read on
	// every request. Nil is the open-source 24 h for both.
	ChangesLimits func() awssnapshot.ChangesLimits
	// FinOpsOptions returns the licensed FinOps options. Nil is the
	// open-source set: every licensed option off.
	FinOpsOptions func() finops.EditionOptions
	// CostStorageRetentionMonths returns how many months of cost lines are
	// stored, read at every refresh. It decides what the refresh deletes, so
	// a licensed build feeds it from its storage setting whether or not the
	// license is current. Nil is the open-source 13. What the page shows
	// follows FinOpsOptions().RetentionMonths instead.
	CostStorageRetentionMonths func() int
}

// Mounted is the AWS surface Mount built: the parts the binary starts and
// waits for, and the ones a licensed build extends.
type Mounted struct {
	Settings   *awssettings.Service
	Snapshots  *awssnapshot.Store
	Collectors *awscollect.Scheduler
	Limiter    *awslimit.Manager
	Changes    *awssnapshot.ChangeFeed
	Cost       *awscost.Scheduler
	FinOps     *awscost.Backend
	// HTTPClient is the AWS transport override every AWS caller shares.
	HTTPClient aws.HTTPClient

	deployment       string
	finOpsOptions    func() finops.EditionOptions
	storageRetention func() int
}

// Mount builds the AWS connector, its collectors, the Changes feed and the
// FinOps cost refresh, registers every /api/admin/aws route on deps.Router,
// and binds the agent's AWS, Changes and FinOps tools. Nothing starts until
// Start.
//
// deps.Store is discovered for these optional capabilities; a provider that
// lacks one degrades that feature exactly as the file backend does:
// storage.AWSInventoryStore, storage.AWSChangeFeedStore,
// storage.AWSUnitHistoryStore, storage.AWSRetentionSweeper,
// storage.FinOpsStore, storage.FinOpsSavingDecisionStore,
// storage.FinOpsSavingDecisionSubjects, storage.FinOpsRollupStore,
// storage.SessionLocker (the change-pair and cost refresh locks),
// storage.SessionLockVerifier and storage.BlobCAS. The Changes preferences
// are a blob under settings/aws-changes-preferences/.
func Mount(deps Deps) (*Mounted, error) {
	if deps.Store == nil || deps.Config == nil || deps.Router == nil {
		return nil, errors.New("aws mount: store, config and router are required")
	}
	if deps.Scope.Write == "" {
		deps.Scope = tenancy.DefaultOrgScope()
	}
	deps.Scope = deps.Scope.Normalized()
	if deps.Deployment == "" {
		deps.Deployment = DefaultDeployment
	}
	mounted := mountConnector(deps)
	mounted.deployment = deps.Deployment
	mounted.finOpsOptions = deps.FinOpsOptions
	mounted.storageRetention = deps.CostStorageRetentionMonths
	agent.ConfigureAWS(func() *awssettings.Service { return mounted.Settings })
	mounted.Changes = mountChanges(deps, mounted)
	cost, backend, err := mountFinOps(deps, mounted)
	if err != nil {
		return nil, err
	}
	mounted.Cost, mounted.FinOps = cost, backend
	return mounted, nil
}

// Start runs the settings health loop, the collectors and the cost refresh
// until ctx ends.
func (m *Mounted) Start(ctx context.Context) {
	m.Settings.Start(ctx)
	m.Collectors.Start(ctx)
	m.Cost.Start(ctx)
}

// Wait waits, after the context given to Start ended, for the collectors and
// the cost refresh to stop, each bounded by grace, so storage is not closed
// under a write. It logs whichever did not stop in time.
func (m *Mounted) Wait(grace time.Duration) {
	if !waitForStop(m.Collectors.Stopped(), grace) {
		log.Printf("aws collect: collectors did not stop within %s", grace)
	}
	if !waitForStop(m.Cost.Stopped(), grace) {
		log.Printf("aws cost: refresh did not stop within %s", grace)
	}
}

// FinOpsOptions returns the FinOps options in force: the licensed build's,
// or the open-source set.
func (m *Mounted) FinOpsOptions() finops.EditionOptions {
	if m == nil || m.finOpsOptions == nil {
		return finops.DefaultEditionOptions()
	}
	return m.finOpsOptions().Effective()
}

// CostStorageRetentionMonths is how many months of cost lines the refresh
// keeps: the licensed build's storage setting, or the open-source 13.
func (m *Mounted) CostStorageRetentionMonths() int {
	if m == nil || m.storageRetention == nil {
		return finops.DefaultCostRetentionMonths
	}
	return max(m.storageRetention(), finops.DefaultCostRetentionMonths)
}

// costVisibleMonths is how many months reads reach: the edition's
// retention, which follows the license.
func (m *Mounted) costVisibleMonths() int { return m.FinOpsOptions().RetentionMonths }

// waitForStop waits for done or grace, whichever comes first, and reports
// whether done closed.
func waitForStop(done <-chan struct{}, grace time.Duration) bool {
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

// liveAWS reads the live connector config, falling back to the boot config
// before the global one is loaded.
func liveAWS(boot *c.Config) func() c.AWSConnectorConfig {
	return func() c.AWSConnectorConfig {
		if live := c.GetConfigOrNil(); live != nil {
			return live.Connectors.AWS
		}
		return boot.Connectors.AWS
	}
}

// mountConnector mounts the AWS connector's admin surface and builds the
// snapshot store and collectors behind it. The probes and the collectors
// share one limiter.
func mountConnector(deps Deps) *Mounted {
	cfg, store, scope := deps.Config, deps.Store, deps.Scope
	limiter := awslimit.NewManager(cfg.Connectors.AWS.Budgets.MaxRequestsPerSecond)
	current := liveAWS(cfg)
	if warning := cfg.Connectors.AWS.Collect.HistoryHorizonWarning(); warning != "" && cfg.Connectors.AWS.Enable {
		log.Printf("aws collect: %s", warning)
	}
	var service *awssettings.Service
	snapshots := awssnapshot.New(store, scope, awssnapshot.WithTagAllowlist(func() []string {
		// The same effective allowlist the collectors redact with at write.
		if service == nil {
			return awssettings.DefaultTagValueAllowlist()
		}
		return service.TagValueAllowlist()
	}), awssnapshot.WithOptions(func() awssnapshot.Options {
		connector := current()
		return awssnapshot.Options{
			MaxGenerations: connector.Collect.MaxGenerations(),
			GenerationsFor: func(service string) int {
				switch awscollect.KindOf(service) {
				case awscollect.KindHealth:
					return connector.Collect.HealthMaxGenerations()
				case awscollect.KindInventory:
					return connector.Collect.InventoryMaxGenerations()
				}
				return connector.Collect.MaxGenerations()
			},
			MaxAge:          connector.Collect.MaxAge(),
			ChangeRetention: connector.Collect.ChangeRetention(),
			AppSources:      connector.Applications.EffectiveSources(),
		}
	}))
	collector := awscollect.New(snapshots, limiter,
		awscollect.WithHTTPClient(deps.HTTPClient),
		awscollect.WithLogger(log.Printf),
		awscollect.WithAccountHealth(collectAccountHealth(awssettings.NewStore(store, scope), scope.Write)),
		awscollect.WithOptions(func() awscollect.Options {
			collect := current().Collect
			return awscollect.Options{
				TagAllowlist:     service.TagValueAllowlist(),
				AppTagKeys:       current().Applications.EffectiveTagKeys(),
				ChangeLookback:   collect.ChangeLookback(),
				ChangeOverlap:    collect.ChangeOverlap(),
				ChangesInterval:  collect.ChangesInterval(),
				ChangePageBudget: collect.Changes.PageBudget,
				RoutineEvents:    collect.ChangeRoutineEvents(),
				FailureKeep:      collect.ChangeFailureKeep(),
			}
		}),
	)
	scheduler := awscollect.NewScheduler(collector,
		func(ctx context.Context) []awscollect.Target {
			targets := service.CollectTargets(ctx)
			out := make([]awscollect.Target, 0, len(targets))
			for _, target := range targets {
				out = append(out, awscollect.Target{AccountID: target.AccountID, Regions: target.Regions, Credentials: target.Credentials})
			}
			return out
		},
		func() awscollect.Intervals {
			collect := current().Collect
			return awscollect.Intervals{Inventory: collect.InventoryInterval(), Health: collect.HealthInterval(), Changes: collect.ChangesInterval()}
		},
		func() bool {
			connector := current()
			return connector.Enable && connector.Collect.Enable
		},
		awscollect.WithAccounts(func(ctx context.Context) ([]string, error) { return service.AccountIDs(ctx) }),
	)
	service = awssettings.NewService(
		awssettings.NewStore(store, scope),
		awssettings.WithConfig(func() c.AWSConnectorConfig { return current() }),
		awssettings.WithLimiter(limiter),
		awssettings.WithAIInfo(awssettings.AIInfo{Enabled: cfg.Agent.AI.Enable, Provider: cfg.Agent.AI.Provider, Model: cfg.Agent.AI.Model}),
		awssettings.WithSnapshots(snapshots),
		awssettings.WithAccountAdded(scheduler.Trigger),
		awssettings.WithHTTPClient(deps.HTTPClient),
	)
	controllers.NewAWSSettingsController(service).Register(deps.Router)
	return &Mounted{Settings: service, Snapshots: snapshots, Collectors: scheduler, Limiter: limiter, HTTPClient: deps.HTTPClient}
}

// mountChanges mounts the read-only Changes page over the snapshot store.
// The expected scope is the configured accounts and regions, read from
// settings at request time, so a region added a moment ago shows as not yet
// read rather than as quiet.
func mountChanges(deps Deps, mounted *Mounted) *awssnapshot.ChangeFeed {
	current := liveAWS(deps.Config)
	scope := deps.Scope
	settingsStore := awssettings.NewStore(deps.Store, scope)
	feed := awssnapshot.NewChangeFeed(mounted.Snapshots,
		awssnapshot.WithSuspectEngine(alarmsuspect.New(mounted.Snapshots)),
		awssnapshot.WithScope(func(context.Context) ([]awssnapshot.ScopeAccount, bool, error) {
			conn := current()
			if !conn.Enable {
				return nil, true, nil
			}
			return changeScope(settingsStore, scope.Write, conn)
		}),
		awssnapshot.WithFeedOptions(func() awssnapshot.FeedOptions {
			collect := current().Collect
			return awssnapshot.FeedOptions{
				Lookback:    collect.ChangeLookback(),
				Overlap:     collect.ChangeOverlap(),
				Interval:    collect.ChangesInterval(),
				BurstWindow: collect.ChangeBurstWindow(),
				BurstMin:    collect.ChangeBurstMin(),
				ReadCap:     collect.ChangeReadCap(),
			}
		}),
		awssnapshot.WithChangesLimits(deps.ChangesLimits),
	)
	controllers.NewAWSChangesController(feed).WithPreferences(awssnapshot.NewChangePreferenceStore(deps.Store)).Register(deps.Router)
	// The agent's get_aws_changes tool and the recent_changes feed answer
	// from the same change list as the page.
	agent.ConfigureAWSChanges(changeListFor(current, feed))
	return feed
}

// CostOptions maps the connector configuration onto one cost refresh's
// options. Cost calls share the connector's limiter, inventory and AWS
// transport, so a test build redirects them with every other AWS call;
// otherwise the cost adapter keeps its hardened default client.
func (m *Mounted) CostOptions(conn c.AWSConnectorConfig, targets []awscost.Target, allowlist []string) awscost.Options {
	deployment := m.deployment
	if deployment == "" {
		deployment = DefaultDeployment
	}
	opts := awscost.FromConfig(conn, targets, allowlist, deployment)
	opts.Limiter = m.Limiter
	opts.Inventory = m.Snapshots
	opts.RetentionMonths = m.CostStorageRetentionMonths
	opts.VisibleMonths = m.costVisibleMonths
	if m.HTTPClient != nil {
		opts.HTTPClient = m.HTTPClient
	}
	return opts
}

// mountFinOps mounts the FinOps routes, binds the read-only FinOps agent
// tools and builds the once-a-day cost refresh. Cost is read only when the
// AWS connector and its Cost Explorer opt-in are both on; otherwise the
// routes answer enabled=false and nothing calls AWS. New increases go
// through the existing alert path once each.
func mountFinOps(deps Deps, connector *Mounted) (*awscost.Scheduler, *awscost.Backend, error) {
	current := liveAWS(deps.Config)
	store, scope := deps.Store, deps.Scope
	adapter, err := awscost.New(store, scope, awscost.Options{Deployment: deps.Deployment, Limiter: connector.Limiter,
		Enabled: func() bool { return false }, DisabledReason: awscost.ReasonCostDataOff,
		RetentionMonths: connector.CostStorageRetentionMonths, VisibleMonths: connector.costVisibleMonths})
	if err != nil {
		return nil, nil, ErrInvalidCostOptions
	}
	// Deleting an account deletes the cost data it paid for.
	connector.Settings.SetPayerRemover(adapter)
	var backend *awscost.Backend
	prepare := func(ctx context.Context) error {
		conn := current()
		// Read before the accounts: an account deleted between this read and
		// the update keeps its removal, because this snapshot's revision is
		// then not newer than the one the deletion recorded. An unreadable
		// revision reads as zero, which revives no removed account.
		revision, err := connector.Settings.AccountsRevision(ctx)
		if err != nil {
			log.Printf("aws cost: account settings revision unreadable; removed accounts stay out of collection")
		}
		// The accounts are read after the revision too, also with cost data
		// off, so a pending cost purge can tell an account added again from
		// one still removed.
		var accounts []string
		if ids, err := connector.Settings.AccountIDs(ctx); err == nil {
			accounts = append([]string{}, ids...)
		}
		var targets []awscost.Target
		if conn.CostDataEnabled() {
			for _, target := range connector.Settings.CostTargets(ctx) {
				targets = append(targets, awscost.Target{AccountID: target.AccountID, Credentials: target.Credentials,
					Cause: target.Cause, Since: target.Since})
			}
		}
		opts := connector.CostOptions(conn, targets, connector.Settings.TagValueAllowlist())
		opts.SettingsRevision = revision
		opts.SettingsAccounts = accounts
		// Resource-level cost for the services with an open increase.
		opts.ResourceServices = func() []string { return backend.OpenIncreaseServices(context.Background()) }
		return adapter.Update(opts)
	}
	backend = awscost.NewBackend(adapter, awscost.NewStore(store, scope), current)
	controllers.NewAWSFinOpsController(backend).Register(deps.Router)
	agent.ConfigureFinOps(backend.Reader)
	signals := awscost.NewSignalSource(deps.Deployment, awscost.ServiceFunc(backend.Service), nil)
	sink := func(ctx context.Context, sig core.Signal) error {
		return services.CreateIncidentFromFinOpsSignal(ctx, scope.Write, sig)
	}
	scheduler := awscost.NewScheduler(adapter,
		func() (bool, int, int, *time.Location) {
			conn := current()
			refresh := conn.FinOps.Effective()
			hour, minute := refresh.RefreshClock()
			return conn.CostDataEnabled(), hour, minute, refresh.Location()
		},
		prepare,
		awscost.WithLogger(log.Printf),
		awscost.WithAfterRefresh(func(ctx context.Context, _ awscost.RefreshResult) {
			if _, err := awscost.PumpAlerts(ctx, signals, sink); err != nil {
				log.Printf("aws cost: increase or budget alerts not sent; retried after the next refresh")
			}
			// Saving decisions follow the savings the refreshed data
			// produces; syncing here keeps page reads free of writes.
			if err := backend.Service().SyncSavingDecisions(ctx); err != nil {
				log.Printf("aws cost: saving decisions not synced; retried after the next refresh")
			}
		}),
	)
	return scheduler, backend, nil
}
