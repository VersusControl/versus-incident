package tools_test

import (
	"os/exec"
	"strings"
	"testing"
)

func TestToolGroupsStayReadOnly(t *testing.T) {
	groups := []string{".", "./common", "./versus", "./k8s"}
	forbiddenPrefixes := []string{
		"github.com/VersusControl/versus-incident/pkg/services",
		"github.com/VersusControl/versus-incident/pkg/common",
		"github.com/VersusControl/versus-incident/pkg/runbook",
	}
	forbiddenExact := "github.com/VersusControl/versus-incident/pkg/agent"

	for _, group := range groups {
		t.Run(group, func(t *testing.T) {
			output, err := exec.Command("go", "list", "-deps", group).Output()
			if err != nil {
				t.Fatalf("go list -deps %s: %v", group, err)
			}
			for _, dependency := range strings.Split(strings.TrimSpace(string(output)), "\n") {
				dependency = strings.TrimSpace(dependency)
				for _, forbidden := range forbiddenPrefixes {
					if dependency == forbidden || strings.HasPrefix(dependency, forbidden+"/") {
						t.Errorf("tool group %s imports forbidden read-write dependency %q", group, dependency)
					}
				}
				if dependency == forbiddenExact {
					t.Errorf("tool group %s imports forbidden wiring package %q", group, dependency)
				}
			}
		})
	}
}
