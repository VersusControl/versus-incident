package settings

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"

	"github.com/VersusControl/versus-incident/pkg/aws/limit"
	"github.com/VersusControl/versus-incident/pkg/awsauth"
	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/tests/fakeaws"
)

// optInClient answers EC2 DescribeRegions itself, with af-south-1 opted in
// or not, and sends everything else to the fixture.
type optInClient struct {
	base     aws.HTTPClient
	optedIn  atomic.Bool
	refuse   atomic.Bool
	describe atomic.Int64
	hosts    sync.Map
}

func (client *optInClient) Do(request *http.Request) (*http.Response, error) {
	client.hosts.Store(request.URL.Host, true)
	var body []byte
	if request.Body != nil {
		body, _ = io.ReadAll(request.Body)
		request.Body = io.NopCloser(bytes.NewReader(body))
	}
	if !strings.Contains(string(body), "Action=DescribeRegions") {
		return client.base.Do(request)
	}
	client.describe.Add(1)
	if client.refuse.Load() {
		denied := `<Response><Errors><Error><Code>UnauthorizedOperation</Code><Message>not authorized</Message></Error></Errors><RequestID>r</RequestID></Response>`
		return &http.Response{StatusCode: http.StatusForbidden, Header: http.Header{"Content-Type": {"text/xml"}}, Body: io.NopCloser(strings.NewReader(denied)), Request: request}, nil
	}
	status := "not-opted-in"
	if client.optedIn.Load() {
		status = "opted-in"
	}
	inner := ""
	for _, region := range [][2]string{{"us-east-1", "opt-in-not-required"}, {"eu-west-1", "opt-in-not-required"}, {"af-south-1", status}} {
		inner += "<item><regionName>" + region[0] + "</regionName><optInStatus>" + region[1] + "</optInStatus></item>"
	}
	xml := `<DescribeRegionsResponse xmlns="http://ec2.amazonaws.com/doc/2016-11-15/"><requestId>r</requestId><regionInfo>` + inner + `</regionInfo></DescribeRegionsResponse>`
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/xml"}}, Body: io.NopCloser(strings.NewReader(xml)), Request: request}, nil
}

func optInService(t *testing.T, client *optInClient, accounts ...config.AWSAccountConfig) *Service {
	t.Helper()
	client.base = fakeaws.New(t, "new-account-connect").HTTPClient()
	return NewService(testStore(t), WithHTTPClient(client), WithBaseProvider(staticBase), WithLimiter(limit.NewManager(1000)),
		WithConfig(func() config.AWSConnectorConfig {
			connector := enabledConfig()()
			connector.Accounts = accounts
			return connector
		}))
}

func targetRegions(t *testing.T, service *Service, account string) []string {
	t.Helper()
	for _, target := range service.CollectTargets(context.Background()) {
		if target.AccountID == account {
			return target.Regions
		}
	}
	return nil
}

// TestOptedInLaterRegionIsCollectedAndNotEnabledIsNot: for an auto account
// and for one with a selection, a Region recorded as not enabled is not
// collected; once the account opts in, the next health check records it
// enabled and the collector polls it.
func TestOptedInLaterRegionIsCollectedAndNotEnabledIsNot(t *testing.T) {
	for name, regions := range map[string][]string{"auto": nil, "selected": {"af-south-1", "us-east-1"}} {
		t.Run(name, func(t *testing.T) {
			client := &optInClient{}
			account := config.AWSAccountConfig{AccountID: "444455556666", Regions: regions,
				Auth: config.AWSAuthConfig{Mode: "assume_role", RoleARN: "arn:aws:iam::444455556666:role/versus-reader", ExternalID: regionTestExternalID}}
			service := optInService(t, client, account)
			record := AccountRecord{AccountID: account.AccountID, RoleARN: account.Auth.RoleARN, ExternalID: regionTestExternalID, Regions: regions, Source: SourceConfig}
			if _, err := service.CheckAccount(context.Background(), record); err != nil {
				t.Fatal(err)
			}
			if got := targetRegions(t, service, account.AccountID); len(got) == 0 || slices.Contains(got, "af-south-1") {
				t.Fatalf("collector regions before opt-in = %v", got)
			}
			client.optedIn.Store(true)
			if _, err := service.CheckAccount(context.Background(), record); err != nil {
				t.Fatal(err)
			}
			if got := targetRegions(t, service, account.AccountID); !slices.Contains(got, "af-south-1") {
				t.Fatalf("collector regions after opt-in = %v", got)
			}
		})
	}
}

