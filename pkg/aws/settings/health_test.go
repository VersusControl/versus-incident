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
	view, err := connector.Connection(context.Background(), Principal{Org: "default", CanWrite: false})
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

// Credentials that sign in to another AWS account than the configured one
// (a wrong account ID next to a working key) disconnect the account at once
// with account_mismatch. The record stays under the configured ID, nothing
// is probed, and neither the collectors nor cost read through those
// credentials, so the other account's data is never filed under this one.
func TestConfigAccountWithCredentialsOfAnotherAccountIsDisconnected(t *testing.T) {
	store := testStore(t)
	now := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	store.setClock(func() time.Time { return now })
	configured := config.AWSAccountConfig{AccountID: "111122223333", Regions: []string{"us-east-1"}, Auth: config.AWSAuthConfig{Mode: "default_chain"}}
	probed := false
	connector := NewService(store,
		WithConfig(collectingConfig(true, configured)),
		WithClock(func() time.Time { return now }),
		WithBaseProvider(staticBase),
		WithIdentity(func(context.Context, aws.CredentialsProvider, string) (string, string, error) {
			return "arn:aws:iam::444455556666:user/versus-quickstart", "444455556666", nil
		}),
		WithHTTPClient(probeRecorder{called: &probed}),
	)

	view, err := connector.Recheck(context.Background(), "default", "111122223333")
	if err != nil {
		t.Fatal(err)
	}
	if view.Status != StatusDisconnected || view.ErrorCode == nil || *view.ErrorCode != "account_mismatch" {
		t.Fatalf("view status = %q, error code = %v", view.Status, view.ErrorCode)
	}
	if probed {
		t.Fatal("services were probed through another account's credentials")
	}
	record, err := store.GetHealth("default", "111122223333")
	if err != nil || record.AccountID != "111122223333" || record.ErrorCode != "account_mismatch" || len(record.ProbeResults) != 0 {
		t.Fatalf("health record = %+v, err = %v", record, err)
	}
	if other, err := store.GetHealth("default", "444455556666"); err != nil || other.Status != StatusUnknown || !other.AttemptedAt.IsZero() {
		t.Fatal("health was filed under the credentials' account")
	}
	if targets := connector.CollectTargets(context.Background()); len(targets) != 0 {
		t.Fatalf("collect targets = %+v", targets)
	}
	targets := connector.CostTargets(context.Background())
	if len(targets) != 1 || targets[0].Cause != CauseAccountMismatch || targets[0].Credentials != nil || !targets[0].Since.Equal(now) {
		t.Fatalf("cost targets = %+v", targets)
	}
}

// probeRecorder notes that a service probe reached the network.
type probeRecorder struct{ called *bool }

func (recorder probeRecorder) Do(*http.Request) (*http.Response, error) {
	*recorder.called = true
	return &http.Response{StatusCode: http.StatusInternalServerError, Body: io.NopCloser(strings.NewReader(""))}, nil
}

// healthLoopOnFakeClock runs the health loop for passes passes with every
// wait taken on a fake clock, and returns each wait and each status seen.
func healthLoopOnFakeClock(t *testing.T, passes int, failUntil int) (waits []time.Duration, statuses []string, disconnectedAt time.Time, start time.Time) {
	t.Helper()
	start = time.Date(2026, 11, 19, 3, 38, 0, 0, time.UTC)
	now := start
	store := testStore(t)
	store.setClock(func() time.Time { return now })
	fake := fakeaws.New(t, "new-account-connect")
	attempts := 0
	connector := NewService(store,
		WithConfig(func() config.AWSConnectorConfig {
			return config.AWSConnectorConfig{Enable: true, HealthCheckInterval: "5m", Budgets: config.AWSBudgetsConfig{MaxRequestsPerSecond: 1000},
				Accounts: []config.AWSAccountConfig{{AccountID: "444455556666", Regions: []string{"us-east-1"},
					Auth: config.AWSAuthConfig{Mode: "assume_role", RoleARN: "arn:aws:iam::444455556666:role/versus-reader"}}}}
		}),
		WithClock(func() time.Time { return now }),
		WithHTTPClient(fake.HTTPClient()),
		WithLimiter(limit.NewManager(1000)),
		WithBaseProvider(staticBase),
		WithAssumeRole(func(context.Context, aws.CredentialsProvider, string, string, string) (aws.CredentialsProvider, error) {
			return aws.NewCredentialsCache(credentials.NewStaticCredentialsProvider("AKIAIOSFODNN7EXAMPLE", "secret", "session-444455556666")), nil
		}),
		WithIdentity(func(context.Context, aws.CredentialsProvider, string) (string, string, error) {
			attempts++
			if failUntil < 0 || attempts <= failUntil {
				// The endpoint is not answering, as when a check runs before
				// the AWS endpoint is up.
				return "", "", awsauth.ErrNetwork
			}
			return "arn:aws:iam::444455556666:role/versus-reader", "444455556666", nil
		}),
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	connector.after = func(wait time.Duration) <-chan time.Time {
		record, err := store.GetHealth("default", "444455556666")
		if err != nil {
			t.Fatal(err)
		}
		statuses = append(statuses, record.Status)
		if record.Status == StatusDisconnected && disconnectedAt.IsZero() {
			disconnectedAt = now
		}
		waits = append(waits, wait)
		if len(waits) == passes {
			cancel()
			return make(chan time.Time)
		}
		now = now.Add(wait)
		fired := make(chan time.Time, 1)
		fired <- now
		return fired
	}
	connector.healthLoop(ctx)
	return waits, statuses, disconnectedAt, start
}

// TestFailedHealthCheckRetriesSoonWithBackoff: a check that fails once at
// start-up (endpoint not ready yet) is retried after about 30 seconds and
// connects, instead of showing "checking" for a whole interval. A real
// outage reaches disconnected within about a minute, then backs off to the
// interval and stays disconnected without flapping.
func TestFailedHealthCheckRetriesSoonWithBackoff(t *testing.T) {
	near := func(got, want time.Duration) bool { return got >= want-want/10 && got <= want+want/10 }

	waits, statuses, _, _ := healthLoopOnFakeClock(t, 3, 1)
	if statuses[0] != StatusChecking || !near(waits[0], 30*time.Second) {
		t.Fatalf("first failure: status %q, next check in %s", statuses[0], waits[0])
	}
	if statuses[1] != StatusConnected || !near(waits[1], 5*time.Minute) {
		t.Fatalf("after the retry: status %q, next check in %s", statuses[1], waits[1])
	}

	waits, statuses, disconnectedAt, start := healthLoopOnFakeClock(t, 8, -1)
	if disconnectedAt.IsZero() || disconnectedAt.Sub(start) > time.Minute {
		t.Fatalf("outage disconnected at %v (start %v); statuses %v", disconnectedAt, start, statuses)
	}
	for index, want := range []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 5 * time.Minute, 5 * time.Minute} {
		if !near(waits[index], want) {
			t.Fatalf("wait %d = %s, want about %s (all %v)", index, waits[index], want, waits)
		}
	}
	for _, status := range statuses[1:] {
		if status != StatusDisconnected {
			t.Fatalf("steady outage flapped: %v", statuses)
		}
	}
}
