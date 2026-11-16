package kubernetes

import (
	"fmt"
	"testing"
)

// A Role and RoleBinding in a few namespaces gives a partial cluster
// overview, not a denied one: the overview answers on every cycle once the
// granted namespaces have been read, and counts what they hold.
func TestNamespaceOnlyRoleKeepsAPartialOverview(t *testing.T) {
	t.Parallel()
	cluster := namespaceOnlyCluster(t)
	ctx := t.Context()
	holdsOnEveryCycle(t, 6, func(cycle int) {
		due := map[string]bool(nil)
		if cycle%2 == 1 {
			due = map[string]bool{"Pod": true, "Event": true}
		}
		cluster.cycle(due, "Pod")
	}, func() error {
		overview, err := cluster.service.Overview(ctx)
		if err != nil {
			return fmt.Errorf("overview failed under a namespace-only role: %w", err)
		}
		if overview.Pods == 0 || overview.Workloads == 0 {
			return fmt.Errorf("overview counted nothing the granted namespaces hold: pods=%d workloads=%d", overview.Pods, overview.Workloads)
		}
		if overview.Sync.State == "error" {
			return fmt.Errorf("overview reported sync state=%s", overview.Sync.State)
		}
		return nil
	})
}
