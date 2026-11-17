package collect

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/awsauth"
	"github.com/VersusControl/versus-incident/tests/fakeaws"
)

// When the server's credentials cannot be loaded at all (no identity on the
// host, a metadata endpoint that times out, a failed role assumption), the
// unit records a credentials cause, never the provider's transport symptom
// as timeout or network. A unit whose own deadline ends while credentials
// load still reads as a timeout.
func TestCredentialsThatCannotLoadAreACredentialsCause(t *testing.T) {
	h := newHarness(t, fakeaws.ScenarioBadDeploy)
	writer := h.store.Writer(productionAccount)
	run := func(ctx context.Context, service string, failure error) string {
		t.Helper()
		for _, entry := range specs {
			if entry.service != service {
				continue
			}
			failing := aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) { return aws.Credentials{}, failure })
			unit := model.Unit{Account: productionAccount, Region: testRegion, Service: service}
			c := &call{env: env{account: productionAccount, region: testRegion, credentials: failing,
				httpClient: h.fake.HTTPClient(), limiter: h.limiter, sleep: noSleep},
				unit: unit, partition: "aws", endpointRegion: testRegion, now: time.Now(), writer: writer}
			return h.collector.runUnit(ctx, c, entry).ErrorCode
		}
		t.Fatalf("no collector for %s", service)
		return ""
	}
	want := awsauth.ErrNoServerIdentity.Error()
	for _, failure := range []error{context.DeadlineExceeded, errors.New("no EC2 instance role found"), awsauth.ErrNetwork} {
		for _, service := range []string{"ec2", "lambda"} {
			if got := run(context.Background(), service, failure); got != want {
				t.Errorf("%s with a provider failing %q recorded %q, want %q", service, failure, got, want)
			}
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := run(ctx, "ec2", context.Canceled); got == want {
		t.Errorf("a unit cancelled while credentials load recorded %q", got)
	}
}
