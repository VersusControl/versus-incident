package collect

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"

	"github.com/VersusControl/versus-incident/pkg/aws/limit"
	"github.com/VersusControl/versus-incident/pkg/aws/snapshot"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
	"github.com/VersusControl/versus-incident/tests/fakeaws"
)

const (
	productionAccount = "111122223333"
	stagingAccount    = "444455556666"
	testRegion        = "us-east-1"
)

// testCredentials carry the account in the session token, which is how the
// fixture tells accounts apart.
func testCredentials(account string) aws.CredentialsProvider {
	return credentials.NewStaticCredentialsProvider("AKIDCOLLECTORTEST", "collector-test-secret", "session-"+account)
}

type harness struct {
	fake      *fakeaws.Server
	store     *snapshot.Store
	limiter   *limit.Manager
	collector *Collector
	provider  storage.Provider
}

func noSleep(context.Context, time.Duration) error { return nil }

func newHarness(t *testing.T, scenario string, options ...Option) harness {
	t.Helper()
	fake := fakeaws.New(t, scenario)
	provider := storage.NewMemory()
	store := snapshot.New(provider, tenancy.DefaultOrgScope())
	limiter := limit.NewManager(1000)
	base := []Option{WithHTTPClient(fake.HTTPClient()), WithSleep(noSleep), WithRetries(0),
		WithOptions(func() Options { return Options{TagAllowlist: []string{"Name", "app", "service", "team", "env"}} })}
	collector := New(store, limiter, append(base, options...)...)
	return harness{fake: fake, store: store, limiter: limiter, collector: collector, provider: provider}
}

func target(account string, regions ...string) Target {
	if len(regions) == 0 {
		regions = []string{testRegion}
	}
	return Target{AccountID: account, Regions: regions, Credentials: testCredentials(account)}
}

func mustSync(t *testing.T, h harness, tgt Target, kinds ...Kind) Report {
	t.Helper()
	report, err := h.collector.Sync(context.Background(), tgt, kinds...)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	return report
}
