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
// The OSS default organization is always included for backward compatibility.
func Organizations(ctx context.Context) ([]string, error) {
	organizationListerMu.RLock()
	lister := organizationLister
	organizationListerMu.RUnlock()
	organizations := []string{storage.DefaultOrgID}
	if lister == nil {
		return organizations, nil
	}
	listed, err := lister(ctx)
	if err != nil {
		return nil, err
	}
	seen := map[string]struct{}{storage.DefaultOrgID: {}}
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
