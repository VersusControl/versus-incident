package settings

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	mathrand "math/rand/v2"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/VersusControl/versus-incident/pkg/aws/catalog"
	"github.com/VersusControl/versus-incident/pkg/aws/limit"
	"github.com/VersusControl/versus-incident/pkg/aws/probe"
	"github.com/VersusControl/versus-incident/pkg/aws/snapshot"
	"github.com/VersusControl/versus-incident/pkg/awsauth"
	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

const (
	// DefaultHealthCheckInterval is the identity-check cadence when the config
	// does not set one.
	DefaultHealthCheckInterval = 5 * time.Minute
	// ServiceProbeInterval is how often the service grid is re-probed inside
	// the health loop.
	ServiceProbeInterval = 6 * time.Hour
	// DefaultBudgetRPS is the per-account, per-region request rate used when
	// neither config nor a saved setting provides one.
	DefaultBudgetRPS = 2
	// defaultHomeRegion is where regional probes and STS calls run when the
	// account has no selection.
	defaultHomeRegion = "us-east-1"
	// verifyWindow and verifyLimit bound the on-demand verify calls.
	verifyWindow = time.Minute
	verifyLimit  = 6
	// probeTimeout bounds one full verify sweep.
	probeTimeout = 2 * time.Minute
)

// Closed service errors. They are the only text the API ever renders.
var (
	// ErrDisabled means connectors.aws.enable is false.
	ErrDisabled = errors.New("aws connector disabled")
	// ErrRateLimited means the org exhausted its verify allowance.
	ErrRateLimited = errors.New("verify rate limited")
	// ErrConfigManaged means the account is defined in config and cannot be
	// changed through the API.
	ErrConfigManaged = errors.New("account is defined in config")
	// ErrNotVerified means a draft has not finished its access check.
	ErrNotVerified = errors.New("draft is not verified")
	// ErrInvalidArguments means request input failed validation.
	ErrInvalidArguments = errors.New("invalid arguments")
	// ErrNoServerIdentity means the deployment has no AWS identity to assume
	// roles with.
	ErrNoServerIdentity = errors.New("no server identity")
	// ErrAccountMismatch means the credentials work but belong to another
	// AWS account than the one configured, so nothing is read through them.
	ErrAccountMismatch = errors.New("account_mismatch")
	// ErrOrganizationsUnavailable means Organizations discovery requires the
	// enterprise module.
	ErrOrganizationsUnavailable = errors.New("organizations unavailable")
)

// AssumeFunc assumes a customer role and returns its credentials.
type AssumeFunc func(ctx context.Context, base aws.CredentialsProvider, roleARN, externalID, region string) (aws.CredentialsProvider, error)

// IdentityFunc answers the caller identity for a credential provider.
type IdentityFunc func(ctx context.Context, credentials aws.CredentialsProvider, region string) (string, string, error)

// BaseFunc resolves the Versus principal from the host environment.
type BaseFunc func(region string) (aws.CredentialsProvider, error)

// Templates carries the role-template destinations the wizard offers.
type Templates struct {
	TemplateURL string
}

// DefaultTemplates is the published role template.
func DefaultTemplates() Templates {
	return Templates{TemplateURL: "https://versus-releases.s3.amazonaws.com/aws/versus-reader.yaml"}
}

// AIInfo is the configured model destination shown on the privacy card.
type AIInfo struct {
	Enabled  bool
	Provider string
	Model    string
}

// Option adjusts a Service.
type Option func(*Service)

// WithConfig wires the connector configuration reader.
func WithConfig(read func() config.AWSConnectorConfig) Option {
	return func(conn *Service) {
		if read != nil {
			conn.readConfig = read
		}
	}
}

// WithAIInfo wires the privacy card's model destination.
func WithAIInfo(info AIInfo) Option {
	return func(conn *Service) { conn.ai = info }
}

// WithLimiter wires the shared per-account request limiter and meter.
func WithLimiter(manager *limit.Manager) Option {
	return func(conn *Service) {
		if manager != nil {
			conn.limiter = manager
		}
	}
}

// WithHTTPClient injects the transport used for STS and probes.
func WithHTTPClient(client aws.HTTPClient) Option {
	return func(conn *Service) {
		if client != nil {
			conn.httpClient = client
		}
	}
}

// WithClock injects the clock used for health transitions and draft TTLs.
func WithClock(now func() time.Time) Option {
	return func(conn *Service) {
		if now != nil {
			conn.now = now
			conn.store.setClock(now)
		}
	}
}

// WithBaseProvider injects the host identity resolver.
func WithBaseProvider(base BaseFunc) Option {
	return func(conn *Service) {
		if base != nil {
			conn.base = base
		}
	}
}

// WithAssumeRole injects the assume-role seam.
func WithAssumeRole(assume AssumeFunc) Option {
	return func(conn *Service) {
		if assume != nil {
			conn.assume = assume
		}
	}
}

// WithIdentity injects the caller-identity seam.
func WithIdentity(identity IdentityFunc) Option {
	return func(conn *Service) {
		if identity != nil {
			conn.identity = identity
		}
	}
}

// WithTemplates overrides the role-template destinations.
func WithTemplates(templates Templates) Option {
	return func(conn *Service) {
		if templates.TemplateURL != "" {
			conn.templates = templates
		}
	}
}

// Service owns the connector's view, wizard and health behaviour.
type Service struct {
	store      *Store
	scope      tenancy.OrgScope
	readConfig func() config.AWSConnectorConfig
	ai         AIInfo
	limiter    *limit.Manager
	httpClient aws.HTTPClient
	base       BaseFunc
	assume     AssumeFunc
	identity   IdentityFunc
	now        func() time.Time
	// after waits between health passes; tests drive it on a fake clock.
	after      func(time.Duration) <-chan time.Time
	templates  Templates
	selfTester *SelfTester
	snapshots  *snapshot.Store
	// accountAdded runs after a wizard account is connected.
	accountAdded func(accountID string)
	// payersMu guards payers, which boot may set after the service is built.
	payersMu sync.RWMutex
	payers   PayerRemover

	mu               sync.Mutex
	principal        string
	principalChecked time.Time
	verifies         map[string][]time.Time
	// confirmed caches, per account, that this process saw its credentials
	// sign in to it, so collectors and cost may read it.
	confirmed map[string]string

	startOnce sync.Once
	stopOnce  sync.Once
	stopped   chan struct{}
	wg        sync.WaitGroup
	verifyWG  sync.WaitGroup
	// healthLocks holds one mutex per account; every read-modify-write of
	// an account's health record runs under it, so a check that probes for
	// a while never overwrites a failure another check recorded meanwhile.
	healthLocks sync.Map
	rootCtx     context.Context
}

