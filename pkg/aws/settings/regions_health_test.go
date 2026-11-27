package settings

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/VersusControl/versus-incident/pkg/aws/limit"
	"github.com/VersusControl/versus-incident/pkg/aws/probe"
	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/tests/fakeaws"
)

const regionTestExternalID = "vrs-ext-regiontest01"

func regionAccount(id string, regions ...string) AccountRecord {
	return AccountRecord{AccountID: id, RoleARN: "arn:aws:iam::" + id + ":role/versus-reader", ExternalID: regionTestExternalID, Regions: regions}
}

func regionService(t *testing.T, scenario string, client aws.HTTPClient) *Service {
	t.Helper()
	fake := fakeaws.New(t, scenario)
	if client == nil {
		client = fake.HTTPClient()
	} else if gate, ok := client.(*gatedClient); ok {
		gate.base = fake.HTTPClient()
	}
	return NewService(testStore(t), WithConfig(enabledConfig()), WithHTTPClient(client), WithBaseProvider(staticBase), WithLimiter(limit.NewManager(1000)))
}

// TestNotEnabledRegionSortingFirstKeepsTheAccountConnected: a wizard
// account stores its Regions sorted, so an opt-in Region it has not
// enabled (af-south-1) comes first. STS is signed in the partition's home
// Region, not the first selected one, so the account connects and its
// probes run in an enabled Region.
func TestNotEnabledRegionSortingFirstKeepsTheAccountConnected(t *testing.T) {
	if got := homeRegion([]string{"af-south-1", "eu-west-1"}); got != "us-east-1" {
		t.Fatalf("home Region = %s", got)
	}
	if got := homeRegion([]string{"cn-northwest-1"}); got != "cn-north-1" {
		t.Fatalf("China home Region = %s", got)
	}
	service := regionService(t, fakeaws.ScenarioRegionNotEnabled, nil)
	record, err := service.CheckAccount(context.Background(), regionAccount("444455556666", fakeaws.NotEnabledRegion, "us-east-1"))
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != StatusConnected || record.ErrorCode != "" {
		t.Fatalf("account with af-south-1 first = %q %q", record.Status, record.ErrorCode)
	}
	readable := 0
	for _, result := range record.ProbeResults {
		if result.Status == probe.StatusReadable {
			readable++
		}
	}
	if readable == 0 {
		t.Fatalf("probes ran in the not-enabled Region: %+v", record.ProbeResults)
	}
}

// TestHealthCheckRecordsNotEnabledRegions: a configured account's health
// check records each selected Region's opt-in status, so Settings lists the
// not-enabled one instead of reading it as selected.
func TestHealthCheckRecordsNotEnabledRegions(t *testing.T) {
	service := regionService(t, fakeaws.ScenarioRegionNotEnabled, nil)
	selected := []string{"us-east-1", fakeaws.NotEnabledRegion}
	record, err := service.CheckAccount(context.Background(), regionAccount("444455556666", selected...))
	if err != nil {
		t.Fatal(err)
	}
	if got := record.NotEnabledRegions(selected); !slices.Equal(got, []string{fakeaws.NotEnabledRegion}) {
		t.Fatalf("not enabled = %v (regions %+v)", got, record.Regions)
	}
	stored, err := service.store.GetHealth("default", "444455556666")
	if err != nil || !slices.Equal(stored.NotEnabledRegions(selected), []string{fakeaws.NotEnabledRegion}) {
		t.Fatalf("stored not enabled = %v %v", stored.NotEnabledRegions(selected), err)
	}
	view, err := service.accountView("default", regionAccount("444455556666", selected...), false)
	if err != nil || !slices.Equal(view.Regions.NotEnabled, []string{fakeaws.NotEnabledRegion}) {
		t.Fatalf("settings view not enabled = %+v %v", view.Regions, err)
	}
	if got := (HealthRecord{}).NotEnabledRegions(selected); len(got) != 0 {
		t.Fatalf("unknown status listed as not enabled: %v", got)
	}
}

// gatedClient holds every non-STS call until released, so a test can look
// at what a check saved while its service probe is still running.
type gatedClient struct {
	base    aws.HTTPClient
	release chan struct{}
	once    sync.Once
}

func (client *gatedClient) Do(request *http.Request) (*http.Response, error) {
	if !strings.HasPrefix(request.URL.Host, "sts.") && !strings.HasPrefix(request.Host, "sts.") {
		<-client.release
	}
	return client.base.Do(request)
}

func (client *gatedClient) open() { client.once.Do(func() { close(client.release) }) }

// TestFirstCheckSavesIdentityBeforeTheProbes: right after boot every
// account shows its identity result while the slower region and service
// reads run, and accounts are checked side by side, so none waits at
// "unknown" behind another account's probe.
func TestFirstCheckSavesIdentityBeforeTheProbes(t *testing.T) {
	gate := &gatedClient{release: make(chan struct{})}
	defer gate.open()
	service := regionService(t, "new-account-connect", gate)
	service.readConfig = func() config.AWSConnectorConfig {
		connector := enabledConfig()()
		for _, account := range []AccountRecord{regionAccount("111122223333", "us-east-1"), regionAccount("444455556666", "us-east-1")} {
			connector.Accounts = append(connector.Accounts, config.AWSAccountConfig{AccountID: account.AccountID, Regions: account.Regions,
				Auth: config.AWSAuthConfig{Mode: "assume_role", RoleARN: account.RoleARN, ExternalID: regionTestExternalID}})
		}
		return connector
	}
	done := make(chan bool)
	go func() { done <- service.checkAll(context.Background()) }()
	eventually(t, "both accounts to connect while their probes are held", func() bool {
		connected := 0
		for _, id := range []string{"111122223333", "444455556666"} {
			if record, err := service.store.GetHealth("default", id); err == nil && record.Status == StatusConnected {
				connected++
			}
		}
		return connected == 2
	})
	gate.open()
	if failed := <-done; failed {
		t.Fatal("a check failed")
	}
}
