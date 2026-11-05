package settings

import (
	"context"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/limit"
	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
	"github.com/VersusControl/versus-incident/tests/fakeaws"
)

func TestSavedBudgetReachesTheLimiterAtRuntime(t *testing.T) {
	store := testStore(t)
	limiter := limit.NewManager(2)
	configured := config.AWSConnectorConfig{
		Enable:              true,
		HealthCheckInterval: "5m",
		Budgets:             config.AWSBudgetsConfig{MaxRequestsPerSecond: 2},
		Accounts: []config.AWSAccountConfig{{
			AccountID:   "111122223333",
			DisplayName: "Production",
			Regions:     []string{"us-east-1", "eu-west-1"},
		}},
	}
	connector := NewService(store,
		WithConfig(func() config.AWSConnectorConfig { return configured }),
		WithLimiter(limiter),
	)

	before, err := connector.View(context.Background(), "default", true)
	if err != nil {
		t.Fatal(err)
	}
	if before.Budget.BudgetRPS != 2 || before.Budget.Source != sourceDefault {
		t.Fatalf("boot budget = %+v", before.Budget)
	}
	if before.Accounts[0].APIUse.BudgetRPS != 4 {
		t.Fatalf("account budget = %+v", before.Accounts[0].APIUse)
	}

	saved := 7
	if err := connector.SaveOptions("default", Options{MaxRequestsPerSecond: &saved}); err != nil {
		t.Fatal(err)
	}
	if limiter.MaxRPS() != 7 {
		t.Fatalf("limiter rate = %d, want 7 without a restart", limiter.MaxRPS())
	}
	after, err := connector.View(context.Background(), "default", true)
	if err != nil {
		t.Fatal(err)
	}
	if after.Budget.BudgetRPS != 7 || after.Budget.Source != string(SourceWizard) {
		t.Fatalf("saved budget = %+v", after.Budget)
	}
	if after.Accounts[0].APIUse.BudgetRPS != 14 {
		t.Fatalf("account budget after save = %+v", after.Accounts[0].APIUse)
	}
}

func TestGlobalProbesDoNotInflateTheBudget(t *testing.T) {
	fake := fakeaws.New(t, "new-account-connect")
	store := NewStore(storage.NewMemory(), tenancy.DefaultOrgScope())
	now := time.Date(2026, 11, 3, 17, 0, 0, 0, time.UTC)
	store.setClock(func() time.Time { return now })
	limiter := limit.NewManager(1000)
	configured := config.AWSConnectorConfig{
		Enable:              true,
		HealthCheckInterval: "5m",
		Budgets:             config.AWSBudgetsConfig{MaxRequestsPerSecond: 2},
	}
	connector := NewService(store,
		WithConfig(func() config.AWSConnectorConfig { return configured }),
		WithClock(func() time.Time { return now }),
		WithHTTPClient(fake.HTTPClient()),
		WithBaseProvider(staticBase),
		WithLimiter(limiter),
	)
	account := AccountRecord{
		AccountID:  "111122223333",
		RoleARN:    "arn:aws:iam::111122223333:role/versus-reader",
		ExternalID: "vrs-ext-abcdefghijklmnopqrstuvwx",
		Regions:    []string{"eu-west-1"},
	}
	if err := store.SaveAccount("default", account); err != nil {
		t.Fatal(err)
	}
	if _, err := connector.CheckAccount(context.Background(), account); err != nil {
		t.Fatal(err)
	}
	view, err := connector.View(context.Background(), "default", true)
	if err != nil {
		t.Fatal(err)
	}
	row := view.Accounts[0]
	// Global services probe us-east-1; that must not add a region to the
	// budget, which stays 2 requests per second for the one enabled region.
	if row.APIUse.BudgetRPS != 2 {
		t.Fatalf("global probes inflated the budget: %+v", row.APIUse)
	}
	if row.APIUse.UsedRPS == 0 {
		t.Fatalf("global probes were not metered: %+v", row.APIUse)
	}
}

