package tools_test

import (
	"os/exec"
	"strings"
	"testing"
)

// TestTools_ImportGraphStaysReadOnly is the static import-graph guard
// for the analyze tool catalog. The find_runbook tool (E12) declares its
// own local interfaces (RunbookSearcher, core.Embedder) and must NOT
// pull in any write/emit path. This asserts the whole tools package
// graph never transitively imports:
//
//   - pkg/services  (the emitter / notification path)
//   - pkg/common    (emit helpers)
//   - pkg/agent     (the wiring layer that owns the write path)
//   - pkg/runbook   (the runbook ingestion/write path)
//
// A regression here means a tool gained write capability and broke the
// analyze read-only invariant — fix the import, do not relax the guard.
func TestTools_ImportGraphStaysReadOnly(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").Output()
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}

	// Pure write/ingest/emit paths: forbidden as a whole subtree.
	forbiddenPrefixes := []string{
		"github.com/VersusControl/versus-incident/pkg/services",
		"github.com/VersusControl/versus-incident/pkg/common",
		"github.com/VersusControl/versus-incident/pkg/runbook",
	}
	// The wiring layer that owns the write path. Forbidden as an EXACT
	// match only — this tools package legitimately lives under
	// pkg/agent/ai/analyze/tools, so a prefix match would flag itself.
	forbiddenExact := []string{
		"github.com/VersusControl/versus-incident/pkg/agent",
	}

	for _, dep := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		dep = strings.TrimSpace(dep)
		for _, bad := range forbiddenPrefixes {
			if dep == bad || strings.HasPrefix(dep, bad+"/") {
				t.Errorf("analyze tools package must not import %q (read-only guard)", dep)
			}
		}
		for _, bad := range forbiddenExact {
			if dep == bad {
				t.Errorf("analyze tools package must not import %q (read-only guard)", dep)
			}
		}
	}
}
