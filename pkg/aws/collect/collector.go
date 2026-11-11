package collect

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/VersusControl/versus-incident/pkg/aws/catalog"
	"github.com/VersusControl/versus-incident/pkg/aws/limit"
	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/aws/snapshot"
	"github.com/VersusControl/versus-incident/pkg/awsauth"
)

// Defaults for the change poller and for one unit's run time.
const (
	// DefaultChangeLookback is how far back the first poll of a region reads,
	// and the oldest gap later polls try to fill.
	DefaultChangeLookback = 24 * time.Hour
	// DefaultChangeOverlap re-reads the newest stretch of coverage on every
	// poll, because CloudTrail can deliver an event several minutes late.
	DefaultChangeOverlap = 15 * time.Minute
	// unitTimeout bounds one unit so a stalled endpoint cannot hold the whole
	// account sync.
	unitTimeout = 5 * time.Minute
	// defaultRetries is how many times a throttled call is retried after
	// waiting out the shared limiter's backoff.
	defaultRetries = 2
)

// Target is one connected account: its ID, the regions to read and the
// credentials of its assumed reader role.
type Target struct {
	AccountID   string
	Regions     []string
	Credentials aws.CredentialsProvider
}

// Options are the live collection settings read at the start of each sync.
type Options struct {
	// TagAllowlist names the tag keys whose values are kept. Every other
	// value is replaced before anything is stored.
	TagAllowlist []string
	// AppTagKeys are the tag keys the tag application source reads.
	AppTagKeys []string
	// ChangeLookback and ChangeOverlap shape the CloudTrail windows.
	ChangeLookback time.Duration
	ChangeOverlap  time.Duration
}

// DefaultAppTagKeys is the tag application source when none is configured.
func DefaultAppTagKeys() []string { return []string{"app", "service"} }

// Collector reads targets into the committed snapshot store. It owns no
// limiter of its own: it is handed the one the settings probes use, so both
// share one budget per account and region.
type Collector struct {
	store      *snapshot.Store
	limiter    *limit.Manager
	httpClient aws.HTTPClient
	now        func() time.Time
	sleep      func(context.Context, time.Duration) error
	retries    int
	options    func() Options
	logf       func(format string, args ...any)

	mu      sync.Mutex
	running map[string]bool
}

// Option adjusts a Collector.
type Option func(*Collector)

// WithHTTPClient injects the transport every collector call uses.
func WithHTTPClient(client aws.HTTPClient) Option {
	return func(collector *Collector) {
		if client != nil {
			collector.httpClient = client
		}
	}
}

// WithClock injects the clock used for change windows and event ages.
func WithClock(now func() time.Time) Option {
	return func(collector *Collector) {
		if now != nil {
			collector.now = now
		}
	}
}

// WithSleep injects the pause used between throttled retries.
func WithSleep(sleep func(context.Context, time.Duration) error) Option {
	return func(collector *Collector) {
		if sleep != nil {
			collector.sleep = sleep
		}
	}
}

// WithRetries sets how often a throttled call is retried. Zero disables
// retries.
func WithRetries(retries int) Option {
	return func(collector *Collector) {
		if retries >= 0 {
			collector.retries = retries
		}
	}
}

// WithOptions wires the live collection settings.
func WithOptions(read func() Options) Option {
	return func(collector *Collector) {
		if read != nil {
			collector.options = read
		}
	}
}

// WithLogger receives one line per failed unit. Lines carry identifiers and
// closed error codes only, never provider text or resource data.
func WithLogger(logf func(format string, args ...any)) Option {
	return func(collector *Collector) {
		if logf != nil {
			collector.logf = logf
		}
	}
}

// New builds a collector over store and the shared limiter.
func New(store *snapshot.Store, limiter *limit.Manager, options ...Option) *Collector {
	collector := &Collector{
		store:      store,
		limiter:    limiter,
		httpClient: awsauth.NewHTTPClient(),
		now:        time.Now,
		retries:    defaultRetries,
		options:    func() Options { return Options{} },
		logf:       func(string, ...any) {},
		running:    make(map[string]bool),
	}
	for _, option := range options {
		option(collector)
	}
	return collector
}

