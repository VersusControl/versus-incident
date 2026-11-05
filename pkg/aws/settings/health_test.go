package settings

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"

	"github.com/VersusControl/versus-incident/pkg/aws/limit"
	"github.com/VersusControl/versus-incident/pkg/aws/probe"
	"github.com/VersusControl/versus-incident/pkg/awsauth"
	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
	"github.com/VersusControl/versus-incident/tests/fakeaws"
)

func staticBase(string) (aws.CredentialsProvider, error) {
	return aws.NewCredentialsCache(credentials.NewStaticCredentialsProvider("AKIAIOSFODNN7EXAMPLE", "secret", "")), nil
}

func enabledConfig() func() config.AWSConnectorConfig {
	return func() config.AWSConnectorConfig {
		return config.AWSConnectorConfig{
			Enable:              true,
			HealthCheckInterval: "5m",
			Budgets:             config.AWSBudgetsConfig{MaxRequestsPerSecond: 1000},
		}
	}
}

func TestHealthTransitionsAfterTwoConsecutiveFailures(t *testing.T) {
	store := testStore(t)
	now := time.Date(2026, 11, 3, 13, 0, 0, 0, time.UTC)
	store.setClock(func() time.Time { return now })

	failing := true
	connector := NewService(store,
		WithConfig(enabledConfig()),
		WithClock(func() time.Time { return now }),
		WithBaseProvider(staticBase),
		WithAssumeRole(func(context.Context, aws.CredentialsProvider, string, string, string) (aws.CredentialsProvider, error) {
			return aws.NewCredentialsCache(credentials.NewStaticCredentialsProvider("AKIAIOSFODNN7EXAMPLE", "secret", "")), nil
		}),
		WithIdentity(func(context.Context, aws.CredentialsProvider, string) (string, string, error) {
			if failing {
				return "", "", awsauth.ErrTrustDenied
			}
			return "arn:aws:iam::444455556666:role/versus-reader", "444455556666", nil
		}),
	)
	account := AccountRecord{AccountID: "444455556666", RoleARN: "arn:aws:iam::444455556666:role/versus-reader"}

	first, err := connector.CheckAccount(context.Background(), account)
	if err != nil {
		t.Fatal(err)
	}
	if first.Status == StatusDisconnected {
		t.Fatal("one failure marked the account disconnected")
	}
	firstAttempt := first.AttemptedAt
	if first.AttemptOK {
		t.Fatal("failed attempt recorded as a success")
	}

	now = now.Add(time.Minute)
	second, err := connector.CheckAccount(context.Background(), account)
	if err != nil {
		t.Fatal(err)
	}
	if second.Status != StatusDisconnected {
		t.Fatalf("second failure status = %q", second.Status)
	}
	if second.ErrorCode != awsauth.ErrTrustDenied.Error() {
		t.Fatalf("error code = %q", second.ErrorCode)
	}
	if !second.DisconnectedSince.Equal(firstAttempt) {
		t.Fatalf("disconnected since %v, want first failure %v", second.DisconnectedSince, firstAttempt)
	}
	if second.CheckedAt.After(time.Time{}) {
		t.Fatal("a failed attempt advanced the last success time")
	}

	failing = false
	now = now.Add(time.Minute)
	// A success within the probe window must not touch the network.
	seed := second
	seed.ProbedAt = now
	seed.ProbeResults = []probe.Result{{Service: "ec2", Status: probe.StatusReadable, Items: 1}}
	if err := store.SaveHealth("default", seed); err != nil {
		t.Fatal(err)
	}
	third, err := connector.CheckAccount(context.Background(), account)
	if err != nil {
		t.Fatal(err)
	}
	if third.Status != StatusConnected || !third.AttemptOK {
		t.Fatalf("recovered status = %q attempt_ok = %v", third.Status, third.AttemptOK)
	}
	if !third.DisconnectedSince.IsZero() || third.ErrorCode != "" {
		t.Fatalf("recovered record kept %v / %q", third.DisconnectedSince, third.ErrorCode)
	}
	if third.ConsecutiveFailures != 0 {
		t.Fatalf("consecutive failures = %d", third.ConsecutiveFailures)
	}
	if !third.CheckedAt.Equal(now) {
		t.Fatalf("checked at %v, want %v", third.CheckedAt, now)
	}
}

