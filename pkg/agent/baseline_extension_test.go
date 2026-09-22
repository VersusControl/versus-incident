package agent

import (
	"errors"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/baseline"
	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

func TestRegisterBaselineProviderContributorIsOrderedAndScoped(t *testing.T) {
	RegisterBaselineProviderContributor("z-test", func(scope tenancy.OrgScope, _ []config.AgentSourceConfig) ([]baseline.Surface, []error) {
		if scope.Write != "acme" {
			t.Fatalf("scope = %#v", scope)
		}
		return []baseline.Surface{{Family: "traces"}}, []error{errors.New("unavailable")}
	})
	RegisterBaselineProviderContributor("a-test", func(tenancy.OrgScope, []config.AgentSourceConfig) ([]baseline.Surface, []error) {
		return []baseline.Surface{{Family: "metrics"}}, nil
	})
	t.Cleanup(func() {
		RegisterBaselineProviderContributor("z-test", nil)
		RegisterBaselineProviderContributor("a-test", nil)
	})
	surfaces, errs := contributedBaselineProviders(tenancy.NewOrgScope("acme"), nil)
	if len(surfaces) != 2 || surfaces[0].Family != "metrics" || surfaces[1].Family != "traces" {
		t.Fatalf("surfaces = %#v", surfaces)
	}
	if len(errs) != 1 || errs[0].Error() != "baseline contributor z-test: unavailable" {
		t.Fatalf("errors = %v", errs)
	}
}