// NewService builds the connector service over a store.
func NewService(store *Store, options ...Option) *Service {
	conn := &Service{
		store:      store,
		scope:      store.Scope(),
		readConfig: func() config.AWSConnectorConfig { return config.AWSConnectorConfig{} },
		limiter:    limit.NewManager(2),
		httpClient: awsauth.NewHTTPClient(),
		base:       awsauth.BaseProvider,
		now:        time.Now,
		after:      time.After,
		templates:  DefaultTemplates(),
		verifies:   make(map[string][]time.Time),
		confirmed:  make(map[string]string),
		stopped:    make(chan struct{}),
	}
	conn.assume = func(ctx context.Context, base aws.CredentialsProvider, roleARN, externalID, region string) (aws.CredentialsProvider, error) {
		return awsauth.AssumeRoleWithHTTPClient(ctx, base, awsauth.RoleSpec{RoleARN: roleARN, ExternalID: externalID}, region, conn.httpClient)
	}
	conn.identity = func(ctx context.Context, credentials aws.CredentialsProvider, region string) (string, string, error) {
		return awsauth.CallerIdentity(ctx, aws.Config{Region: region, Credentials: credentials, HTTPClient: conn.httpClient, RetryMaxAttempts: 1})
	}
	for _, option := range options {
		option(conn)
	}
	conn.selfTester = NewSelfTester(store, conn.now)
	return conn
}

// SetBaseProvider replaces the host identity resolver. It is called once at
// boot by the enterprise module when it supplies its own credential chain.
func (conn *Service) SetBaseProvider(base BaseFunc) {
	if base != nil {
		conn.base = base
	}
}

func (conn *Service) connector() config.AWSConnectorConfig {
	if conn == nil || conn.readConfig == nil {
		return config.AWSConnectorConfig{}
	}
	return conn.readConfig()
}

// Enabled reports connectors.aws.enable.
func (conn *Service) Enabled() bool { return conn.connector().Enable }

func (conn *Service) storageOrg() string {
	return conn.scope.Write
}

// Principal returns the Versus principal ARN, resolving it once and caching
// the answer. Failures answer an empty string: the wizard explains how to give
// the server an identity instead of showing a dead control.
func (conn *Service) Principal(ctx context.Context) string {
	conn.mu.Lock()
	cached := conn.principal
	checked := conn.principalChecked
	conn.mu.Unlock()
	if cached != "" {
		return cached
	}
	if !checked.IsZero() && conn.now().Sub(checked) < 5*time.Minute {
		return ""
	}
	arn := ""
	if base, err := conn.base(defaultHomeRegion); err == nil {
		if resolved, _, err := conn.identity(ctx, base, defaultHomeRegion); err == nil {
			arn = resolved
		}
	}
	conn.mu.Lock()
	conn.principal = arn
	conn.principalChecked = conn.now()
	conn.mu.Unlock()
	return arn
}

// credentials resolves the provider for one account. An account with no role
// ARN uses the host identity directly (config only).
func (conn *Service) credentials(ctx context.Context, roleARN, externalID, region string) (aws.CredentialsProvider, error) {
	base, err := conn.base(region)
	if err != nil {
		return nil, fmt.Errorf("%w", ErrNoServerIdentity)
	}
	if roleARN == "" {
		return base, nil
	}
	return conn.assume(ctx, base, roleARN, externalID, region)
}

func newDraftID() (string, error) {
	random := make([]byte, 16)
	if _, err := cryptorand.Read(random); err != nil {
		return "", ErrExternalIDUnavailable
	}
	return hex.EncodeToString(random), nil
}

// CreateDraft starts a wizard draft and generates its external ID server-side.
func (conn *Service) CreateDraft(ctx context.Context, org, method string) (DraftStartView, error) {
	if !conn.Enabled() {
		return DraftStartView{}, ErrDisabled
	}
	method = strings.ToLower(strings.TrimSpace(method))
	switch method {
	case "cloudformation", "terraform", "role_arn", "profile":
	case "organizations":
		if !OrganizationsCapability() {
			return DraftStartView{}, ErrOrganizationsUnavailable
		}
	default:
		return DraftStartView{}, ErrInvalidArguments
	}
	externalID, err := GenerateExternalID()
	if err != nil {
		return DraftStartView{}, err
	}
	draftID, err := newDraftID()
	if err != nil {
		return DraftStartView{}, err
	}
	principal := conn.Principal(ctx)
	now := conn.now().UTC()
	draft := Draft{
		ID:           draftID,
		Method:       method,
		ExternalID:   externalID,
		PrincipalARN: principal,
		Status:       DraftStarted,
		CreatedAt:    now,
		ExpiresAt:    now.Add(DraftTTL),
		Counts:       CountResults(nil),
	}
	if err := conn.store.SaveDraft(org, draft); err != nil {
		return DraftStartView{}, err
	}
	return DraftStartView{
		DraftID:          draft.ID,
		ExternalIDMasked: MaskExternalID(externalID),
		PrincipalARN:     principal,
		TemplateURL:      conn.templates.TemplateURL,
		TerraformSnippet: terraformSnippet(principal, draft.ID),
	}, nil
}

func terraformSnippet(principal, draftID string) string {
	return strings.Join([]string{
		"variable \"versus_principal_arn\" {",
		"  default = " + quoteHCL(principal) + " // empty means this deployment has no AWS identity yet",
		"}",
		"variable \"versus_external_id\" {}",
		"",
		"module \"versus_reader\" {",
		"  source        = \"github.com/versus/versus-aws-reader\"",
		"  principal_arn = var.versus_principal_arn",
		"  external_id   = var.versus_external_id",
		"}",
		"",
		"// Draft " + draftID + " generated this snippet. Reveal the external ID",
		"// through the audited endpoint and pass it as versus_external_id.",
	}, "\n")
}

func quoteHCL(value string) string {
	if value == "" {
		return "\"\""
	}
	return "\"" + strings.ReplaceAll(value, "\"", "\\\"") + "\""
}

// Draft returns one live draft in its API shape.
func (conn *Service) Draft(org, draftID string) (DraftView, error) {
	draft, err := conn.store.GetDraft(org, draftID)
	if err != nil {
		return DraftView{}, err
	}
	return draft.View(), nil
}

// RevealExternalID returns the full external ID for the audited reveal path.
func (conn *Service) RevealExternalID(org, draftID string) (string, error) {
	draft, err := conn.store.GetDraft(org, draftID)
	if err != nil {
		return "", err
	}
	return draft.ExternalID, nil
}

