package config

import (
	"reflect"
	"testing"
)

// TestCloneAgentAIAnalyzeConfig asserts the analyze model override is
// carried into the cloned config so per-request overrides never lose the
// operator's analyze model. (The tool-loop knobs tool_timeout and
// parallel_tools now live on the shared tools block — see
// TestCloneToolsConfig.)
func TestCloneAgentAIAnalyzeConfig(t *testing.T) {
	src := AgentAIAnalyzeConfig{
		Model: "gpt-4o-mini",
	}
	got := cloneAgentAIAnalyzeConfig(src)
	if got != src {
		t.Fatalf("clone = %+v, want %+v", got, src)
	}
}

// TestCloneConfigCarriesAnalyzeKnobs asserts the analyze model override
// survives a full cloneConfig round-trip via the AI block.
func TestCloneConfigCarriesAnalyzeKnobs(t *testing.T) {
	src := &Config{}
	src.Agent.AI.Analyze = AgentAIAnalyzeConfig{
		Model: "claude-4-6-sonnet",
	}
	dst := cloneConfig(src)
	if dst.Agent.AI.Analyze != src.Agent.AI.Analyze {
		t.Fatalf("analyze block = %+v, want %+v", dst.Agent.AI.Analyze, src.Agent.AI.Analyze)
	}
}

// TestCloneToolsConfig asserts the per-tool config (tools.yaml) is
// carried into the clone — including the root-level tool-loop knobs, the
// recent_changes git repos, and the describe_dependencies graph — so
// per-request overrides never lose it. It also asserts the cloned slices
// are independent from the source.
func TestCloneToolsConfig(t *testing.T) {
	src := ToolsConfig{
		ToolTimeout:   "30s",
		ParallelTools: true,
		RecentChanges: RecentChangesToolConfig{
			Git: RecentChangesGitConfig{
				Auth: GitAuthConfig{Token: "global-token", SSHKeyPath: "/global/key"},
				Repos: []RecentChangesGitRepo{
					{URL: "https://github.com/acme/api.git", Branch: "main", Service: "api"},
					{URL: "git@github.com:acme/web.git", Auth: GitAuthConfig{SSHKeyPath: "/web/key"}},
				},
			},
		},
		DescribeDependencies: DescribeDependenciesToolConfig{
			Services: []ServiceDependency{
				{Name: "web", DependsOn: []string{"api"}},
				{Name: "api", DependsOn: []string{"database", "cache"}},
			},
		},
	}
	got := cloneToolsConfig(src)
	if !reflect.DeepEqual(got, src) {
		t.Fatalf("clone = %+v, want %+v", got, src)
	}
	// Mutating the clone must not affect the source (deep copy).
	got.DescribeDependencies.Services[0].DependsOn[0] = "mutated"
	if src.DescribeDependencies.Services[0].DependsOn[0] != "api" {
		t.Fatal("clone shares the underlying DependsOn slice with the source")
	}
	got.RecentChanges.Git.Repos[0].URL = "mutated"
	if src.RecentChanges.Git.Repos[0].URL != "https://github.com/acme/api.git" {
		t.Fatal("clone shares the underlying Repos slice with the source")
	}
}

// TestCloneConfigCarriesToolsConfig asserts the tools block survives a
// full cloneConfig round-trip via the agent block.
func TestCloneConfigCarriesToolsConfig(t *testing.T) {
	src := &Config{}
	src.Agent.Tools = ToolsConfig{
		ToolTimeout:   "45s",
		ParallelTools: true,
		RecentChanges: RecentChangesToolConfig{
			Git: RecentChangesGitConfig{
				Repos: []RecentChangesGitRepo{{URL: "https://github.com/acme/api.git", Branch: "release"}},
			},
		},
		DescribeDependencies: DescribeDependenciesToolConfig{
			Services: []ServiceDependency{{Name: "api", DependsOn: []string{"database"}}},
		},
	}
	dst := cloneConfig(src)
	if !reflect.DeepEqual(dst.Agent.Tools, src.Agent.Tools) {
		t.Fatalf("tools block = %+v, want %+v", dst.Agent.Tools, src.Agent.Tools)
	}
}
