package agent

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/VersusControl/versus-incident/pkg/baseline"
	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

// BaselineProviderContributor builds authorized read-only baseline surfaces.
type BaselineProviderContributor func(tenancy.OrgScope, []config.AgentSourceConfig) ([]baseline.Surface, []error)

var baselineProviderContributors = struct {
	sync.RWMutex
	items map[string]BaselineProviderContributor
}{items: map[string]BaselineProviderContributor{}}

// RegisterBaselineProviderContributor installs or removes one named contributor.
func RegisterBaselineProviderContributor(name string, contributor BaselineProviderContributor) {
	name = strings.TrimSpace(name)
	if name == "" {
		return
	}
	baselineProviderContributors.Lock()
	defer baselineProviderContributors.Unlock()
	if contributor == nil {
		delete(baselineProviderContributors.items, name)
		return
	}
	baselineProviderContributors.items[name] = contributor
}

func contributedBaselineProviders(scope tenancy.OrgScope, sources []config.AgentSourceConfig) ([]baseline.Surface, []error) {
	baselineProviderContributors.RLock()
	names := make([]string, 0, len(baselineProviderContributors.items))
	items := make(map[string]BaselineProviderContributor, len(baselineProviderContributors.items))
	for name, contributor := range baselineProviderContributors.items {
		names = append(names, name)
		items[name] = contributor
	}
	baselineProviderContributors.RUnlock()
	sort.Strings(names)
	var surfaces []baseline.Surface
	var errs []error
	for _, name := range names {
		built, buildErrs := items[name](scope.Normalized(), sources)
		for _, err := range buildErrs {
			if err != nil {
				errs = append(errs, fmt.Errorf("baseline contributor %s: %w", name, err))
			}
		}
		surfaces = append(surfaces, built...)
	}
	return surfaces, errs
}