// Verify starts an access check and returns its probe identifier. The work
// runs in the background; the draft GET observes it filling in.
func (conn *Service) Verify(org, draftID, roleARN string) (string, error) {
	if !conn.Enabled() {
		return "", ErrDisabled
	}
	draft, err := conn.store.GetDraft(org, draftID)
	if err != nil {
		return "", err
	}
	roleARN = strings.TrimSpace(roleARN)
	if roleARN == "" {
		roleARN = draft.RoleARN
	}
	if awsauth.ValidateRoleARN(roleARN) != nil {
		return "", ErrInvalidArguments
	}
	if !conn.allowVerify(org) {
		return "", ErrRateLimited
	}
	probeID, err := newDraftID()
	if err != nil {
		return "", err
	}
	draft.RoleARN = roleARN
	draft.ProbeID = probeID
	draft.Status = DraftVerifying
	draft.Assume = AssumeResult{}
	draft.Services = nil
	draft.Counts = CountResults(nil)
	draft.FixPolicy = ""
	draft.ErrorCode = ""
	draft.AccountID = accountIDFromRoleARN(roleARN)
	if err := conn.store.SaveDraft(org, draft); err != nil {
		return "", err
	}
	conn.verifyWG.Add(1)
	go func() {
		defer conn.verifyWG.Done()
		conn.runVerify(org, draft.ID)
	}()
	return probeID, nil
}

func (conn *Service) allowVerify(org string) bool {
	key := tenancy.NormalizeOrgID(org)
	now := conn.now()
	cutoff := now.Add(-verifyWindow)
	conn.mu.Lock()
	defer conn.mu.Unlock()
	grants := conn.verifies[key]
	kept := grants[:0]
	for _, grant := range grants {
		if grant.After(cutoff) {
			kept = append(kept, grant)
		}
	}
	if len(kept) >= verifyLimit {
		conn.verifies[key] = kept
		return false
	}
	conn.verifies[key] = append(kept, now)
	return true
}

func accountIDFromRoleARN(roleARN string) string {
	parts := strings.Split(roleARN, ":")
	if len(parts) < 5 || len(parts[4]) != 12 {
		return ""
	}
	return parts[4]
}

func (conn *Service) runVerify(org, draftID string) {
	ctx, cancel := context.WithTimeout(conn.backgroundContext(), probeTimeout)
	defer cancel()

	draft, err := conn.store.GetDraft(org, draftID)
	if err != nil {
		return
	}
	started := conn.now()
	credentials, err := conn.credentials(ctx, draft.RoleARN, draft.ExternalID, defaultHomeRegion)
	if err != nil {
		conn.finishVerify(org, draft, AssumeResult{OK: false, LatencyMS: conn.latency(started), ErrorCode: errorCode(err)}, nil, "")
		return
	}
	arn, accountID, err := conn.identity(ctx, credentials, defaultHomeRegion)
	if err != nil {
		conn.finishVerify(org, draft, AssumeResult{OK: false, LatencyMS: conn.latency(started), ErrorCode: errorCode(err)}, nil, "")
		return
	}
	draft.AccountID = accountID
	assume := AssumeResult{OK: true, LatencyMS: conn.latency(started)}
	if err := conn.store.SaveDraft(org, draft); err != nil {
		return
	}
	if arn != "" && draft.PrincipalARN == "" {
		draft.PrincipalARN = arn
	}

	prober := probe.New(accountID, credentials, conn.limiter, probe.WithHTTPClient(conn.httpClient))
	services := make([]probe.Result, 0, len(catalog.Services()))
	for _, entry := range catalog.Services() {
		if err := ctx.Err(); err != nil {
			break
		}
		services = append(services, prober.Probe(ctx, entry, defaultHomeRegion))
		current, readErr := conn.store.GetDraft(org, draftID)
		if readErr != nil {
			return
		}
		current.AccountID = accountID
		current.PrincipalARN = draft.PrincipalARN
		current.Assume = assume
		current.Services = append([]probe.Result(nil), services...)
		current.Counts = CountResults(current.Services)
		if err := conn.store.SaveDraft(org, current); err != nil {
			return
		}
	}
	conn.finishVerify(org, draft, assume, services, accountID)
}

func (conn *Service) finishVerify(org string, draft Draft, assume AssumeResult, services []probe.Result, accountID string) {
	current, err := conn.store.GetDraft(org, draft.ID)
	if err != nil {
		return
	}
	current.AccountID = accountID
	current.Assume = assume
	current.Status = DraftVerified
	current.VerifiedAt = conn.now().UTC()
	if services != nil {
		current.Services = services
		current.Counts = CountResults(services)
	}
	if denied := current.Counts.Denied; len(denied) > 0 {
		results := make([]probe.Result, 0, len(denied))
		for _, conn := range current.Services {
			if conn.Status == probe.StatusDenied {
				results = append(results, conn)
			}
		}
		if policy, policyErr := probe.BuildFixPolicy(results); policyErr == nil {
			current.FixPolicy = policy
		}
	}
	_ = conn.store.SaveDraft(org, current)
}

func (conn *Service) latency(started time.Time) int {
	elapsed := conn.now().Sub(started).Milliseconds()
	if elapsed < 0 {
		return 0
	}
	if elapsed > int64(^uint(0)>>1) {
		return int(^uint(0) >> 1)
	}
	return int(elapsed)
}

func errorCode(err error) string {
	if err == nil {
		return ""
	}
	if code := awsauth.ErrorCode(err); code != "" {
		return code
	}
	if errors.Is(err, ErrNoServerIdentity) {
		return ErrNoServerIdentity.Error()
	}
	if errors.Is(err, ErrAccountMismatch) {
		return ErrAccountMismatch.Error()
	}
	return awsauth.ErrNetwork.Error()
}

// CountResults builds the grid summary in catalog order. A service without a
// result is not checked, never zero and never denied.
func CountResults(results []probe.Result) ServiceCounts {
	byService := make(map[string]probe.Result, len(results))
	for _, result := range results {
		byService[result.Service] = result
	}
	counts := ServiceCounts{
		Total:              len(catalog.Services()),
		Denied:             []string{},
		DeniedServices:     []string{},
		NotOnPlan:          []string{},
		NotOnPlanServices:  []string{},
		NotCheckedServices: []string{},
	}
	for _, entry := range catalog.Services() {
		result, ok := byService[entry.ID]
		if !ok {
			counts.NotChecked++
			counts.NotCheckedServices = append(counts.NotCheckedServices, entry.ID)
			continue
		}
		switch result.Status {
		case probe.StatusReadable:
			counts.Readable++
		case probe.StatusNotUsed:
			counts.NotUsed++
		case probe.StatusDenied:
			counts.Denied = append(counts.Denied, entry.ID)
			counts.DeniedServices = append(counts.DeniedServices, entry.ID)
		case probe.StatusNotOnPlan:
			counts.NotOnPlan = append(counts.NotOnPlan, entry.ID)
			counts.NotOnPlanServices = append(counts.NotOnPlanServices, entry.ID)
		default:
			counts.NotChecked++
			counts.NotCheckedServices = append(counts.NotCheckedServices, entry.ID)
		}
	}
	return counts
}

// CheckAccount runs one identity check for an account and records attempt and
// success separately. Two consecutive identity failures mark it disconnected.
// A healthy account re-probes at most every ServiceProbeInterval.
func (conn *Service) CheckAccount(ctx context.Context, account AccountRecord) (HealthRecord, error) {
	return conn.checkAccount(ctx, account, false)
}