// Limiter returns the limiter every collector call waits on.
func (collector *Collector) Limiter() *limit.Manager { return collector.limiter }

// Store returns the snapshot store the collector commits to.
func (collector *Collector) Store() *snapshot.Store { return collector.store }

// Outcome is what happened to one unit in one sync.
type Outcome string

const (
	// OutcomeCommitted means a new or re-verified generation is current.
	OutcomeCommitted Outcome = "committed"
	// OutcomeKept means the attempt failed and the last good generation, if
	// any, stays current.
	OutcomeKept Outcome = "kept"
)

// UnitReport is one unit's result.
type UnitReport struct {
	Unit      model.Unit `json:"unit"`
	Outcome   Outcome    `json:"outcome"`
	ErrorCode string     `json:"error_code,omitempty"`
	Resources int        `json:"resources"`
}

// Report lists every unit a sync attempted.
type Report struct {
	Units []UnitReport `json:"units"`
}

// Failed counts units whose attempt did not commit.
func (report Report) Failed() int {
	failed := 0
	for _, unit := range report.Units {
		if unit.Outcome != OutcomeCommitted {
			failed++
		}
	}
	return failed
}

// ErrBusy means a sync for the same account is already running.
var ErrBusy = errors.New("aws collection already running for account")

type plannedUnit struct {
	spec           spec
	unit           model.Unit
	limiterRegion  string
	endpointRegion string
}

// Plan returns the units a sync of kinds would attempt for regions, in run
// order. Global services run once per account, against the global region.
func Plan(accountID string, regions []string, kinds ...Kind) []model.Unit {
	planned := plan(accountID, regions, kinds)
	units := make([]model.Unit, 0, len(planned))
	for _, item := range planned {
		units = append(units, item.unit)
	}
	return units
}

func wantKind(kinds []Kind, kind Kind) bool {
	if len(kinds) == 0 {
		return true
	}
	for _, wanted := range kinds {
		if wanted == kind {
			return true
		}
	}
	return false
}

// cleanRegions validates, de-duplicates and sorts the target regions.
func cleanRegions(regions []string) []string {
	seen := make(map[string]bool, len(regions))
	out := make([]string, 0, len(regions))
	for _, region := range regions {
		region = strings.TrimSpace(region)
		if awsauth.ValidateRegion(region) != nil || seen[region] {
			continue
		}
		seen[region] = true
		out = append(out, region)
	}
	sort.Strings(out)
	return out
}

func plan(accountID string, regions []string, kinds []Kind) []plannedUnit {
	regions = cleanRegions(regions)
	var planned []plannedUnit
	for _, entry := range specs {
		if !wantKind(kinds, entry.kind) {
			continue
		}
		if entry.global {
			planned = append(planned, plannedUnit{
				spec:           entry,
				unit:           model.Unit{Account: accountID, Region: model.GlobalRegion, Service: entry.service},
				limiterRegion:  catalog.GlobalHomeRegion,
				endpointRegion: catalog.GlobalHomeRegion,
			})
			continue
		}
		for _, region := range regions {
			planned = append(planned, plannedUnit{
				spec:           entry,
				unit:           model.Unit{Account: accountID, Region: region, Service: entry.service},
				limiterRegion:  region,
				endpointRegion: region,
			})
		}
	}
	if wantKind(kinds, KindChanges) {
		for _, region := range regions {
			planned = append(planned, plannedUnit{
				spec:           spec{service: snapshot.ChangeService, kind: KindChanges},
				unit:           model.Unit{Account: accountID, Region: region, Service: snapshot.ChangeService},
				limiterRegion:  region,
				endpointRegion: region,
			})
		}
	}
	return planned
}

