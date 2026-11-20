//go:build awsemulator

package awsemulator

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/VersusControl/versus-incident/pkg/aws/collect"
	"github.com/VersusControl/versus-incident/pkg/aws/limit"
	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/aws/settings"
	"github.com/VersusControl/versus-incident/pkg/aws/snapshot"
	"github.com/VersusControl/versus-incident/pkg/awsauth"
	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

const (
	// endpointEnv and caBundleEnv are the test-build redirect the server
	// binary reads (pkg/aws/harnessendpoint/endpoint.go); the tests read the same two.
	endpointEnv = "VERSUS_HARNESS_AWS_ENDPOINT_URL"
	caBundleEnv = "VERSUS_HARNESS_AWS_CA_BUNDLE"

	// emulatorAccount is the one account the emulator answers for.
	emulatorAccount = "000000000000"
	testRegion      = "us-east-1"

	// fleetInstances is what seed.sh launches beyond the two tagged
	// instances with its default SEED_SCALE_BATCHES (16 calls of 64). More
	// than 1000 makes the collector's DescribeInstances and
	// DescribeNetworkInterfaces calls (1000 per page) follow a NextToken.
	fleetInstances = 16 * 64

	redacted = model.RedactedValue
)

// requireEmulator skips the test unless the redirect names an emulator.
func requireEmulator(t *testing.T) {
	t.Helper()
	if os.Getenv(endpointEnv) == "" {
		t.Skipf("%s is not set: these contract tests run only against a seeded AWS Emulator (the aws-emulator CI job)", endpointEnv)
	}
}

// seededWorld is what one account check and one full collector sync stored,
// shared by every test: the emulator is seeded once and read once.
type seededWorld struct {
	health      settings.HealthRecord
	allowlist   []string
	identityARN string
	identityID  string
	report      collect.Report
	resources   []model.Resource
	gaps        []model.Gap
	changes     snapshot.ChangesResult
	syncTime    time.Duration
}

var (
	worldOnce sync.Once
	world     *seededWorld
	worldErr  error
)

// seeded returns the shared world, building it on first use.
func seeded(t *testing.T) *seededWorld {
	t.Helper()
	requireEmulator(t)
	worldOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		world, worldErr = readWorld(ctx)
	})
	if worldErr != nil {
		t.Fatalf("reading the seeded emulator through Versus: %v", worldErr)
	}
	return world
}

// readWorld wires the AWS connector the way cmd/main.go does for one account
// listed in connectors.yaml with auth.mode default_chain (the quickstart
// path): the settings service checks the account and probes its services,
// then the collector reads every unit through the same transport.
func readWorld(ctx context.Context) (*seededWorld, error) {
	// Versus refuses an ambient AWS_ENDPOINT_URL (an override it never
	// honours), and the CI job may export one for the AWS CLI seed step.
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if name == "AWS_ENDPOINT_URL" || strings.HasPrefix(name, "AWS_ENDPOINT_URL_") {
			os.Unsetenv(name)
		}
	}
	client, err := emulatorClient(os.Getenv(endpointEnv), os.Getenv(caBundleEnv))
	if err != nil {
		return nil, err
	}

	provider := storage.NewMemory()
	scope := tenancy.DefaultOrgScope()
	limiter := limit.NewManager(1000)
	connector := config.AWSConnectorConfig{
		Enable: true,
		Accounts: []config.AWSAccountConfig{{
			AccountID: emulatorAccount,
			Regions:   []string{testRegion},
			Auth:      config.AWSAuthConfig{Mode: "default_chain"},
		}},
		Collect: config.AWSCollectConfig{Enable: true},
	}
	var service *settings.Service
	snapshots := snapshot.New(provider, scope, snapshot.WithTagAllowlist(func() []string { return service.TagValueAllowlist() }))
	service = settings.NewService(settings.NewStore(provider, scope),
		settings.WithConfig(func() config.AWSConnectorConfig { return connector }),
		settings.WithLimiter(limiter),
		settings.WithSnapshots(snapshots),
		settings.WithHTTPClient(client),
	)

	out := &seededWorld{allowlist: service.TagValueAllowlist()}
	accounts := settings.MergeAccounts(nil, connector.Accounts)
	if len(accounts) != 1 {
		return nil, fmt.Errorf("connector config yields %d accounts, want 1", len(accounts))
	}
	if out.health, err = service.CheckAccount(ctx, accounts[0]); err != nil {
		return nil, fmt.Errorf("account check: %w", err)
	}
	if out.health.Status != settings.StatusConnected {
		return nil, fmt.Errorf("account check: status %q, error %q; want connected", out.health.Status, out.health.ErrorCode)
	}
	targets := service.CollectTargets(ctx)
	if len(targets) != 1 || targets[0].Credentials == nil {
		return nil, fmt.Errorf("collect targets: got %d (want one with credentials)", len(targets))
	}
	target := targets[0]
	if out.identityARN, out.identityID, err = callerIdentity(ctx, target.Credentials, client); err != nil {
		return nil, fmt.Errorf("caller identity: %w", err)
	}

	collector := collect.New(snapshots, limiter,
		collect.WithHTTPClient(client),
		collect.WithOptions(func() collect.Options {
			return collect.Options{TagAllowlist: service.TagValueAllowlist()}
		}),
	)
	started := time.Now()
	out.report, err = collector.Sync(ctx, collect.Target{AccountID: target.AccountID, Regions: target.Regions, Credentials: target.Credentials})
	out.syncTime = time.Since(started)
	if err != nil {
		return nil, fmt.Errorf("collector sync: %w", err)
	}
	read, err := snapshots.Resources(ctx, snapshot.Query{Account: emulatorAccount})
	if err != nil {
		return nil, fmt.Errorf("reading resources: %w", err)
	}
	out.resources, out.gaps = read.Resources, read.Gaps
	now := time.Now().UTC()
	if out.changes, err = snapshots.Changes(ctx, snapshot.Query{Account: emulatorAccount}, now.Add(-collect.DefaultChangeLookback), now.Add(time.Minute)); err != nil {
		return nil, fmt.Errorf("reading changes: %w", err)
	}
	return out, nil
}

