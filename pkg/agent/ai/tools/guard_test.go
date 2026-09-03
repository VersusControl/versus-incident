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

func TestKubernetesNativeAuthDependencyClosure(t *testing.T) {
	output, err := exec.Command("go", "list", "-deps", "../../../kubernetes").Output()
	if err != nil {
		t.Fatalf("go list -deps Kubernetes: %v", err)
	}
	forbidden := []string{
		"os/exec",
		"github.com/aws/aws-sdk-go-v2/config",
		"github.com/aws/aws-sdk-go-v2/credentials/processcreds",
		"github.com/aws/aws-sdk-go-v2/credentials/ssocreds",
		"golang.org/x/oauth2/google",
		"golang.org/x/oauth2/authhandler",
		"cloud.google.com/go/auth/credentials/internal/externalaccount/executable",
		"k8s.io/client-go",
	}
	for _, dependency := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		dependency = strings.TrimSpace(dependency)
		for _, prefix := range forbidden {
			if dependency == prefix || strings.HasPrefix(dependency, prefix+"/") {
				t.Errorf("Kubernetes imports forbidden authentication dependency %q", dependency)
			}
		}
	}
}