func (collector *Collector) effectiveOptions() Options {
	options := collector.options()
	if len(options.AppTagKeys) == 0 {
		options.AppTagKeys = DefaultAppTagKeys()
	}
	if options.ChangeLookback <= 0 {
		options.ChangeLookback = DefaultChangeLookback
	}
	if options.ChangeOverlap <= 0 {
		options.ChangeOverlap = DefaultChangeOverlap
	}
	return options
}

// Sync reads every unit of kinds (all kinds when none is given) for one
// target and commits each unit on its own. A unit that fails keeps its last
// good generation and records why; it never affects another unit. Only one
// sync per account runs at a time.
func (collector *Collector) Sync(ctx context.Context, target Target, kinds ...Kind) (Report, error) {
	if collector == nil || collector.store == nil {
		return Report{}, snapshot.ErrNoStorage
	}
	if !snapshot.ValidUnit(model.Unit{Account: target.AccountID, Region: model.GlobalRegion, Service: "iam"}) {
		return Report{}, snapshot.ErrInvalidUnit
	}
	collector.mu.Lock()
	if collector.running[target.AccountID] {
		collector.mu.Unlock()
		return Report{}, ErrBusy
	}
	collector.running[target.AccountID] = true
	collector.mu.Unlock()
	defer func() {
		collector.mu.Lock()
		delete(collector.running, target.AccountID)
		collector.mu.Unlock()
	}()

	// Regions dropped from the account's selection are purged, so their
	// last snapshot does not linger as if it were still watched.
	if err := collector.purgeDroppedRegions(target); err != nil {
		return Report{}, err
	}
	writer := collector.store.Writer(target.AccountID)
	planned := plan(target.AccountID, target.Regions, kinds)
	// Every unit of every kind is registered before the first one runs, so
	// first-sync progress has a fixed total from the start and never reads
	// as finished between kinds.
	units := Plan(target.AccountID, target.Regions)
	if err := writer.RecordPending(units); err != nil {
		return Report{}, err
	}
	options := collector.effectiveOptions()
	allowlist := make(map[string]bool, len(options.TagAllowlist))
	for _, key := range options.TagAllowlist {
		allowlist[key] = true
	}

	report := Report{Units: make([]UnitReport, 0, len(planned))}
	for _, item := range planned {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		if writer.Stale() {
			return report, snapshot.ErrPurged
		}
		environment := env{
			account:     target.AccountID,
			region:      item.limiterRegion,
			credentials: target.Credentials,
			httpClient:  collector.httpClient,
			limiter:     collector.limiter,
			retries:     collector.retries,
			sleep:       collector.sleep,
		}
		c := &call{
			env:            environment,
			unit:           item.unit,
			partition:      model.PartitionForRegion(item.endpointRegion),
			endpointRegion: item.endpointRegion,
			allowlist:      allowlist,
			appTagKeys:     options.AppTagKeys,
			now:            collector.now().UTC(),
			writer:         writer,
		}
		var unitReport UnitReport
		if item.unit.Service == snapshot.ChangeService {
			unitReport = collector.pollChanges(ctx, c, options)
		} else {
			unitReport = collector.runUnit(ctx, c, item.spec)
		}
		report.Units = append(report.Units, unitReport)
	}
	return report, nil
}