// emulatorClient is the same redirect the test build of the server installs
// (pkg/aws/harnessendpoint/endpoint.go): a request signed for the real
// regional AWS host keeps its Host header and signature and is sent to the
// emulator instead. It accepts only a
// loopback or private endpoint and dials nothing else, so no request can
// reach AWS.
func emulatorClient(rawURL, caBundle string) (aws.HTTPClient, error) {
	base, err := url.Parse(rawURL)
	if err != nil || (base.Scheme != "https" && base.Scheme != "http") || base.Host == "" ||
		(base.Path != "" && base.Path != "/") || base.User != nil || base.RawQuery != "" {
		return nil, fmt.Errorf("%s must be a bare http(s)://host:port origin", endpointEnv)
	}
	if !localHost(base.Hostname()) {
		return nil, fmt.Errorf("%s must name a loopback or private host", endpointEnv)
	}
	var roots *x509.CertPool
	if caBundle != "" {
		pem, err := os.ReadFile(caBundle)
		if err != nil {
			return nil, fmt.Errorf("%s unreadable: %w", caBundleEnv, err)
		}
		roots = x509.NewCertPool()
		if !roots.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("%s holds no PEM certificate", caBundleEnv)
		}
	} else if base.Scheme == "https" {
		return nil, fmt.Errorf("%s is required for an https emulator (it serves a self-signed CA)", caBundleEnv)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}
	dialer := &net.Dialer{Timeout: 10 * time.Second, Control: localDialOnly}
	transport.DialContext = dialer.DialContext
	return &http.Client{
		Transport: redirect{base: &url.URL{Scheme: base.Scheme, Host: base.Host}, next: transport},
		Timeout:   60 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}, nil
}

// callerIdentity runs the identity call the settings page shows, through the
// same transport and the collector target's credentials.
func callerIdentity(ctx context.Context, credentials aws.CredentialsProvider, client aws.HTTPClient) (string, string, error) {
	return awsauth.CallerIdentity(ctx, aws.Config{Region: testRegion, Credentials: credentials, HTTPClient: client})
}

func localHost(host string) bool {
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback() || ip.IsPrivate()
	}
	host = strings.ToLower(host)
	return host == "localhost" || !strings.Contains(host, ".")
}

func localDialOnly(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	if ip := net.ParseIP(host); ip == nil || !(ip.IsLoopback() || ip.IsPrivate()) {
		return errors.New("refusing to dial a non-local address")
	}
	return nil
}

type redirect struct {
	base *url.URL
	next http.RoundTripper
}

func (r redirect) RoundTrip(request *http.Request) (*http.Response, error) {
	clone := request.Clone(request.Context())
	target := *request.URL
	target.Scheme, target.Host = r.base.Scheme, r.base.Host
	clone.URL = &target
	if clone.Host == "" {
		clone.Host = request.URL.Host
	}
	return r.next.RoundTrip(clone)
}

