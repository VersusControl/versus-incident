package scheduler_test

import (
	"os/exec"
	"strings"
	"testing"
)

// TestScheduler_ImportGraphStaysReadOnly is the static import-graph guard
// for the recurring-evaluation seam (E13-T2). Scheduler-driven jobs are
// bound by the SAME read-only analyze invariant as on-demand analyze: the
// scheduler package itself must never gain a write/mutation/emit import, so
// a registered job cannot acquire write capability through it. Findings a
// job raises must route through the single permitted emission path
// (services.CreateIncident), which lives in the consumer's job closure —
// never in this package.
//
// This asserts the scheduler package graph never transitively imports any
// write/emit/persistence path. A regression here means the scheduler gained
// mutation capability and broke the invariant — fix the import, do not relax
// the guard.
func TestScheduler_ImportGraphStaysReadOnly(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").Output()
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}

	// Pure write/ingest/emit/persistence paths: forbidden as whole subtrees.
	// The scheduler owns timing/lifecycle only and must stay storage- and
	// emit-agnostic; the job content (and any emission) is the consumer's.
	forbidden := []string{
		"github.com/VersusControl/versus-incident/pkg/services",
		"github.com/VersusControl/versus-incident/pkg/common",
		"github.com/VersusControl/versus-incident/pkg/runbook",
		"github.com/VersusControl/versus-incident/pkg/storage",
		"github.com/VersusControl/versus-incident/pkg/agent",
		"github.com/VersusControl/versus-incident/pkg/controllers",
	}

	for _, dep := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		dep = strings.TrimSpace(dep)
		for _, bad := range forbidden {
			if dep == bad || strings.HasPrefix(dep, bad+"/") {
				t.Errorf("scheduler package must not import %q (read-only seam)", dep)
			}
		}
	}
}