// checkAccount is the identity check shared by the scheduled loop and the
// explicit Recheck, which forces a fresh service probe.
func (conn *Service) checkAccount(ctx context.Context, account AccountRecord, forceProbe bool) (HealthRecord, error) {
	defer conn.lockHealth(account.AccountID)()
	previous, err := conn.store.GetHealth(conn.storageOrg(), account.AccountID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return HealthRecord{}, err
	}
	now := conn.now().UTC()
	record := previous
	record.AccountID = account.AccountID
	record.AttemptedAt = now
	record.AttemptOK = false

	home := homeRegion(account.Regions)
	credentials, identityErr := conn.credentials(ctx, account.RoleARN, account.ExternalID, home)
	if identityErr == nil {
		_, accountID, err := conn.identity(ctx, credentials, home)
		if err != nil {
			identityErr = err
		} else if accountID != "" && accountID != account.AccountID {
			// The credentials sign in, but to another account: reading
			// through them would file that account's data under this one.
			identityErr = ErrAccountMismatch
		}
	}

	conn.setIdentityConfirmed(account, identityErr == nil)
	if identityErr != nil {
		recordIdentityFailure(&record, identityErr, now)
		if saveErr := conn.store.SaveHealth(conn.storageOrg(), record); saveErr != nil {
			return record, saveErr
		}
		return record, nil
	}

	record.AttemptOK = true
	record.Status = StatusConnected
	record.CheckedAt = now
	record.DisconnectedSince = time.Time{}
	record.FailureSince = time.Time{}
	record.ErrorCode = ""
	record.ConsecutiveFailures = 0
	// The identity result is saved as soon as it is known, so Settings shows
	// the account connected while the slower region and service reads run.
	if saveErr := conn.store.SaveHealth(conn.storageOrg(), record); saveErr != nil {
		return record, saveErr
	}

	due := forceProbe || len(record.ProbeResults) == 0 || record.ProbedAt.IsZero() || now.Sub(record.ProbedAt) >= ServiceProbeInterval
	switch {
	case len(account.Regions) == 0 && len(record.Regions) == 0:
		// An account configured with "regions: auto" has no selection yet;
		// the first healthy check discovers the account's regions so the
		// table and the budget show real numbers instead of 0 of 0.
		if discovered, discoverErr := conn.discoverAccountRegions(ctx, account.AccountID, credentials, home); discoverErr == nil && len(discovered) > 0 {
			record.Regions = discovered
			conn.markRegionsRead(&record, now)
		}
	case due || optInStale(account.Regions, record, now):
		// Opt-in status is read again on the probe cadence, and on every
		// check while a Region that matters is not enabled, for selected
		// and auto accounts alike: a Region opted in later becomes expected
		// and read, and one not enabled is named instead of read as quiet.
		// A refused lookup keeps what was known.
		if statuses, optInErr := conn.describeRegions(ctx, account.AccountID, credentials, home); optInErr == nil && len(statuses) > 0 {
			record.Regions = keepResourcesFound(statuses, record.Regions)
			conn.markRegionsRead(&record, now)
		}
	}

	probeRegions := account.Regions
	if len(probeRegions) == 0 {
		probeRegions = enabledRegions(record.Regions)
	}
	if len(probeRegions) > 0 {
		conn.limiter.Expect(account.AccountID, probeRegions)
	}
	if due {
		prober := probe.New(account.AccountID, credentials, conn.limiter, probe.WithHTTPClient(conn.httpClient))
		record.ProbeResults = prober.ProbeAll(ctx, probeRegion(probeRegions, record.Regions))
		record.ProbedAt = now
	}
	if saveErr := conn.store.SaveHealth(conn.storageOrg(), record); saveErr != nil {
		return record, saveErr
	}
	return record, nil
}

// recordIdentityFailure notes one failed identity check on record.
func recordIdentityFailure(record *HealthRecord, identityErr error, now time.Time) {
	record.AttemptedAt = now
	record.AttemptOK = false
	record.ErrorCode = errorCode(identityErr)
	record.ConsecutiveFailures++
	if record.FailureSince.IsZero() {
		record.FailureSince = now
	}
	// A wrong account does not clear on a retry, so it disconnects at
	// once; other failures wait for a second one in a row.
	if record.ConsecutiveFailures >= 2 || errors.Is(identityErr, ErrAccountMismatch) {
		record.Status = StatusDisconnected
		record.DisconnectedSince = record.FailureSince
	} else if record.Status == "" || record.Status == StatusUnknown {
		record.Status = StatusChecking
	}
}

// lockHealth takes the account's health-record lock and returns its
// release.
func (conn *Service) lockHealth(accountID string) func() {
	value, _ := conn.healthLocks.LoadOrStore(accountID, &sync.Mutex{})
	lock := value.(*sync.Mutex)
	lock.Lock()
	return lock.Unlock
}

// recordRegionStatus stores the account's Region opt-in status on its
// health record. A refused or failed lookup leaves the record as it was.
func (conn *Service) recordRegionStatus(ctx context.Context, org string, account AccountRecord) {
	home := homeRegion(account.Regions)
	credentials, err := conn.credentials(ctx, account.RoleARN, account.ExternalID, home)
	if err != nil {
		return
	}
	statuses, err := conn.describeRegions(ctx, account.AccountID, credentials, home)
	if err != nil || len(statuses) == 0 {
		return
	}
	defer conn.lockHealth(account.AccountID)()
	health, err := conn.store.GetHealth(org, account.AccountID)
	if err != nil {
		return
	}
	health.Regions = keepResourcesFound(statuses, health.Regions)
	conn.markRegionsRead(&health, conn.now().UTC())
	_ = conn.store.SaveHealth(org, health)
}

// keepResourcesFound carries each Region's resources-found mark from the
// last discovery over to a fresh opt-in read.
func keepResourcesFound(fresh, previous []RegionStatus) []RegionStatus {
	found := make(map[string]bool, len(previous))
	for _, region := range previous {
		found[region.Region] = region.ResourcesFound
	}
	for index := range fresh {
		fresh[index].ResourcesFound = fresh[index].Enabled && found[fresh[index].Region]
	}
	return fresh
}

// optInStale reports whether the account's Region opt-in status should be
// read again now: never read, no longer fresh, or a Region that matters to
// the account is recorded as not enabled (an opt-in can happen at any time,
// and the collector polls the Region only once it is known enabled).
func optInStale(selected []string, record HealthRecord, now time.Time) bool {
	if len(record.Regions) == 0 || !record.regionsFresh(now) {
		return true
	}
	for _, region := range record.Regions {
		if !region.Enabled && (len(selected) == 0 || slices.Contains(selected, region.Region)) {
			return true
		}
	}
	return false
}

