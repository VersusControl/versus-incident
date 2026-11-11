package collect

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/aws/snapshot"
	"github.com/VersusControl/versus-incident/tests/fakeaws"
)

// hookTransport runs hook once, on the request number trigger.
type hookTransport struct {
	next    *http.Client
	mu      sync.Mutex
	count   int
	trigger int
	hook    func()
}

func (transport *hookTransport) Do(request *http.Request) (*http.Response, error) {
	transport.mu.Lock()
	transport.count++
	fire := transport.count == transport.trigger
	transport.mu.Unlock()
	if fire {
		transport.hook()
	}
	return transport.next.Do(request)
}

func TestDroppedRegionIsPurgedOnNextSync(t *testing.T) {
	h := newHarness(t, fakeaws.ScenarioBadDeploy)
	mustSync(t, h, target(productionAccount, "us-east-1", "eu-west-1"), KindHealth, KindChanges)
	regions, _ := h.store.Regions(productionAccount)
	if len(regions) != 2 {
		t.Fatalf("regions before = %v", regions)
	}
	mustSync(t, h, target(productionAccount, "us-east-1"), KindHealth)
	regions, _ = h.store.Regions(productionAccount)
	if len(regions) != 1 || regions[0] != "us-east-1" {
		t.Fatalf("regions after = %v", regions)
	}
	result, _ := h.store.Resources(context.Background(), snapshot.Query{})
	for _, resource := range result.Resources {
		if resource.Region == "eu-west-1" {
			t.Fatalf("dropped region still read: %+v", resource)
		}
	}
}

// TestSyncRacingAPurgeCannotResurrect purges the account while its sync is
// in flight: nothing the sync reads afterwards is written back.
func TestSyncRacingAPurgeCannotResurrect(t *testing.T) {
	h := newHarness(t, fakeaws.ScenarioBadDeploy)
	transport := &hookTransport{next: h.fake.HTTPClient(), trigger: 3}
	transport.hook = func() {
		if err := h.store.PurgeAccount(productionAccount); err != nil {
			t.Error(err)
		}
	}
	collector := New(h.store, h.limiter, WithHTTPClient(transport), WithSleep(noSleep), WithRetries(0))
	_, err := collector.Sync(context.Background(), target(productionAccount))
	if !errors.Is(err, snapshot.ErrPurged) {
		t.Fatalf("sync after purge = %v", err)
	}
	accounts, _ := h.store.Accounts()
	if len(accounts) != 0 {
		t.Fatalf("purged account resurrected: %v", accounts)
	}
	result, _ := h.store.Resources(context.Background(), snapshot.Query{})
	if len(result.Resources) != 0 || len(result.Units) != 0 {
		t.Fatalf("store after racing purge = %+v", result)
	}
}

// TestShutdownRecordsNoFailures cancels the context mid-sync: no unit is
// marked failed or timed out and nothing is logged.
func TestShutdownRecordsNoFailures(t *testing.T) {
	var logged []string
	var logMu sync.Mutex
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := newHarness(t, fakeaws.ScenarioBadDeploy)
	transport := &hookTransport{next: h.fake.HTTPClient(), trigger: 2, hook: cancel}
	collector := New(h.store, h.limiter, WithHTTPClient(transport), WithSleep(noSleep), WithRetries(0),
		WithLogger(func(format string, args ...any) {
			logMu.Lock()
			logged = append(logged, format)
			logMu.Unlock()
		}))
	if _, err := collector.Sync(ctx, target(productionAccount)); !errors.Is(err, context.Canceled) {
		t.Fatalf("sync = %v", err)
	}
	statuses, _ := h.store.Sync(context.Background(), snapshot.Query{Account: productionAccount})
	for _, status := range statuses {
		if status.Sync.State == model.SyncFailed {
			t.Fatalf("shutdown recorded a failure: %+v", status)
		}
	}
	logMu.Lock()
	defer logMu.Unlock()
	for _, line := range logged {
		if strings.Contains(line, "failed") {
			t.Fatalf("shutdown logged a failure: %v", logged)
		}
	}
}

func TestSchedulerPurgesRemovedAccounts(t *testing.T) {
	var logged []string
	h := newHarness(t, fakeaws.ScenarioBadDeploy, WithLogger(func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }))
	mustSync(t, h, target(productionAccount), KindHealth)
	mustSync(t, h, target(stagingAccount, "eu-west-1"), KindHealth)
	known := []string{productionAccount, stagingAccount}
	var listErr error
	scheduler := NewScheduler(h.collector,
		func(context.Context) []Target { return nil },
		func() Intervals { return Intervals{} },
		nil,
		WithAccounts(func(context.Context) ([]string, error) { return known, listErr }))

	listErr = errors.New("storage unavailable")
	known = nil
	scheduler.RunDue(context.Background())
	if accounts, _ := h.store.Accounts(); len(accounts) != 2 {
		t.Fatalf("an unreadable account list purged data: %v", accounts)
	}
	if len(logged) == 0 || logged[len(logged)-1] != "aws collect: purge skipped code=account_list_unavailable" {
		t.Fatalf("skipped purge not logged: %v", logged)
	}
	listErr = nil
	known = []string{productionAccount}
	scheduler.RunDue(context.Background())
	if accounts, _ := h.store.Accounts(); len(accounts) != 1 || accounts[0] != productionAccount {
		t.Fatalf("accounts after removal = %v", accounts)
	}
}

// TestFirstSyncTotalIsFixedFromTheStart is the QA-373 repro: syncing only
// the health kind already registers every unit, so the progress total does
// not grow as later kinds start.
func TestFirstSyncTotalIsFixedFromTheStart(t *testing.T) {
	h := newHarness(t, fakeaws.ScenarioBadDeploy)
	full := len(Plan(productionAccount, []string{testRegion}))
	mustSync(t, h, target(productionAccount), KindHealth)
	statuses, err := h.store.Sync(context.Background(), snapshot.Query{Account: productionAccount})
	if err != nil {
		t.Fatal(err)
	}
	pending := 0
	for _, status := range statuses {
		if status.Sync.State == model.SyncPending {
			pending++
		}
	}
	if len(statuses) != full || pending == 0 {
		t.Fatalf("after the health kind: %d units (%d pending), want the full %d", len(statuses), pending, full)
	}
	mustSync(t, h, target(productionAccount), KindChanges, KindInventory)
	statuses, _ = h.store.Sync(context.Background(), snapshot.Query{Account: productionAccount})
	if len(statuses) != full {
		t.Fatalf("total moved from %d to %d", full, len(statuses))
	}
}
