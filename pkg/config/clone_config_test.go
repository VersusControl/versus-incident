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

// TestResolveCarriesBaseURL asserts the shared ai.base_url flows through
// Resolve when a task does not override it, and that a per-task base_url
// wins when set — the contract the factory relies on to point detect and
// analyze at an OpenAI-compatible endpoint.
func TestResolveCarriesBaseURL(t *testing.T) {
	base := AgentAIConfig{APIKey: "k", BaseURL: "http://shared:11434/v1", Model: "m"}

	if got := base.Resolve(AgentAITaskConfig{}); got.BaseURL != "http://shared:11434/v1" {
		t.Errorf("Resolve dropped top-level base_url: %q", got.BaseURL)
	}
	if got := base.Resolve(AgentAITaskConfig{BaseURL: "http://detect:8080/v1"}); got.BaseURL != "http://detect:8080/v1" {
		t.Errorf("Resolve ignored per-task base_url: %q", got.BaseURL)
	}
}

// TestCloneConfigCarriesBaseURL asserts base_url survives a full
// cloneConfig round-trip at every level (shared, detect, analyze) — the
// config triple-touch landmine that silently drops per-request overrides
// when a new field is added to the struct but not the deep-clone.
func TestCloneConfigCarriesBaseURL(t *testing.T) {
	src := &Config{}
	src.Agent.AI.BaseURL = "http://shared/v1"
	src.Agent.AI.Detect = AgentAITaskConfig{BaseURL: "http://detect/v1"}
	src.Agent.AI.Analyze = AgentAIAnalyzeConfig{BaseURL: "http://analyze/v1"}

	dst := cloneConfig(src)
	if dst.Agent.AI.BaseURL != "http://shared/v1" {
		t.Errorf("shared base_url lost: %q", dst.Agent.AI.BaseURL)
	}
	if dst.Agent.AI.Detect.BaseURL != "http://detect/v1" {
		t.Errorf("detect base_url lost: %q", dst.Agent.AI.Detect.BaseURL)
	}
	if dst.Agent.AI.Analyze.BaseURL != "http://analyze/v1" {
		t.Errorf("analyze base_url lost: %q", dst.Agent.AI.Analyze.BaseURL)
	}
}