// healthInterval is the configured identity-check cadence.
func (conn *Service) healthInterval() time.Duration {
	if parsed, err := time.ParseDuration(conn.connector().HealthCheckInterval); err == nil && parsed > 0 {
		return parsed
	}
	return DefaultHealthCheckInterval
}

// markRegionsRead records a successful opt-in read: its status counts as
// confirmed for two health intervals, so one refused read in between does
// not turn it unknown, and a Region refused for longer is polled again.
func (conn *Service) markRegionsRead(record *HealthRecord, now time.Time) {
	record.RegionsReadAt = now
	record.RegionsFreshUntil = now.Add(2 * conn.healthInterval())
}

// regionsFresh reports whether the recorded opt-in status is still
// confirmed at now.
func (record HealthRecord) regionsFresh(now time.Time) bool {
	return !record.RegionsFreshUntil.IsZero() && now.Before(record.RegionsFreshUntil)
}

// partitionHomes are the Regions every account of a partition has enabled.
var partitionHomes = []struct{ prefix, home string }{
	{"cn-", "cn-north-1"},
	{"us-gov-", "us-gov-west-1"},
}

// homeRegion is where an account's STS calls are signed. It is the
// partition's always-enabled Region (us-east-1 in the standard partition),
// chosen from the partition of the selected Regions and never from their
// sort order: the first selected Region may be an opt-in Region the account
// has not enabled (af-south-1 sorts first), and signing STS there would
// make the whole account look like bad credentials.
func homeRegion(regions []string) string {
	for _, region := range regions {
		if awsauth.ValidateRegion(region) != nil {
			continue
		}
		for _, partition := range partitionHomes {
			if strings.HasPrefix(region, partition.prefix) {
				return partition.home
			}
		}
		if strings.HasPrefix(region, "us-iso") {
			// Isolated partitions have no fixed home; use the selection.
			return region
		}
		return defaultHomeRegion
	}
	return defaultHomeRegion
}

// probeRegion is where an account's service probes run: the first selected
// Region not known to be disabled, else the home Region.
func probeRegion(selected []string, known []RegionStatus) string {
	disabled := make(map[string]bool, len(known))
	for _, region := range known {
		disabled[region.Region] = !region.Enabled
	}
	for _, region := range selected {
		if awsauth.ValidateRegion(region) == nil && !disabled[region] {
			return region
		}
	}
	return homeRegion(selected)
}

// NotEnabledRegions lists the selected Regions confirmed not enabled in the
// account now (an opt-in Region it has not opted in to), in the selection's
// order. See NotEnabledRegionsAt.
func (record HealthRecord) NotEnabledRegions(selected []string) []string {
	return record.NotEnabledRegionsAt(selected, time.Now())
}

// NotEnabledRegionsAt lists the selected Regions confirmed not enabled at
// now. A Region whose status is not known, or whose last successful read is
// no longer fresh (refused for about two health intervals), is not listed:
// it is polled, and its errors are real gaps.
func (record HealthRecord) NotEnabledRegionsAt(selected []string, now time.Time) []string {
	if !record.regionsFresh(now) {
		return []string{}
	}
	known := make(map[string]bool, len(record.Regions))
	for _, region := range record.Regions {
		known[region.Region] = region.Enabled
	}
	out := []string{}
	for _, region := range selected {
		if enabled, ok := known[region]; ok && !enabled {
			out = append(out, region)
		}
	}
	return out
}

// Recheck runs an identity check and a fresh service probe now, ignoring the
// scheduled six-hour probe window.
func (conn *Service) Recheck(ctx context.Context, org, accountID string) (AccountView, error) {
	account, _, err := conn.effectiveAccount(org, accountID)
	if err != nil {
		return AccountView{}, err
	}
	if _, err := conn.checkAccount(ctx, account, true); err != nil {
		return AccountView{}, err
	}
	return conn.accountView(org, account, false)
}

// effectiveAccount returns one merged account and whether it came from config.
func (conn *Service) effectiveAccount(org, accountID string) (AccountRecord, bool, error) {
	if !ValidAccountID(accountID) {
		return AccountRecord{}, false, ErrInvalidArguments
	}
	stored, err := conn.store.ListAccounts(org)
	if err != nil {
		return AccountRecord{}, false, err
	}
	configured := false
	for _, account := range conn.connector().Accounts {
		if account.AccountID == accountID {
			configured = true
		}
	}
	merged := MergeAccounts(stored, conn.connector().Accounts)
	for _, account := range merged {
		if account.AccountID == accountID {
			return account, configured, nil
		}
	}
	return AccountRecord{}, configured, ErrNotFound
}

// Start launches the health and self-test loops. It is inert unless the
// connector is enabled, and both loops stop when ctx is cancelled.
func (conn *Service) Start(ctx context.Context) {
	if conn == nil || ctx == nil || !conn.Enabled() {
		if conn != nil {
			conn.stopOnce.Do(func() { close(conn.stopped) })
		}
		return
	}
	conn.startOnce.Do(func() {
		conn.rootCtx = ctx
		options, _ := conn.store.LoadOptions(conn.storageOrg())
		conn.applyBudget(options)
		conn.wg.Add(2)
		go func() {
			defer conn.wg.Done()
			conn.healthLoop(ctx)
		}()
		go func() {
			defer conn.wg.Done()
			conn.selfTestLoop(ctx)
		}()
		go func() {
			conn.wg.Wait()
			conn.stopOnce.Do(func() { close(conn.stopped) })
		}()
	})
}

// Stopped closes once the background loops have exited.
func (conn *Service) Stopped() <-chan struct{} {
	if conn == nil {
		closed := make(chan struct{})
		close(closed)
		return closed
	}
	return conn.stopped
}

func (conn *Service) backgroundContext() context.Context {
	conn.mu.Lock()
	defer conn.mu.Unlock()
	if conn.rootCtx != nil {
		return conn.rootCtx
	}
	return context.Background()
}

// healthRetryFirst is the first wait after a failed identity check.
const healthRetryFirst = 30 * time.Second

func (conn *Service) healthLoop(ctx context.Context) {
	interval := DefaultHealthCheckInterval
	if parsed, err := time.ParseDuration(conn.connector().HealthCheckInterval); err == nil && parsed > 0 {
		interval = parsed
	}
	failedPasses := 0
	for {
		if conn.checkAll(ctx) {
			failedPasses++
		} else {
			failedPasses = 0
		}
		select {
		case <-ctx.Done():
			return
		case <-conn.after(jitter(healthWait(failedPasses, interval))):
		}
	}
}

// healthWait is the wait before the next health pass. A pass with a failed
// identity check is retried soon, so a passing failure (an endpoint not
// ready at start-up) clears in seconds and a real outage reaches
// disconnected (two failures in a row) within about a minute. The retry
// backs off, 30 s, 60 s, 120 s, up to the interval, so a steady failure
// polls no faster than a healthy account and its state never flaps.
func healthWait(failedPasses int, interval time.Duration) time.Duration {
	if failedPasses <= 0 {
		return interval
	}
	wait := healthRetryFirst
	for pass := 1; pass < failedPasses && wait < interval; pass++ {
		wait *= 2
	}
	return min(wait, interval)
}

