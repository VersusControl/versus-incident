package config

import (
	"crypto/x509"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestCloneHistoryStorageOptions(t *testing.T) {
	source := Config{Storage: StorageConfig{Postgres: StoragePostgresConfig{DSN: "configured", MaxOpenConns: 24, HistoryReadConcurrency: 8}}, Kubernetes: KubernetesHistoryConfig{Evidence: KubernetesEvidenceConfig{ObservationHeartbeat: "5m", Retention: "720h", SaveChunk: 250}}}
	copy := cloneConfig(&source)
	if !reflect.DeepEqual(copy.Storage, source.Storage) || !reflect.DeepEqual(copy.Kubernetes, source.Kubernetes) {
		t.Fatal("clone dropped persisted-history configuration")
	}
	copy.Kubernetes.Evidence.Retention = "0"
	if source.Kubernetes.Evidence.Retention != "720h" {
		t.Fatal("history retention aliases global config")
	}
}

func loadKubernetesHistoryConfig(t *testing.T, yaml string) KubernetesEvidenceConfig {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadConfigFromPath(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	return loaded.Kubernetes.Evidence
}

func TestKubernetesHistoryDefaults(t *testing.T) {
	t.Setenv("KUBERNETES_EVIDENCE_SAVE_CHUNK", "")
	got := loadKubernetesHistoryConfig(t, "name: versus\n")
	if got.Retention != "168h" || got.SaveChunk != 500 {
		t.Fatalf("defaults = retention %q, save_chunk %d; want 168h and 500", got.Retention, got.SaveChunk)
	}
}

func TestKubernetesHistoryRetentionZeroKeepsEverythingFromFile(t *testing.T) {
	got := loadKubernetesHistoryConfig(t, "kubernetes:\n  evidence:\n    retention: \"0\"\n")
	if got.Retention != "0" {
		t.Fatalf("retention = %q, want the configured \"0\"", got.Retention)
	}
}

func TestKubernetesHistorySaveChunk(t *testing.T) {
	tests := []struct {
		name string
		file string
		env  string
		want int
	}{
		{name: "file value is used", file: "300", want: 300},
		{name: "environment overrides the file", file: "300", env: "64", want: 64},
		{name: "environment alone", env: "1024", want: 1024},
		{name: "smallest value", file: "1", want: 1},
		{name: "zero in the file falls back", file: "0", want: 500},
		{name: "above the range in the file falls back", file: "1025", want: 500},
		{name: "negative in the file falls back", file: "-5", want: 500},
		{name: "above the range in the environment falls back", file: "300", env: "2000", want: 500},
		{name: "empty environment keeps the file value", file: "300", env: "", want: 300},
		{name: "unreadable environment value falls back", file: "300", env: "many", want: 500},
		{name: "unreadable file value falls back", file: "many", want: 500},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("KUBERNETES_EVIDENCE_SAVE_CHUNK", test.env)
			yaml := "name: versus\n"
			if test.file != "" {
				yaml = "kubernetes:\n  evidence:\n    save_chunk: " + test.file + "\n"
			}
			if got := loadKubernetesHistoryConfig(t, yaml).SaveChunk; got != test.want {
				t.Fatalf("save_chunk = %d, want %d", got, test.want)
			}
		})
	}
}

func TestCloneKubernetesIssueTemplateOverrides(t *testing.T) {
	src := Config{Alert: AlertConfig{
		Slack:    SlackConfig{KubernetesIssueTemplatePath: "slack"},
		Telegram: TelegramConfig{KubernetesIssueTemplatePath: "telegram"},
		Viber:    ViberConfig{KubernetesIssueTemplatePath: "viber"},
		Email:    EmailConfig{KubernetesIssueTemplatePath: "email"},
		MSTeams:  MSTeamsConfig{KubernetesIssueTemplatePath: "teams"},
		Lark:     LarkConfig{KubernetesIssueTemplatePath: "lark"},
	}}
	got := cloneConfig(&src)
	if !reflect.DeepEqual(got.Alert, src.Alert) {
		t.Fatalf("%+v", got.Alert)
	}
	got.Alert.Slack.KubernetesIssueTemplatePath = "changed"
	if src.Alert.Slack.KubernetesIssueTemplatePath != "slack" {
		t.Fatal("source mutated")
	}
}

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

func TestCloneConfigCarriesChatKnobs(t *testing.T) {
	src := &Config{}
	src.Agent.AI.Chat = AgentAITaskConfig{Model: "chat-model", MaxTokens: 2048, MaxCallsPerHour: 17}
	dst := cloneConfig(src)
	if dst.Agent.AI.Chat != src.Agent.AI.Chat {
		t.Fatalf("chat block = %+v, want %+v", dst.Agent.AI.Chat, src.Agent.AI.Chat)
	}
}

