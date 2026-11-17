package collect

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/smithy-go"

	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/aws/snapshot"
	"github.com/VersusControl/versus-incident/pkg/awsauth"
	"github.com/VersusControl/versus-incident/tests/fakeaws"
)

// noRegionalHost answers like a resolver for a service with no endpoint in
// the region: the pinned host does not exist. Other hosts go through.
type noRegionalHost struct {
	next   aws.HTTPClient
	prefix string
}

func (client noRegionalHost) Do(request *http.Request) (*http.Response, error) {
	if strings.HasPrefix(request.URL.Host, client.prefix+".") {
		return nil, &url.Error{Op: request.Method, URL: request.URL.String(), Err: &net.OpError{Op: "dial", Net: "tcp",
			Err: &net.DNSError{Err: "no such host", Name: request.URL.Host, IsNotFound: true}}}
	}
	return client.next.Do(request)
}

func TestErrorCodeNamesAServiceMissingInTheRegion(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{&net.DNSError{Err: "no such host", Name: "servicecatalog-appregistry.ap-southeast-5.amazonaws.com", IsNotFound: true}, CodeNotInRegion},
		{&smithy.GenericAPIError{Code: "UnsupportedAPIEndpointException"}, CodeNotInRegion},
		{&smithy.GenericAPIError{Code: "UnsupportedOperation", Message: "The functionality you requested is not available in this region."}, CodeNotInRegion},
		// A resolver that cannot answer is the network, not a missing service.
		{&net.DNSError{Err: "server misbehaving", Name: "ec2.us-east-1.amazonaws.com", IsTemporary: true}, awsauth.ErrNetwork.Error()},
		{&smithy.GenericAPIError{Code: "UnsupportedOperation", Message: "instance type not supported"}, awsauth.ErrNetwork.Error()},
	}
	for _, c := range cases {
		if got := errorCode(c.err); got != c.want {
			t.Errorf("errorCode(%v) = %q, want %q", c.err, got, c.want)
		}
	}
	if transientCode(CodeNotInRegion) {
		t.Fatal("a service missing in a region is not a passing failure")
	}
}

// A service with no endpoint in a region (AppRegistry in a new region, say)
// is reported as not available there: the unit reads as done, not failed,
// with a gap naming why it holds nothing, so Settings does not show a
// failure for it.
func TestServiceMissingInARegionIsNotAvailableNotAFailure(t *testing.T) {
	h := newHarness(t, fakeaws.ScenarioBadDeploy)
	const region = "ap-southeast-5"
	var logged []string
	h.collector.logf = func(format string, args ...any) { logged = append(logged, format) }
	writer := h.store.Writer(productionAccount)
	for _, entry := range specs {
		if entry.service != snapshot.AppRegistryService {
			continue
		}
		unit := model.Unit{Account: productionAccount, Region: region, Service: entry.service}
		c := &call{env: env{account: productionAccount, region: region, credentials: testCredentials(productionAccount),
			httpClient: noRegionalHost{next: h.fake.HTTPClient(), prefix: regionalHosts["appregistry"]}, limiter: h.limiter, sleep: noSleep},
			unit: unit, partition: "aws", endpointRegion: region, now: time.Now(), writer: writer}
		report := h.collector.runUnit(context.Background(), c, entry)
		if report.ErrorCode != CodeNotInRegion || report.Outcome != OutcomeCommitted {
			t.Fatalf("report = %+v, want %q and committed", report, CodeNotInRegion)
		}
		statuses, err := h.store.Sync(context.Background(), snapshot.Query{Account: productionAccount, Region: region, Service: entry.service})
		if err != nil || len(statuses) != 1 {
			t.Fatalf("unit status %+v %v", statuses, err)
		}
		sync := statuses[0].Sync
		if sync.State != model.SyncOK || len(sync.Gaps) != 1 || sync.Gaps[0].Reason != model.GapNotInRegion {
			t.Fatalf("unit sync = %+v, want ok with a not-available gap", sync)
		}
		for _, line := range logged {
			if strings.Contains(line, "failed") {
				t.Fatalf("logged as a failure: %q", line)
			}
		}
		return
	}
	t.Fatal(errors.New("no AppRegistry collector"))
}

// A unit that already holds data from a region read there before, so its
// endpoint exists: a host that stops resolving (a DNS firewall, a private
// zone mistake) is a network failure, and the stored data keeps its age
// instead of looking freshly read.
func TestMissingHostForAUnitWithDataIsANetworkFailure(t *testing.T) {
	h := newHarness(t, fakeaws.ScenarioBadDeploy)
	const region = "ap-southeast-5"
	writer := h.store.Writer(productionAccount)
	for _, entry := range specs {
		if entry.service != snapshot.AppRegistryService {
			continue
		}
		unit := model.Unit{Account: productionAccount, Region: region, Service: entry.service}
		if _, err := writer.Commit(unit, snapshot.Candidate{Resources: []model.Resource{{
			ARN: "arn:aws:servicecatalog:ap-southeast-5:" + productionAccount + ":/applications/shop", Service: entry.service,
			Type: "application", Account: productionAccount, Region: region, Name: "shop"}}}); err != nil {
			t.Fatal(err)
		}
		c := &call{env: env{account: productionAccount, region: region, credentials: testCredentials(productionAccount),
			httpClient: noRegionalHost{next: h.fake.HTTPClient(), prefix: regionalHosts["appregistry"]}, limiter: h.limiter, sleep: noSleep},
			unit: unit, partition: "aws", endpointRegion: region, now: time.Now(), writer: writer}
		report := h.collector.runUnit(context.Background(), c, entry)
		if report.ErrorCode != awsauth.ErrNetwork.Error() || report.Outcome != OutcomeKept {
			t.Fatalf("report = %+v, want network and kept", report)
		}
		statuses, err := h.store.Sync(context.Background(), snapshot.Query{Account: productionAccount, Region: region, Service: entry.service})
		if err != nil || len(statuses) != 1 || statuses[0].Sync.State != model.SyncFailed {
			t.Fatalf("unit status %+v %v, want failed", statuses, err)
		}
		return
	}
	t.Fatal("no AppRegistry collector")
}

// The application comes from the first configured tag key that is both set
// and allowlisted: a key that is not allowlisted does not hide a later one
// that is.
func TestTagApplicationSkipsKeysThatAreNotAllowlisted(t *testing.T) {
	c := &call{appTagKeys: []string{"app", "service"}, allowlist: map[string]bool{"service": true}}
	var resource model.Resource
	c.applyTags(&resource, map[string]string{"app": "hidden", "service": "checkout"})
	if resource.App != "checkout" || resource.AppTag != "service" || resource.AppSource != snapshot.AppSourceTags {
		t.Fatalf("app = %q from %q (%q), want checkout from the allowlisted service tag", resource.App, resource.AppTag, resource.AppSource)
	}
	if resource.Tags["app"] != model.RedactedValue {
		t.Fatalf("the unlisted tag value was kept: %v", resource.Tags)
	}
}
