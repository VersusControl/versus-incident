package servicehealth

import (
	"context"
	"sync"

	"github.com/VersusControl/versus-incident/pkg/storage"
)

// OrganizationLister returns organizations that need scheduled snapshots.
// Out-of-tree runtimes may register their authoritative organization catalog.
type OrganizationLister func(context.Context) ([]string, error)

var (
	organizationListerMu sync.RWMutex
	organizationLister   OrganizationLister
)

// SetOrganizationLister installs the process-wide organization enumeration hook.
// Passing nil restores the single-tenant OSS default.
func SetOrganizationLister(lister OrganizationLister) {
	organizationListerMu.Lock()
	defer organizationListerMu.Unlock()
	organizationLister = lister
}

// Organizations returns normalized, deduplicated organizations to collect.
// Without a registered lister the OSS default organization is the only one;
// with a lister, its result is authoritative and default is not added.
func Organizations(ctx context.Context) ([]string, error) {
	organizationListerMu.RLock()
	lister := organizationLister
	organizationListerMu.RUnlock()
	if lister == nil {
		return []string{storage.DefaultOrgID}, nil
	}
	listed, err := lister(ctx)
	if err != nil {
		return nil, err
	}
	organizations := make([]string, 0, len(listed))
	seen := make(map[string]struct{}, len(listed))
	for _, orgID := range listed {
		orgID = storage.NormalizeOrgID(orgID)
		if _, exists := seen[orgID]; exists {
			continue
		}
		seen[orgID] = struct{}{}
		organizations = append(organizations, orgID)
	}
	return organizations, nil
}