// runUnit runs one inventory or health unit and commits or keeps it.
func (collector *Collector) runUnit(ctx context.Context, c *call, entry spec) UnitReport {
	report := UnitReport{Unit: c.unit, Outcome: OutcomeKept}
	if c.env.credentials == nil {
		report.ErrorCode = awsauth.ErrNoServerIdentity.Error()
		collector.fail(ctx, c, report.ErrorCode, nil)
		return report
	}
	unitCtx, cancel := context.WithTimeout(ctx, unitTimeout)
	out, err := entry.run(unitCtx, c)
	cancel()
	if ctx.Err() != nil {
		// Shutdown: whatever the unit read is incomplete and is not kept.
		return report
	}
	if err != nil {
		report.ErrorCode = errorCode(err)
		if report.ErrorCode == CodeNotInRegion {
			if !collector.store.HasGeneration(c.unit) {
				return collector.notInRegion(c, report)
			}
			// The unit has read data here before, so the endpoint exists:
			// a host that stopped resolving is an egress or DNS fault, and
			// the stored data must not look fresh.
			report.ErrorCode = awsauth.ErrNetwork.Error()
		}
		collector.fail(ctx, c, report.ErrorCode, nil)
		return report
	}

	// A detail call that failed for a passing reason (throttling, a timeout)
	// would replace good detail with nothing. Keep the last good generation
	// and name the gaps instead; only a unit that has never committed takes
	// the partial candidate, because something is better than nothing there.
	if c.transientCode != "" && collector.store.HasGeneration(c.unit) {
		report.ErrorCode = c.transientCode
		collector.fail(ctx, c, report.ErrorCode, out.gaps)
		return report
	}

	resources := make([]model.Resource, 0, len(out.resources))
	for _, resource := range out.resources {
		if resource.ARN == "" {
			continue
		}
		resources = append(resources, resource)
	}
	generation, err := c.writer.Commit(c.unit, snapshot.Candidate{
		Resources:  resources,
		Health:     out.health,
		Flags:      out.flags,
		DetailGaps: out.gaps,
	})
	if errors.Is(err, snapshot.ErrPurged) {
		return report
	}
	if err != nil {
		report.ErrorCode = CodeInvalid
		if !errors.Is(err, snapshot.ErrInvalidCandidate) {
			report.ErrorCode = CodeStorage
		}
		collector.fail(ctx, c, report.ErrorCode, nil)
		return report
	}
	report.Outcome = OutcomeCommitted
	report.Resources = len(generation.Resources)
	return report
}

// notInRegion records a unit whose service has no endpoint in its region,
// and that never read data there, as read with a gap that says so: there
// is nothing to read there, so it is neither a failure nor an empty answer.
func (collector *Collector) notInRegion(c *call, report UnitReport) UnitReport {
	unit := c.unit
	collector.logf("aws collect: account=%s region=%s service=%s not available in region", unit.Account, unit.Region, unit.Service)
	gap := model.Gap{Account: unit.Account, Region: unit.Region, Service: unit.Service, Reason: model.GapNotInRegion}
	if err := c.writer.RecordSuccess(unit, []model.Gap{gap}); err != nil {
		if !errors.Is(err, snapshot.ErrPurged) {
			collector.logf("aws collect: account=%s region=%s service=%s state not saved", unit.Account, unit.Region, unit.Service)
		}
		return report
	}
	report.Outcome = OutcomeCommitted
	return report
}

// fail records a unit's failed attempt. A sync cut short by shutdown records
// nothing: the unit did not fail, the process stopped, and its last state
// is still the truth.
func (collector *Collector) fail(ctx context.Context, c *call, code string, gaps []model.Gap) {
	if ctx.Err() != nil {
		return
	}
	unit := c.unit
	collector.logf("aws collect: account=%s region=%s service=%s failed code=%s", unit.Account, unit.Region, unit.Service, code)
	if err := c.writer.RecordFailure(unit, code, gaps...); err != nil && !errors.Is(err, snapshot.ErrPurged) {
		collector.logf("aws collect: account=%s region=%s service=%s state not saved", unit.Account, unit.Region, unit.Service)
	}
}

func (collector *Collector) purgeDroppedRegions(target Target) error {
	stored, err := collector.store.Regions(target.AccountID)
	if err != nil {
		return err
	}
	wanted := make(map[string]bool, len(target.Regions))
	for _, region := range cleanRegions(target.Regions) {
		wanted[region] = true
	}
	if len(wanted) == 0 {
		return nil
	}
	for _, region := range stored {
		if wanted[region] {
			continue
		}
		if err := collector.store.PurgeRegion(target.AccountID, region); err != nil {
			return err
		}
		collector.logf("aws collect: account=%s region=%s purged after leaving the selection", target.AccountID, region)
	}
	return nil
}