func TestHealthReprobesEverySixHours(t *testing.T) {
	fake := fakeaws.New(t, "new-account-connect")
	store := testStore(t)
	now := time.Date(2026, 11, 3, 13, 0, 0, 0, time.UTC)
	store.setClock(func() time.Time { return now })
	connector := NewService(store,
		WithConfig(enabledConfig()),
		WithClock(func() time.Time { return now }),
		WithHTTPClient(fake.HTTPClient()),
		WithBaseProvider(staticBase),
		WithLimiter(limit.NewManager(1000)),
	)
	account := AccountRecord{AccountID: "111122223333", RoleARN: "arn:aws:iam::111122223333:role/versus-reader", ExternalID: "vrs-ext-abcdefghijklmnopqrstuvwx", Regions: []string{"us-east-1"}}

	if _, err := connector.CheckAccount(context.Background(), account); err != nil {
		t.Fatal(err)
	}
	first := fake.OperationCount("ec2:DescribeInstances")
	if first == 0 {
		t.Fatal("first health success did not probe services")
	}
	now = now.Add(5 * time.Minute)
	if _, err := connector.CheckAccount(context.Background(), account); err != nil {
		t.Fatal(err)
	}
	if got := fake.OperationCount("ec2:DescribeInstances"); got != first {
		t.Fatalf("probed again inside six hours: %d -> %d", first, got)
	}
	now = now.Add(ServiceProbeInterval)
	if _, err := connector.CheckAccount(context.Background(), account); err != nil {
		t.Fatal(err)
	}
	if got := fake.OperationCount("ec2:DescribeInstances"); got <= first {
		t.Fatalf("six-hour re-probe did not run: %d -> %d", first, got)
	}
}

func TestHealthLoopStartsOnlyWhenEnabledAndStopsOnCancel(t *testing.T) {
	disabled := NewService(NewStore(storage.NewMemory(), tenancy.DefaultOrgScope()), WithConfig(func() config.AWSConnectorConfig {
		return config.AWSConnectorConfig{Enable: false}
	}))
	disabled.Start(context.Background())
	select {
	case <-disabled.Stopped():
	case <-time.After(time.Second):
		t.Fatal("disabled connector started a loop")
	}

	fake := fakeaws.New(t, "new-account-connect")
	enabled := NewService(NewStore(storage.NewMemory(), tenancy.DefaultOrgScope()),
		WithConfig(enabledConfig()),
		WithHTTPClient(fake.HTTPClient()),
		WithBaseProvider(staticBase),
		WithLimiter(limit.NewManager(1000)),
	)
	ctx, cancel := context.WithCancel(context.Background())
	enabled.Start(ctx)
	cancel()
	select {
	case <-enabled.Stopped():
	case <-time.After(5 * time.Second):
		t.Fatal("health loop did not stop on cancellation")
	}
}

// toggledDenial answers AccessDenied for one service host while enabled and
// otherwise delegates to the fixture. It models "denied, then the operator
// fixed the policy" without restarting the deployment.
type toggledDenial struct {
	base     aws.HTTPClient
	hostBase string

	mu   sync.Mutex
	deny bool
}

func (client *toggledDenial) setDeny(value bool) {
	client.mu.Lock()
	client.deny = value
	client.mu.Unlock()
}