func TestCloneConfigCarriesSplunkNamespace(t *testing.T) {
	src := &Config{}
	src.Agent.Sources = []AgentSourceConfig{{Name: "logs", Type: "splunk", Splunk: AgentSplunkSourceConfig{Owner: "admin", App: "ops"}}}
	dst := cloneConfig(src)
	if dst.Agent.Sources[0].Splunk.Owner != "admin" || dst.Agent.Sources[0].Splunk.App != "ops" {
		t.Fatalf("namespace lost in clone: %+v", dst.Agent.Sources[0].Splunk)
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

// TestCloneConfigCarriesElasticsearchReorderWindow asserts the ES source's
// reorder_window survives a full cloneConfig round-trip (per-request configs are
// built by cloning the base) and that the cloned Sources slice — including the
// Addresses/ExtraFields slices — is independent from the source, so a mutation
// on one config never bleeds into another.
func TestCloneConfigCarriesElasticsearchReorderWindow(t *testing.T) {
	src := &Config{}
	src.Agent.Sources = []AgentSourceConfig{
		{
			Name: "prod",
			Type: "elasticsearch",
			Elasticsearch: AgentElasticsearchSourceConfig{
				Addresses:       []string{"http://es:9200"},
				AllowLoopback:   true,
				Index:           "logs-*",
				TieBreakerField: "event.sequence",
				ExtraFields:     []string{"error.stack_trace"},
				ReorderWindow:   "90s",
			},
		},
	}
	dst := cloneConfig(src)
	if got := dst.Agent.Sources[0].Elasticsearch.ReorderWindow; got != "90s" {
		t.Fatalf("cloned reorder_window = %q, want %q", got, "90s")
	}
	if !reflect.DeepEqual(dst.Agent.Sources[0].Elasticsearch, src.Agent.Sources[0].Elasticsearch) {
		t.Fatalf("cloned ES source = %+v, want %+v", dst.Agent.Sources[0].Elasticsearch, src.Agent.Sources[0].Elasticsearch)
	}

	// Mutating the clone must not touch the source (deep copy, no shared slices).
	dst.Agent.Sources[0].Elasticsearch.ReorderWindow = "mutated"
	dst.Agent.Sources[0].Elasticsearch.TieBreakerField = "mutated"
	dst.Agent.Sources[0].Elasticsearch.Addresses[0] = "http://mutated:9200"
	dst.Agent.Sources[0].Elasticsearch.ExtraFields[0] = "mutated"
	if src.Agent.Sources[0].Elasticsearch.ReorderWindow != "90s" {
		t.Error("clone shares reorder_window with the source")
	}
	if src.Agent.Sources[0].Elasticsearch.TieBreakerField != "event.sequence" {
		t.Error("clone omitted or shares tie_breaker_field with the source")
	}
	if src.Agent.Sources[0].Elasticsearch.Addresses[0] != "http://es:9200" {
		t.Error("clone shares the Addresses slice with the source")
	}
	if src.Agent.Sources[0].Elasticsearch.ExtraFields[0] != "error.stack_trace" {
		t.Error("clone shares the ExtraFields slice with the source")
	}
}

func TestCloneConfigCarriesLokiServiceLabel(t *testing.T) {
	src := &Config{}
	src.Agent.Sources = []AgentSourceConfig{{Name: "logs", Type: "loki", Loki: AgentLokiSourceConfig{ServiceLabel: "service_name"}}}
	if got := cloneConfig(src).Agent.Sources[0].Loki.ServiceLabel; got != "service_name" {
		t.Fatalf("cloned service_label = %q, want service_name", got)
	}
}

// TestCloneConfigCarriesSignozSource asserts the SigNoz typed block survives a
// full cloneConfig round-trip. Per-request configs are built by cloning the
// base, so a field missing from the clone is silently dropped on every request —
// the source would then be constructed with an empty address and API key.
func TestCloneConfigCarriesSignozSource(t *testing.T) {
	rootCAs := x509.NewCertPool()
	src := &Config{}
	src.Agent.Sources = []AgentSourceConfig{
		{
			Name: "signoz-prod",
			Type: "signoz",
			Signoz: AgentSignozSourceConfig{
				Address:              "https://eu.signoz.cloud",
				APIKey:               "signoz-key",
				InsecureSkipVerify:   true,
				AllowLoopback:        true,
				RootCAs:              rootCAs,
				AllowPrivateNetworks: true,
				Query:                "severity_text = 'ERROR'",
				MessageField:         "body",
				SeverityField:        "severity_text",
				ExtraFields:          []string{"service.name"},
				PageSize:             250,
				ReorderWindow:        "90s",
			},
		},
	}
	dst := cloneConfig(src)
	got := dst.Agent.Sources[0].Signoz
	want := src.Agent.Sources[0].Signoz
	want.RootCAs = got.RootCAs
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("cloned SigNoz source = %+v, want %+v", dst.Agent.Sources[0].Signoz, src.Agent.Sources[0].Signoz)
	}
	if dst.Agent.Sources[0].Signoz.RootCAs == rootCAs {
		t.Fatal("clone shares the RootCAs pool with the source")
	}
	if !dst.Agent.Sources[0].Signoz.RootCAs.Equal(rootCAs) {
		t.Fatal("clone did not carry the RootCAs trust certificates")
	}

	// Mutating the clone must not touch the source (deep copy, no shared slices).
	dst.Agent.Sources[0].Signoz.Address = "mutated"
	dst.Agent.Sources[0].Signoz.ReorderWindow = "mutated"
	dst.Agent.Sources[0].Signoz.ExtraFields[0] = "mutated"
	if src.Agent.Sources[0].Signoz.Address != "https://eu.signoz.cloud" {
		t.Error("clone shares address with the source")
	}
	if src.Agent.Sources[0].Signoz.ReorderWindow != "90s" {
		t.Error("clone shares reorder_window with the source")
	}
	if src.Agent.Sources[0].Signoz.ExtraFields[0] != "service.name" {
		t.Error("clone shares the ExtraFields slice with the source")
	}
}