// healthCheckParallel bounds the accounts one health pass checks at once.
const healthCheckParallel = 4

// checkAll checks every account, a few at a time so one slow account does
// not hold the others at "unknown", and reports whether any identity check
// failed.
func (conn *Service) checkAll(ctx context.Context) bool {
	accounts, err := conn.store.ListAccounts(conn.storageOrg())
	if err != nil {
		return true
	}
	var (
		mu     sync.Mutex
		failed bool
		group  sync.WaitGroup
	)
	slots := make(chan struct{}, healthCheckParallel)
	for _, account := range MergeAccounts(accounts, conn.connector().Accounts) {
		if ctx.Err() != nil {
			break
		}
		slots <- struct{}{}
		group.Add(1)
		go func(account AccountRecord) {
			defer group.Done()
			defer func() { <-slots }()
			record, err := conn.CheckAccount(ctx, account)
			if err != nil || !record.AttemptOK {
				mu.Lock()
				failed = true
				mu.Unlock()
			}
		}(account)
	}
	group.Wait()
	return failed
}

func jitter(interval time.Duration) time.Duration {
	spread := interval / 10
	if spread <= 0 {
		return interval
	}
	offset := time.Duration(mathrand.Int64N(int64(spread)*2+1)) - spread
	if interval+offset <= 0 {
		return interval
	}
	return interval + offset
}

// Principal is who asks for the connection view: the organization it reads,
// whether it may change settings, and whether it is an agent. An agent read
// never looks up the server identity, because that is an AWS call and agent
// reads use stored state only.
type Principal struct {
	Org      string
	CanWrite bool
	Agent    bool
}

// AgentPrincipal is the principal an agent tool reads as: the request's
// write organization when the context carries a scope, else the
// deployment's, read-only.
func (conn *Service) AgentPrincipal(ctx context.Context) Principal {
	org := conn.storageOrg()
	if scope, ok := tenancy.ContextOrgScope(ctx); ok && scope.Normalized().Write != "" {
		org = scope.Normalized().Write
	}
	return Principal{Org: org, Agent: true}
}

// Connection builds the connection view: accounts, health, the service
// summary, regions, API use, freshness and the privacy self-test. It is the
// one method behind GET /api/admin/aws/settings and the get_aws_connection
// agent tool. When the connector is off it answers only enabled=false so the
// UI can explain how to turn it on. It never starts a probe.
func (conn *Service) Connection(ctx context.Context, principal Principal) (SettingsView, error) {
	org, canWrite := principal.Org, principal.CanWrite
	connector := conn.connector()
	if !connector.Enable {
		return SettingsView{Enabled: false}, nil
	}
	stored, err := conn.store.ListAccounts(org)
	if err != nil {
		return SettingsView{}, err
	}
	options, _ := conn.store.LoadOptions(org)
	conn.applyBudget(options)
	storedSet := make(map[string]bool, len(stored))
	for _, account := range stored {
		storedSet[account.AccountID] = true
	}
	merged := MergeAccounts(stored, connector.Accounts)
	principalARN := ""
	if !principal.Agent {
		principalARN = conn.Principal(ctx)
	}
	view := SettingsView{
		Enabled:      true,
		PrincipalARN: principalARN,
		Accounts:     make([]AccountView, 0, len(merged)),
		Capabilities: CapabilitiesView{Organizations: OrganizationsCapability(), CanWrite: canWrite},
	}
	usedRPS := 0.0
	for _, account := range merged {
		row, rowErr := conn.accountViewWithOptions(org, account, storedSet[account.AccountID] && account.Source == SourceConfig, options)
		if rowErr != nil {
			return SettingsView{}, rowErr
		}
		usedRPS += row.APIUse.UsedRPS
		view.Accounts = append(view.Accounts, row)
	}
	view.Budget = BudgetView{
		UsedRPS:   usedRPS,
		BudgetRPS: conn.effectiveBudget(options),
		Source:    budgetSource(options, connector),
	}
	view.OptionalData = optionalDataView(connector, options, len(merged))
	selfTest, _ := conn.store.LoadSelfTest(org)
	allowlist, allowlistSource := conn.allowlistWithSource(org)
	view.Privacy = PrivacyView{
		Sent:                  []string{"resource_names_types_counts", "alarm_names_states", "allowlisted_tag_values"},
		NeverSent:             []string{"secrets_env_user_data", "log_lines_request_bodies", "other_tag_values"},
		TagValueAllowlist:     allowlist,
		TagValueAllowlistFrom: allowlistSource,
		AI:                    AIView{Enabled: conn.ai.Enabled, Provider: conn.ai.Provider, Model: conn.ai.Model},
		SelfTest:              selfTest,
	}
	return view, nil
}

// effectiveBudget reports the steady request rate per account and region. A
// saved wizard value wins over the boot config, and a missing value falls back
// to the documented default.
func (conn *Service) effectiveBudget(options Options) int {
	if options.MaxRequestsPerSecond != nil && *options.MaxRequestsPerSecond > 0 {
		return *options.MaxRequestsPerSecond
	}
	budget := conn.connector().Budgets.MaxRequestsPerSecond
	if budget <= 0 {
		return DefaultBudgetRPS
	}
	return budget
}

// sourceDefault marks a value nothing sets, so the UI leaves the control
// editable. A config source is reserved for a value the config actually sets to
// a non-default answer.
const sourceDefault = "default"

func budgetSource(options Options, connector config.AWSConnectorConfig) string {
	if options.MaxRequestsPerSecond != nil {
		return string(SourceWizard)
	}
	if configured := connector.Budgets.MaxRequestsPerSecond; configured > 0 && configured != DefaultBudgetRPS {
		return string(SourceConfig)
	}
	return sourceDefault
}

// applyBudget pushes the effective budget into the shared limiter so a saved
// setting takes effect at runtime.
func (conn *Service) applyBudget(options Options) {
	if conn.limiter != nil {
		conn.limiter.SetMaxRPS(conn.effectiveBudget(options))
	}
}

// allowlistWithSource resolves the tag-value allowlist the same way for the
// privacy card and the self-test: config wins, then the list the user saved
// (an empty one included), then the default.
func (conn *Service) allowlistWithSource(org string) ([]string, string) {
	if configured := conn.connector().Tags.ValueAllowlist; len(configured) > 0 {
		return SanitizeAllowlist(configured), string(SourceConfig)
	}
	if stored, err := conn.store.LoadPrivacy(org); err == nil {
		if saved, ok := userAllowlist(stored); ok {
			return SanitizeAllowlist(saved), string(SourceWizard)
		}
	}
	return DefaultTagValueAllowlist(), sourceDefault
}

