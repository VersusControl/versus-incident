package agent

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

// ToolContributor builds read-only runtime tools from trusted configuration.
// External modules use it to add licensed tools without importing private code
// into OSS. Implementations must enforce their own entitlement and org scope.
type ToolContributor func(tenancy.OrgScope, []config.AgentSourceConfig, core.Scrubber) ([]core.Tool, []error)

var toolContributors = struct {
	sync.RWMutex
	items map[string]ToolContributor
}{items: map[string]ToolContributor{}}

// RegisterToolContributor installs or removes one named runtime contributor.
func RegisterToolContributor(name string, contributor ToolContributor) {
	name = strings.TrimSpace(name)
	if name == "" {
		return
	}
	toolContributors.Lock()
	defer toolContributors.Unlock()
	if contributor == nil {
		delete(toolContributors.items, name)
		return
	}
	toolContributors.items[name] = contributor
}

func contributedTools(scope tenancy.OrgScope, sources []config.AgentSourceConfig, scrubber core.Scrubber) ([]core.Tool, []error) {
	toolContributors.RLock()
	names := make([]string, 0, len(toolContributors.items))
	items := make(map[string]ToolContributor, len(toolContributors.items))
	for name, contributor := range toolContributors.items {
		names = append(names, name)
		items[name] = contributor
	}
	toolContributors.RUnlock()
	sort.Strings(names)
	var tools []core.Tool
	var errs []error
	seen := map[string]string{}
	for _, name := range names {
		built, buildErrs := items[name](scope.Normalized(), sources, scrubber)
		for _, err := range buildErrs {
			if err != nil {
				errs = append(errs, fmt.Errorf("tool contributor %s: %w", name, err))
			}
		}
		for _, candidate := range built {
			if candidate == nil {
				continue
			}
			if owner, duplicate := seen[candidate.Name()]; duplicate {
				errs = append(errs, fmt.Errorf("tool %q registered by both %s and %s", candidate.Name(), owner, name))
				continue
			}
			seen[candidate.Name()] = name
			tools = append(tools, candidate)
		}
	}
	return tools, errs
}