// --- read helpers -----------------------------------------------------------

// ofType returns the stored resources of one service and type.
func (w *seededWorld) ofType(service, typ string) []model.Resource {
	var out []model.Resource
	for _, resource := range w.resources {
		if resource.Service == service && resource.Type == typ {
			out = append(out, resource)
		}
	}
	return out
}

// named returns the one resource of a service and type with the given name.
func (w *seededWorld) named(t *testing.T, service, typ, name string) model.Resource {
	t.Helper()
	var found []model.Resource
	for _, resource := range w.ofType(service, typ) {
		if resource.Name == name {
			found = append(found, resource)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%s %s named %q: found %d, want 1 (stored %s)", service, typ, name, len(found), names(w.ofType(service, typ)))
	}
	return found[0]
}

// tagged returns the one resource of a service and type whose tag key holds
// value (an allowlisted key, so the value is stored).
func (w *seededWorld) tagged(t *testing.T, service, typ, key, value string) model.Resource {
	t.Helper()
	var found []model.Resource
	for _, resource := range w.ofType(service, typ) {
		if resource.Tags[key] == value {
			found = append(found, resource)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%s %s tagged %s=%s: found %d, want 1", service, typ, key, value, len(found))
	}
	return found[0]
}

// byARN finds a stored resource by ARN.
func (w *seededWorld) byARN(arn string) (model.Resource, bool) {
	for _, resource := range w.resources {
		if resource.ARN == arn {
			return resource, true
		}
	}
	return model.Resource{}, false
}

// unit returns the sync report of one unit service in the test region (or
// the global region for a global service).
func (w *seededWorld) unit(t *testing.T, service string) collect.UnitReport {
	t.Helper()
	for _, unit := range w.report.Units {
		if unit.Unit.Service == service {
			return unit
		}
	}
	t.Fatalf("no %s unit in the sync report", service)
	return collect.UnitReport{}
}

// requireCommitted fails unless every named unit committed a generation.
func (w *seededWorld) requireCommitted(t *testing.T, services ...string) {
	t.Helper()
	for _, service := range services {
		if unit := w.unit(t, service); unit.Outcome != collect.OutcomeCommitted {
			t.Errorf("%s unit: outcome %q, error %q; want committed", service, unit.Outcome, unit.ErrorCode)
		}
	}
}

// requireNoGaps fails when a unit recorded a detail gap: a detail call the
// collector could not read is a contract break even when the list call
// committed.
func (w *seededWorld) requireNoGaps(t *testing.T, services ...string) {
	t.Helper()
	for _, gap := range w.gaps {
		for _, service := range services {
			if gap.Service == service {
				t.Errorf("%s unit recorded a gap: %+v", service, gap)
			}
		}
	}
}

func names(resources []model.Resource) string {
	out := make([]string, 0, len(resources))
	for _, resource := range resources {
		out = append(out, resource.Name)
	}
	return "[" + strings.Join(out, ", ") + "]"
}

func hasRelation(resource model.Resource, kind model.RelationType, target string) bool {
	for _, relation := range resource.Relations {
		if relation.Type == kind && relation.Target == target {
			return true
		}
	}
	return false
}

// relationTargets lists the targets of one relation kind.
func relationTargets(resource model.Resource, kind model.RelationType) []string {
	var out []string
	for _, relation := range resource.Relations {
		if relation.Type == kind {
			out = append(out, relation.Target)
		}
	}
	return out
}

// requireTags checks that every key in want is stored with that value. Use
// redacted for a key outside the allowlist.
func requireTags(t *testing.T, resource model.Resource, want map[string]string) {
	t.Helper()
	for key, value := range want {
		got, ok := resource.Tags[key]
		if !ok {
			t.Errorf("%s: tag %q missing (tags %v)", resource.ARN, key, resource.Tags)
			continue
		}
		if got != value {
			t.Errorf("%s: tag %q = %q, want %q", resource.ARN, key, got, value)
		}
	}
}

// requireRelationResolves fails unless every target of kind on resource is a
// stored resource: the IDs one collector reports must match the IDs another
// collector stored.
func (w *seededWorld) requireRelationResolves(t *testing.T, resource model.Resource, kind model.RelationType) {
	t.Helper()
	targets := relationTargets(resource, kind)
	if len(targets) == 0 {
		t.Errorf("%s: no %s relation", resource.ARN, kind)
	}
	for _, target := range targets {
		if _, ok := w.byARN(target); !ok {
			t.Errorf("%s: %s target %s is not a stored resource", resource.ARN, kind, target)
		}
	}
}