func optionalDataView(connector config.AWSConnectorConfig, options Options, accounts int) OptionalDataView {
	count := func(configured bool, stored *bool) OptionalCount {
		source := sourceDefault
		switch {
		case configured:
			source = string(SourceConfig)
		case stored != nil:
			source = string(SourceWizard)
		}
		enabled := configured || (stored != nil && *stored)
		if !enabled {
			return OptionalCount{On: 0, Source: source}
		}
		return OptionalCount{On: accounts, Source: source}
	}
	return OptionalDataView{
		CostExplorer:   count(connector.OptionalData.CostExplorer, options.CostExplorer),
		Config:         count(connector.OptionalData.Config, options.Config),
		SecurityHub:    count(connector.OptionalData.SecurityHub, options.SecurityHub),
		TrustedAdvisor: count(connector.OptionalData.TrustedAdvisor, options.TrustedAdvisor),
	}
}

func (conn *Service) accountView(org string, account AccountRecord, conflict bool) (AccountView, error) {
	options, _ := conn.store.LoadOptions(org)
	return conn.accountViewWithOptions(org, account, conflict, options)
}

func (conn *Service) accountViewWithOptions(org string, account AccountRecord, conflict bool, options Options) (AccountView, error) {
	health, err := conn.store.GetHealth(org, account.AccountID)
	if err != nil {
		return AccountView{}, err
	}
	selected, enabled, notEnabled, unknown := regionSelection(account.Regions, health, conn.now().UTC())
	budget := conn.effectiveBudget(options)
	if len(selected) > 0 {
		conn.limiter.Expect(account.AccountID, selected)
	}
	usage := conn.limiter.Usage(account.AccountID)
	if usage.BudgetRPS == 0 && len(selected) > 0 {
		usage.BudgetRPS = budget * len(selected)
	}
	view := AccountView{
		AccountID:   account.AccountID,
		DisplayName: account.DisplayName,
		Source:      account.Source,
		Status:      health.Status,
		CheckedAt:   health.CheckedAt,
		Services:    CountResults(health.ProbeResults),
		Regions:     regionsView(selected, enabled, notEnabled, unknown, health),
		APIUse:      APIUseView{UsedRPS: usage.UsedRPS, BudgetRPS: usage.BudgetRPS},
		RoleARN:     account.RoleARN,
		Conflict:    conflict,
		Sync:        conn.syncView(account.AccountID),
	}
	if view.Status == "" {
		view.Status = StatusUnknown
	}
	if !health.DisconnectedSince.IsZero() {
		since := health.DisconnectedSince
		view.DisconnectedSince = &since
	}
	if view.Status == StatusDisconnected && health.ErrorCode != "" {
		code := health.ErrorCode
		view.ErrorCode = &code
	}
	return view, nil
}

// regionSelection splits the selection by what the last opt-in read said.
// A Region read as not enabled is listed as not enabled only while that
// read is fresh; after that its status is unknown.
func regionSelection(configured []string, health HealthRecord, now time.Time) ([]string, int, []string, []string) {
	discovered := health.Regions
	enabledMap := make(map[string]bool, len(discovered))
	for _, region := range discovered {
		enabledMap[region.Region] = region.Enabled
	}
	selected := append([]string(nil), configured...)
	if len(selected) == 0 {
		for _, region := range discovered {
			if region.Enabled {
				selected = append(selected, region.Region)
			}
		}
	}
	sort.Strings(selected)
	if selected == nil {
		selected = []string{}
	}
	fresh := health.regionsFresh(now)
	notEnabled, unknown := []string{}, []string{}
	enabled := 0
	for _, region := range selected {
		if available, known := enabledMap[region]; known && !available {
			if fresh {
				notEnabled = append(notEnabled, region)
			} else {
				unknown = append(unknown, region)
			}
			continue
		}
		enabled++
	}
	return selected, enabled, notEnabled, unknown
}

func regionsView(selected []string, enabled int, notEnabled, unknown []string, health HealthRecord) RegionsView {
	view := RegionsView{Selected: selected, Enabled: enabled, NotEnabled: notEnabled, StatusUnknown: unknown}
	if !health.RegionsReadAt.IsZero() {
		at := health.RegionsReadAt.UTC()
		view.StatusReadAt = &at
	}
	return view
}

// SaveOptions persists the wizard's optional-data and budget overrides and
// applies the new budget to the running limiter.
func (conn *Service) SaveOptions(org string, options Options) error {
	if options.MaxRequestsPerSecond != nil {
		if *options.MaxRequestsPerSecond < 1 || *options.MaxRequestsPerSecond > 1000 {
			return ErrInvalidArguments
		}
	}
	if err := conn.store.SaveOptions(org, options); err != nil {
		return err
	}
	conn.applyBudget(options)
	return nil
}

// SavePrivacy persists the tag-value allowlist.
func (conn *Service) SavePrivacy(org string, allowlist []string) error {
	if len(allowlist) > 32 {
		return ErrInvalidArguments
	}
	clean := SanitizeAllowlist(allowlist)
	return conn.store.UpdatePrivacy(org, func(privacy *privacyRecord) {
		privacy.TagValueAllowlist = clean
		privacy.Saved = true
	})
}

// Commit turns a verified draft into a stored account.
func (conn *Service) Commit(ctx context.Context, org, draftID, displayName string, regions []string) (AccountView, error) {
	if !conn.Enabled() {
		return AccountView{}, ErrDisabled
	}
	draft, err := conn.store.GetDraft(org, draftID)
	if err != nil {
		return AccountView{}, err
	}
	if draft.Status != DraftVerified || !draft.Assume.OK {
		return AccountView{}, ErrNotVerified
	}
	if draft.AccountID == "" {
		draft.AccountID = accountIDFromRoleARN(draft.RoleARN)
	}
	if !ValidAccountID(draft.AccountID) {
		return AccountView{}, ErrInvalidArguments
	}
	displayName = strings.TrimSpace(displayName)
	if displayName == "" || len(displayName) > 80 || strings.ContainsAny(displayName, "\x00\r\n") {
		return AccountView{}, ErrInvalidArguments
	}
	regions, err = normalizeRegions(regions, draft.Regions)
	if err != nil {
		return AccountView{}, err
	}
	for _, configured := range conn.connector().Accounts {
		if configured.AccountID == draft.AccountID {
			return AccountView{}, ErrConfigManaged
		}
	}
	record := AccountRecord{
		AccountID:   draft.AccountID,
		DisplayName: displayName,
		RoleARN:     draft.RoleARN,
		ExternalID:  draft.ExternalID,
		Regions:     regions,
		CreatedAt:   conn.now().UTC(),
		Source:      SourceWizard,
	}
	if err := conn.store.SaveAccount(org, record); err != nil {
		return AccountView{}, err
	}
	unlock := conn.lockHealth(draft.AccountID)
	health := HealthRecord{
		AccountID:    draft.AccountID,
		Status:       StatusConnected,
		AttemptedAt:  conn.now().UTC(),
		AttemptOK:    true,
		CheckedAt:    conn.now().UTC(),
		ProbedAt:     conn.now().UTC(),
		ProbeResults: draft.Services,
		Regions:      draft.Regions,
	}
	if len(draft.Regions) > 0 {
		conn.markRegionsRead(&health, conn.now().UTC())
	}
	err = conn.store.SaveHealth(org, health)
	unlock()
	if err != nil {
		return AccountView{}, err
	}
	_ = conn.store.DeleteDraft(org, draftID)
	if len(draft.Regions) == 0 {
		// The wizard skipped region discovery: read the selected Regions'
		// opt-in status now, so a not-enabled one is named from the start
		// instead of after the next health pass.
		conn.verifyWG.Add(1)
		go func() {
			defer conn.verifyWG.Done()
			conn.recordRegionStatus(conn.backgroundContext(), org, record)
		}()
	}
	if conn.accountAdded != nil {
		conn.accountAdded(record.AccountID)
	}
	return conn.accountView(org, record, false)
}