// TestDescribeRegionsIsSignedInTheHomeRegion: the opt-in read goes to the
// partition's home Region, not always to us-east-1.
func TestDescribeRegionsIsSignedInTheHomeRegion(t *testing.T) {
	client := &optInClient{}
	service := optInService(t, client)
	creds := aws.NewCredentialsCache(credentials.NewStaticCredentialsProvider("AKIAIOSFODNN7EXAMPLE", "secret", "session-444455556666"))
	_, _ = service.describeRegions(context.Background(), "444455556666", creds, homeRegion([]string{"cn-northwest-1"}))
	if _, ok := client.hosts.Load("ec2.cn-north-1.amazonaws.com.cn"); !ok {
		var hosts []string
		client.hosts.Range(func(key, _ any) bool { hosts = append(hosts, key.(string)); return true })
		t.Fatalf("DescribeRegions went to %v, want the China home Region", hosts)
	}
}

// TestFailureDuringAProbeIsNeverOverwrittenByConnected: a check that
// probes for a while and a second check that fails meanwhile run one
// after the other on the account's record, so the failure stays.
func TestFailureDuringAProbeIsNeverOverwrittenByConnected(t *testing.T) {
	gate := &gatedClient{release: make(chan struct{}), base: fakeaws.New(t, "new-account-connect").HTTPClient()}
	defer gate.open()
	calls := atomic.Int64{}
	service := NewService(testStore(t), WithConfig(enabledConfig()), WithHTTPClient(gate), WithBaseProvider(staticBase), WithLimiter(limit.NewManager(1000)),
		WithAssumeRole(func(context.Context, aws.CredentialsProvider, string, string, string) (aws.CredentialsProvider, error) {
			return aws.NewCredentialsCache(credentials.NewStaticCredentialsProvider("AKIAIOSFODNN7EXAMPLE", "secret", "session-444455556666")), nil
		}),
		WithIdentity(func(context.Context, aws.CredentialsProvider, string) (string, string, error) {
			if calls.Add(1) == 1 {
				return "arn:aws:iam::444455556666:role/versus-reader", "444455556666", nil
			}
			return "", "", awsauth.ErrTrustDenied
		}))
	account := regionAccount("444455556666", "us-east-1")
	first := make(chan error, 1)
	go func() { _, err := service.CheckAccount(context.Background(), account); first <- err }()
	eventually(t, "the first check to save its identity result", func() bool {
		record, err := service.store.GetHealth("default", account.AccountID)
		return err == nil && record.Status == StatusConnected
	})
	second := make(chan error, 1)
	go func() { _, err := service.CheckAccount(context.Background(), account); second <- err }()
	// The second check waits on the account's health lock while the first
	// probes; a check that did not wait would finish its failure here.
	eventually(t, "the second check to wait for the first", func() bool { return blockedIn(".(*Service).lockHealth") })
	gate.open()
	if err := errors.Join(<-first, <-second); err != nil {
		t.Fatal(err)
	}
	final, err := service.store.GetHealth("default", account.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	if final.AttemptOK || final.ErrorCode != awsauth.ErrTrustDenied.Error() || final.ConsecutiveFailures != 1 {
		t.Fatalf("failure overwritten: %+v", final)
	}
}

// TestRefusedOptInReadTurnsStatusUnknown: a Region recorded not enabled
// stays out of the collector while that read is fresh, but once the opt-in
// read has been refused for about two health intervals its status is
// unknown: it is no longer listed as not enabled and the collector polls
// it, so a later opt-in can never hide its changes.
func TestRefusedOptInReadTurnsStatusUnknown(t *testing.T) {
	now := time.Date(2026, 11, 19, 6, 0, 0, 0, time.UTC)
	client := &optInClient{}
	selected := []string{"af-south-1", "us-east-1"}
	account := config.AWSAccountConfig{AccountID: "444455556666", Regions: selected,
		Auth: config.AWSAuthConfig{Mode: "assume_role", RoleARN: "arn:aws:iam::444455556666:role/versus-reader", ExternalID: regionTestExternalID}}
	service := optInService(t, client, account)
	service.now = func() time.Time { return now }
	record := AccountRecord{AccountID: account.AccountID, RoleARN: account.Auth.RoleARN, ExternalID: regionTestExternalID, Regions: selected, Source: SourceConfig}
	check := func() HealthRecord {
		t.Helper()
		health, err := service.CheckAccount(context.Background(), record)
		if err != nil {
			t.Fatal(err)
		}
		return health
	}
	if health := check(); !slices.Equal(health.NotEnabledRegionsAt(selected, now), []string{"af-south-1"}) ||
		slices.Contains(targetRegions(t, service, account.AccountID), "af-south-1") {
		t.Fatalf("fresh read: not enabled %v, collector %v", health.NotEnabledRegionsAt(selected, now), targetRegions(t, service, account.AccountID))
	}
	client.refuse.Store(true)
	client.optedIn.Store(true)
	now = now.Add(6 * time.Minute)
	if health := check(); len(health.NotEnabledRegionsAt(selected, now)) != 1 || slices.Contains(targetRegions(t, service, account.AccountID), "af-south-1") {
		t.Fatal("one refused read within two intervals already dropped the confirmed status")
	}
	now = now.Add(5 * time.Minute)
	health := check()
	if got := health.NotEnabledRegionsAt(selected, now); len(got) != 0 {
		t.Fatalf("stale status still confirmed not enabled: %v", got)
	}
	if got := targetRegions(t, service, account.AccountID); !slices.Contains(got, "af-south-1") {
		t.Fatalf("collector regions with a stale status = %v", got)
	}
}

// TestSettingsShowsStaleOptInStatusAsUnknown: the Settings view lists a
// Region as not enabled only while the opt-in read is fresh; after that it
// is listed as status unknown with the time of the last successful read.
func TestSettingsShowsStaleOptInStatusAsUnknown(t *testing.T) {
	now := time.Date(2026, 11, 19, 6, 0, 0, 0, time.UTC)
	client := &optInClient{}
	selected := []string{"af-south-1", "us-east-1"}
	account := config.AWSAccountConfig{AccountID: "444455556666", Regions: selected,
		Auth: config.AWSAuthConfig{Mode: "assume_role", RoleARN: "arn:aws:iam::444455556666:role/versus-reader", ExternalID: regionTestExternalID}}
	service := optInService(t, client, account)
	service.now = func() time.Time { return now }
	record := AccountRecord{AccountID: account.AccountID, RoleARN: account.Auth.RoleARN, ExternalID: regionTestExternalID, Regions: selected, Source: SourceConfig}
	readAt := now
	if _, err := service.CheckAccount(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	view, err := service.accountView("default", record, false)
	if err != nil || !slices.Equal(view.Regions.NotEnabled, []string{"af-south-1"}) || len(view.Regions.StatusUnknown) != 0 || view.Regions.Enabled != 1 {
		t.Fatalf("fresh view = %+v %v", view.Regions, err)
	}
	client.refuse.Store(true)
	now = now.Add(11 * time.Minute)
	if _, err := service.CheckAccount(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	view, err = service.accountView("default", record, false)
	if err != nil || len(view.Regions.NotEnabled) != 0 || !slices.Equal(view.Regions.StatusUnknown, []string{"af-south-1"}) ||
		view.Regions.Enabled != 1 || view.Regions.StatusReadAt == nil || !view.Regions.StatusReadAt.Equal(readAt) {
		t.Fatalf("stale view = %+v %v", view.Regions, err)
	}
}
