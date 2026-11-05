package settings

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/VersusControl/versus-incident/pkg/aws/limit"
	"github.com/VersusControl/versus-incident/pkg/awsauth"
	"github.com/VersusControl/versus-incident/tests/fakeaws"
)

func staticAssume(context.Context, aws.CredentialsProvider, string, string, string) (aws.CredentialsProvider, error) {
	return staticBase("")
}

func draftForAccount(t *testing.T) Draft {
	t.Helper()
	return Draft{
		ID:         strings.Repeat("c", 32),
		ExternalID: "vrs-ext-abcdefghijklmnopqrstuvwx",
		RoleARN:    "arn:aws:iam::111122223333:role/versus-reader",
		Status:     DraftStarted,
	}
}

func TestRegionDiscoveryIsMeteredAndHonoursBackoff(t *testing.T) {
	fake := fakeaws.New(t, "new-account-connect")
	store := testStore(t)
	limiter := limit.NewManager(100)
	connector := NewService(store,
		WithConfig(enabledConfig()),
		WithHTTPClient(fake.HTTPClient()),
		WithBaseProvider(staticBase),
		WithAssumeRole(staticAssume),
		WithLimiter(limiter),
	)
	draft := draftForAccount(t)
	if err := store.SaveDraft("default", draft); err != nil {
		t.Fatal(err)
	}

	regions, err := connector.DiscoverRegions(context.Background(), "default", draft.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(regions) != 4 {
		t.Fatalf("regions = %+v", regions)
	}
	if got := fake.OperationCount("ec2:DescribeRegions"); got != 1 {
		t.Fatalf("DescribeRegions calls = %d", got)
	}
	if usage := limiter.Usage("111122223333"); usage.UsedRPS <= 0 {
		t.Fatalf("discovery calls were not metered: %+v", usage)
	}

	// A paused account and region stops the next discovery before any call.
	limiter.Throttled("111122223333", "us-east-1")
	before := fake.OperationCount("ec2:DescribeRegions")
	if _, err := connector.DiscoverRegions(context.Background(), "default", draft.ID); !errors.Is(err, awsauth.ErrThrottled) {
		t.Fatalf("backoff error = %v, want throttled", err)
	}
	if got := fake.OperationCount("ec2:DescribeRegions"); got != before {
		t.Fatalf("discovery called AWS during backoff: %d -> %d", before, got)
	}
}

// failingTransport returns a closed-vocabulary error, which the SDK wraps in
// its operation error. The code must survive that wrapping.
type failingTransport struct{ err error }

func (client failingTransport) Do(*http.Request) (*http.Response, error) {
	return nil, client.err
}

func TestRegionDiscoveryKeepsTypedErrorCodes(t *testing.T) {
	store := testStore(t)
	connector := NewService(store,
		WithConfig(enabledConfig()),
		WithHTTPClient(failingTransport{err: awsauth.ErrRegionDisabled}),
		WithBaseProvider(staticBase),
		WithAssumeRole(staticAssume),
		WithLimiter(limit.NewManager(100)),
	)
	draft := draftForAccount(t)
	if err := store.SaveDraft("default", draft); err != nil {
		t.Fatal(err)
	}
	_, err := connector.DiscoverRegions(context.Background(), "default", draft.ID)
	if !errors.Is(err, awsauth.ErrRegionDisabled) {
		t.Fatalf("error = %v, want the region-disabled code", err)
	}
	if code := awsauth.ErrorCode(err); code != awsauth.ErrRegionDisabled.Error() {
		t.Fatalf("closed code = %q, want %q", code, awsauth.ErrRegionDisabled)
	}
}