func (client *toggledDenial) Do(request *http.Request) (*http.Response, error) {
	client.mu.Lock()
	deny := client.deny
	client.mu.Unlock()
	if deny && request.URL != nil && strings.HasPrefix(request.URL.Hostname(), client.hostBase) {
		body := `<ErrorResponse><Error><Type>Sender</Type><Code>AccessDenied</Code>` +
			`<Message>denied</Message></Error><RequestId>req</RequestId></ErrorResponse>`
		return &http.Response{
			StatusCode: http.StatusForbidden,
			Header:     http.Header{"Content-Type": []string{"text/xml"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    request,
		}, nil
	}
	return client.base.Do(request)
}

func TestRecheckForcesServiceProbe(t *testing.T) {
	fake := fakeaws.New(t, "new-account-connect")
	denial := &toggledDenial{base: fake.HTTPClient(), hostBase: "elasticache.", deny: true}
	store := testStore(t)
	now := time.Date(2026, 11, 3, 15, 0, 0, 0, time.UTC)
	store.setClock(func() time.Time { return now })
	connector := NewService(store,
		WithConfig(enabledConfig()),
		WithClock(func() time.Time { return now }),
		WithHTTPClient(denial),
		WithBaseProvider(staticBase),
		WithLimiter(limit.NewManager(1000)),
	)
	account := AccountRecord{
		AccountID:  "111122223333",
		RoleARN:    "arn:aws:iam::111122223333:role/versus-reader",
		ExternalID: "vrs-ext-abcdefghijklmnopqrstuvwx",
		Regions:    []string{"us-east-1"},
	}
	if err := store.SaveAccount("default", account); err != nil {
		t.Fatal(err)
	}

	first, err := connector.CheckAccount(context.Background(), account)
	if err != nil {
		t.Fatal(err)
	}
	if !containsValue(CountResults(first.ProbeResults).Denied, "elasticache") {
		t.Fatalf("first check did not see the denial: %+v", CountResults(first.ProbeResults))
	}
	probes := fake.OperationCount("ec2:DescribeInstances")

	now = now.Add(5 * time.Minute)
	denial.setDeny(false)
	view, err := connector.Recheck(context.Background(), "default", account.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	if containsValue(view.Services.Denied, "elasticache") {
		t.Fatalf("recheck kept the fixed denial: %+v", view.Services)
	}
	if got := fake.OperationCount("ec2:DescribeInstances"); got <= probes {
		t.Fatalf("recheck did not re-probe: %d -> %d", probes, got)
	}

	// The scheduled path still honours the six-hour window.
	probes = fake.OperationCount("ec2:DescribeInstances")
	now = now.Add(5 * time.Minute)
	if _, err := connector.CheckAccount(context.Background(), account); err != nil {
		t.Fatal(err)
	}
	if got := fake.OperationCount("ec2:DescribeInstances"); got != probes {
		t.Fatalf("scheduled check ignored the six-hour window: %d -> %d", probes, got)
	}
}

func TestConfigAccountWithAutoRegionsDiscoversOnFirstCheck(t *testing.T) {
	fake := fakeaws.New(t, "new-account-connect")
	store := testStore(t)
	now := time.Date(2026, 11, 3, 16, 0, 0, 0, time.UTC)
	store.setClock(func() time.Time { return now })
	configured := config.AWSConnectorConfig{
		Enable:              true,
		HealthCheckInterval: "5m",
		Budgets:             config.AWSBudgetsConfig{MaxRequestsPerSecond: 2},
		Accounts: []config.AWSAccountConfig{{
			AccountID:   "111122223333",
			DisplayName: "Production",
			Auth: config.AWSAuthConfig{
				RoleARN:    "arn:aws:iam::111122223333:role/versus-reader",
				ExternalID: "vrs-ext-abcdefghijklmnopqrstuvwx",
			},
		}},
	}
	connector := NewService(store,
		WithConfig(func() config.AWSConnectorConfig { return configured }),
		WithClock(func() time.Time { return now }),
		WithHTTPClient(fake.HTTPClient()),
		WithBaseProvider(staticBase),
		WithLimiter(limit.NewManager(1000)),
	)
	account := MergeAccounts(nil, configured.Accounts)[0]
	health, err := connector.CheckAccount(context.Background(), account)
	if err != nil {
		t.Fatal(err)
	}
	if len(health.Regions) == 0 {
		t.Fatalf("auto regions were not discovered: %+v", health)
	}
	view, err := connector.View(context.Background(), "default", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Accounts) != 1 {
		t.Fatalf("accounts = %+v", view.Accounts)
	}
	row := view.Accounts[0]
	if row.Regions.Enabled == 0 || len(row.Regions.Selected) == 0 {
		t.Fatalf("auto regions rendered as 0 of 0: %+v", row.Regions)
	}
	if want := 2 * len(row.Regions.Selected); row.APIUse.BudgetRPS != want {
		t.Fatalf("budget = %d, want %d", row.APIUse.BudgetRPS, want)
	}
}
