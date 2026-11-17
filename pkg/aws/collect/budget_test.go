package collect

import (
	"context"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/catalog"
	"github.com/VersusControl/versus-incident/pkg/aws/limit"
	"github.com/VersusControl/versus-incident/pkg/aws/probe"
	"github.com/VersusControl/versus-incident/tests/fakeaws"
)

// TestCollectorsStayWithinTwoRequestsPerSecond runs a large account against
// the default budget for a few seconds and counts what reached AWS. Global
// services share the us-east-1 limiter, so one region bounds every call.
func TestCollectorsStayWithinTwoRequestsPerSecond(t *testing.T) {
	if testing.Short() {
		t.Skip("timed budget run")
	}
	h := newHarness(t, fakeaws.ScenarioLoad5k)
	budget := limit.NewManager(2)
	collector := New(h.store, budget, WithHTTPClient(h.fake.HTTPClient()), WithSleep(noSleep))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	started := time.Now()
	_, _ = collector.Sync(ctx, target(productionAccount))
	elapsed := time.Since(started)
	requests := h.fake.TotalRequests()
	allowed := int64(2*elapsed.Seconds()) + limit.Burst + 1
	if requests == 0 || requests > allowed {
		t.Fatalf("%d requests in %s, budget allows %d", requests, elapsed, allowed)
	}
	if usage := budget.Usage(productionAccount); usage.UsedRPS <= 0 {
		t.Fatalf("collector calls did not reach the meter: %+v", usage)
	}
}

// TestProbesAndCollectorsShareOneLimiter proves both paths draw from the same
// bucket: the collector holds the manager it was given, and a probe and a
// sync both show on that manager's meter for the same account and region.
func TestProbesAndCollectorsShareOneLimiter(t *testing.T) {
	h := newHarness(t, fakeaws.ScenarioBadDeploy)
	if h.collector.Limiter() != h.limiter {
		t.Fatal("collector does not use the limiter it was given")
	}
	prober := probe.New(productionAccount, testCredentials(productionAccount), h.limiter, probe.WithHTTPClient(h.fake.HTTPClient()))
	entry, _ := catalog.ByID("ec2")
	prober.Probe(context.Background(), entry, testRegion)
	afterProbe := h.limiter.Usage(productionAccount).UsedRPS
	mustSync(t, h, target(productionAccount), KindHealth)
	afterSync := h.limiter.Usage(productionAccount).UsedRPS
	if afterProbe <= 0 || afterSync <= afterProbe {
		t.Fatalf("meter = %v after probe, %v after sync", afterProbe, afterSync)
	}
	// A throttling reply seen by the collector pauses the probe too.
	h.limiter.Throttled(productionAccount, testRegion)
	result := prober.Probe(context.Background(), entry, testRegion)
	if result.Status != probe.StatusNotChecked || result.ErrorCode != probe.CodeThrottled {
		t.Fatalf("probe during collector backoff = %+v", result)
	}
}

// TestEveryCallWaitsOnTheLimiter checks the meter grew by exactly the number
// of requests that reached AWS.
func TestEveryCallWaitsOnTheLimiter(t *testing.T) {
	h := newHarness(t, fakeaws.ScenarioBadDeploy)
	mustSync(t, h, target(productionAccount))
	requests := float64(h.fake.TotalRequests())
	used := h.limiter.Usage(productionAccount).UsedRPS * limit.Window.Seconds()
	if requests == 0 || used < requests-0.5 || used > requests+0.5 {
		t.Fatalf("meter counted %.0f grants for %.0f requests", used, requests)
	}
}
