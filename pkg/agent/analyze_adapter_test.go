package agent

import (
	"testing"

	analyzetools "github.com/VersusControl/versus-incident/pkg/agent/ai/analyze/tools"
	"github.com/VersusControl/versus-incident/pkg/config"
)

// TestBuildGitRepos_EmptyNil asserts an empty repo list yields a nil
// slice so analyzetools.Default omits the recent_changes tool.
func TestBuildGitRepos_EmptyNil(t *testing.T) {
	if got := buildGitRepos(config.RecentChangesGitConfig{}); got != nil {
		t.Fatalf("empty config should yield nil, got %v", got)
	}
}

// TestBuildGitRepos_AuthFallback asserts each repo inherits the global
// git.auth when its own auth fields are empty, and that a per-repo auth
// value overrides the global default field-by-field.
func TestBuildGitRepos_AuthFallback(t *testing.T) {
	src := config.RecentChangesGitConfig{
		Auth: config.GitAuthConfig{Token: "global-token", SSHKeyPath: "/global/key"},
		Repos: []config.RecentChangesGitRepo{
			// Inherits both global fields.
			{URL: "https://github.com/acme/api.git", Service: "api"},
			// Overrides token; inherits global ssh key.
			{URL: "https://github.com/acme/web.git", Auth: config.GitAuthConfig{Token: "web-token"}},
			// Overrides ssh key; inherits global token.
			{URL: "git@github.com:acme/db.git", Auth: config.GitAuthConfig{SSHKeyPath: "/db/key"}},
		},
	}
	got := buildGitRepos(src)
	if len(got) != 3 {
		t.Fatalf("got %d repos, want 3", len(got))
	}

	if got[0].Token != "global-token" || got[0].SSHKeyPath != "/global/key" {
		t.Fatalf("repo[0] should inherit both global auth fields: %+v", got[0])
	}
	if got[0].URL != "https://github.com/acme/api.git" || got[0].Service != "api" {
		t.Fatalf("repo[0] non-auth fields wrong: %+v", got[0])
	}
	if got[1].Token != "web-token" || got[1].SSHKeyPath != "/global/key" {
		t.Fatalf("repo[1] should override token, inherit ssh key: %+v", got[1])
	}
	if got[2].Token != "global-token" || got[2].SSHKeyPath != "/db/key" {
		t.Fatalf("repo[2] should inherit token, override ssh key: %+v", got[2])
	}
}

// TestBuildGitRepos_NoGlobalAuth asserts empty global auth leaves each
// repo's auth empty so the feed relies on ambient git credentials.
func TestBuildGitRepos_NoGlobalAuth(t *testing.T) {
	got := buildGitRepos(config.RecentChangesGitConfig{
		Repos: []config.RecentChangesGitRepo{{URL: "https://example.com/x.git"}},
	})
	want := analyzetools.GitRepo{URL: "https://example.com/x.git"}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("got %+v, want single %+v", got, want)
	}
}