// TestOptionalDataSourcesFollowExplicitConfig proves that an unset config key
// leaves its control editable. A shipped config that writes every optional key
// as false must not lock the paid-read toggles.
func TestOptionalDataSourcesFollowExplicitConfig(t *testing.T) {
	account := config.AWSAccountConfig{AccountID: "111122223333", DisplayName: "Production"}
	build := func(connector config.AWSConnectorConfig) (*Service, *Store) {
		connector.Enable = true
		connector.HealthCheckInterval = "5m"
		connector.Accounts = []config.AWSAccountConfig{account}
		store := testStore(t)
		service := NewService(store,
			WithConfig(func() config.AWSConnectorConfig { return connector }),
			WithLimiter(limit.NewManager(100)),
		)
		return service, store
	}
	sources := func(view SettingsView) map[string]string {
		return map[string]string{
			"cost_explorer":   view.OptionalData.CostExplorer.Source,
			"config":          view.OptionalData.Config.Source,
			"security_hub":    view.OptionalData.SecurityHub.Source,
			"trusted_advisor": view.OptionalData.TrustedAdvisor.Source,
			"budget":          view.Budget.Source,
			"allowlist":       view.Privacy.TagValueAllowlistFrom,
		}
	}
	assertAll := func(t *testing.T, got map[string]string, want string) {
		t.Helper()
		for key, value := range got {
			if value != want {
				t.Fatalf("%s source = %q, want %q (all: %+v)", key, value, want, got)
			}
		}
	}

	// Shipped defaults: every optional key false, budget at the documented
	// default, no stored value. Nothing is config-locked.
	service, _ := build(config.AWSConnectorConfig{Budgets: config.AWSBudgetsConfig{MaxRequestsPerSecond: DefaultBudgetRPS}})
	view, err := service.View(context.Background(), "default", true)
	if err != nil {
		t.Fatal(err)
	}
	assertAll(t, sources(view), sourceDefault)
	if view.OptionalData.CostExplorer.Source == string(SourceConfig) {
		t.Fatal("unset optional data was reported as config-managed")
	}

	// An explicit non-default config value is config-managed.
	service, _ = build(config.AWSConnectorConfig{
		Tags:         config.AWSTagsConfig{ValueAllowlist: []string{"team"}},
		Budgets:      config.AWSBudgetsConfig{MaxRequestsPerSecond: 10},
		OptionalData: config.AWSOptionalDataConfig{CostExplorer: true},
	})
	view, err = service.View(context.Background(), "default", true)
	if err != nil {
		t.Fatal(err)
	}
	if view.OptionalData.CostExplorer.Source != string(SourceConfig) || view.OptionalData.CostExplorer.On != 1 {
		t.Fatalf("explicit config toggle = %+v", view.OptionalData.CostExplorer)
	}
	if view.OptionalData.Config.Source != sourceDefault {
		t.Fatalf("unset config toggle = %+v", view.OptionalData.Config)
	}
	if view.Budget.Source != string(SourceConfig) || view.Privacy.TagValueAllowlistFrom != string(SourceConfig) {
		t.Fatalf("config budget/allowlist = %+v / %q", view.Budget, view.Privacy.TagValueAllowlistFrom)
	}

	// A saved wizard value is wizard-managed and wins for display.
	service, _ = build(config.AWSConnectorConfig{Budgets: config.AWSBudgetsConfig{MaxRequestsPerSecond: DefaultBudgetRPS}})
	enabled, budget := true, 7
	if err := service.SaveOptions("default", Options{CostExplorer: &enabled, MaxRequestsPerSecond: &budget}); err != nil {
		t.Fatal(err)
	}
	view, err = service.View(context.Background(), "default", true)
	if err != nil {
		t.Fatal(err)
	}
	if view.OptionalData.CostExplorer.Source != string(SourceWizard) || view.OptionalData.CostExplorer.On != 1 {
		t.Fatalf("stored toggle = %+v", view.OptionalData.CostExplorer)
	}
	if view.OptionalData.Config.Source != sourceDefault {
		t.Fatalf("unset toggle after a save = %+v", view.OptionalData.Config)
	}
	if view.Budget.Source != string(SourceWizard) || view.Budget.BudgetRPS != 7 {
		t.Fatalf("stored budget = %+v", view.Budget)
	}
}