func normalizeRegions(requested []string, discovered []RegionStatus) ([]string, error) {
	if len(requested) == 0 {
		for _, region := range discovered {
			if region.Enabled {
				requested = append(requested, region.Region)
			}
		}
	}
	if len(requested) == 0 {
		return nil, ErrInvalidArguments
	}
	enabled := make(map[string]bool, len(discovered))
	known := make(map[string]bool, len(discovered))
	for _, region := range discovered {
		known[region.Region] = true
		enabled[region.Region] = region.Enabled
	}
	clean := make([]string, 0, len(requested))
	seen := make(map[string]struct{}, len(requested))
	for _, region := range requested {
		region = strings.TrimSpace(region)
		if awsauth.ValidateRegion(region) != nil {
			return nil, ErrInvalidArguments
		}
		if known[region] && !enabled[region] {
			return nil, ErrInvalidArguments
		}
		if _, ok := seen[region]; ok {
			continue
		}
		seen[region] = struct{}{}
		clean = append(clean, region)
	}
	if len(clean) == 0 {
		return nil, ErrInvalidArguments
	}
	sort.Strings(clean)
	return clean, nil
}

// UpdateAccount changes a wizard account's display name or regions.
func (conn *Service) UpdateAccount(org, accountID, displayName string, regions []string) (AccountView, error) {
	var removed []string
	account, configured, err := conn.effectiveAccount(org, accountID)
	if err != nil {
		return AccountView{}, err
	}
	if configured {
		return AccountView{}, ErrConfigManaged
	}
	if displayName != "" {
		displayName = strings.TrimSpace(displayName)
		if len(displayName) > 80 || strings.ContainsAny(displayName, "\x00\r\n") {
			return AccountView{}, ErrInvalidArguments
		}
		account.DisplayName = displayName
	}
	if regions != nil {
		health, _ := conn.store.GetHealth(org, accountID)
		clean, normalizeErr := normalizeRegions(regions, health.Regions)
		if normalizeErr != nil {
			return AccountView{}, normalizeErr
		}
		removed = removedRegions(account.Regions, clean)
		account.Regions = clean
	}
	account.Source = SourceWizard
	if err := conn.store.SaveAccount(org, account); err != nil {
		return AccountView{}, err
	}
	if conn.snapshots != nil {
		for _, region := range removed {
			if err := conn.snapshots.PurgeRegion(accountID, region); err != nil {
				return AccountView{}, classifyStoreError(err)
			}
		}
	}
	return conn.accountView(org, account, false)
}

// removedRegions lists the regions in before that are not in after.
func removedRegions(before, after []string) []string {
	kept := make(map[string]bool, len(after))
	for _, region := range after {
		kept[region] = true
	}
	var removed []string
	for _, region := range before {
		if !kept[region] {
			removed = append(removed, region)
		}
	}
	return removed
}

// costPurgeWait bounds how long an account removal waits for a running
// cost refresh before it leaves the cost purge to that refresh.
const costPurgeWait = 5 * time.Second

// AccountRemoval is the outcome of a removed account. PurgePending is true
// when its cost data was not deleted yet: the account is gone and kept out
// of collection, and the next cost refresh deletes the data.
type AccountRemoval struct {
	PurgePending bool
}

// DeleteAccount removes a wizard account and its health record. The
// account's inventory and change history are purged first and the cost data
// is marked removed; the settings record is deleted after, so a purge that
// cannot reach its store leaves the record in place and a later attempt can
// finish the delete. The cost data is then deleted if no cost refresh is
// running within costPurgeWait; otherwise the removal answers with the purge
// pending, and the refresh deletes it. No account lock is held meanwhile.
func (conn *Service) DeleteAccount(ctx context.Context, org, accountID string) (AccountRemoval, error) {
	_, configured, err := conn.effectiveAccount(org, accountID)
	if err != nil {
		return AccountRemoval{}, err
	}
	if configured {
		return AccountRemoval{}, ErrConfigManaged
	}
	remover := conn.payerRemover()
	// Purge what the account produced before forgetting it: a read must never
	// keep serving an account that is no longer connected. The cost tombstone
	// is stored under the store's account lock with the revision read before
	// the removal, so the tombstone and the removal agree on it.
	err = conn.store.RemoveAccount(org, accountID, func(revision uint64) error {
		if conn.snapshots != nil {
			if err := conn.snapshots.PurgeAccount(accountID); err != nil {
				return classifyStoreError(err)
			}
		}
		if remover != nil {
			if err := remover.MarkPayerRemoved(ctx, accountID, revision); err != nil {
				return classifyStoreError(err)
			}
		}
		return nil
	})
	switch {
	case errors.Is(err, ErrRevisionContended):
		// The record is gone and the tombstone was stored at the revision
		// the options still read, so the account stays out of collection;
		// the next cost refresh purges it.
		return AccountRemoval{PurgePending: remover != nil}, nil
	case err != nil:
		return AccountRemoval{}, err
	}
	if remover == nil {
		return AccountRemoval{}, nil
	}
	purged, err := remover.PurgePayer(ctx, accountID, costPurgeWait)
	if err != nil {
		log.Printf("aws settings: account %s removed; its cost data is left to the next cost refresh: %v", accountID, err)
	}
	return AccountRemoval{PurgePending: !purged}, nil
}

// classifyStoreError wraps a snapshot store's "no inventory tables" sentinel
// in the connector's closed storage error, so the API answers storage
// unavailable instead of an internal failure. Other errors pass through.
func classifyStoreError(err error) error {
	if err != nil && errors.Is(err, snapshot.ErrNoStorage) {
		return fmt.Errorf("%w: %w", ErrNoStorage, err)
	}
	return err
}
