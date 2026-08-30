package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	commontools "github.com/VersusControl/versus-incident/pkg/agent/ai/tools/common"
	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

type panicPullSource struct{ name string }

func (source panicPullSource) Name() string { return source.name }
func (panicPullSource) Pull(context.Context, time.Time) ([]core.Signal, time.Time, error) {
	panic("detection health must not pull sources")
}

func TestDetectionHealthAdapterIsPassiveAndRedactsBuildErrors(t *testing.T) {
	reader := newDetectionHealthAdapter(
		tenancy.DefaultOrgScope(),
		[]config.AgentSourceConfig{
			{Name: "connected", Type: "file", Enable: true},
			{Name: "broken", Type: "loki", Enable: true},
			{Name: "off", Type: "splunk", Enable: false},
		},
		[]core.SignalSource{panicPullSource{name: "connected"}},
		[]error{errors.New("source broken: https://user:secret@example.invalid failed")},
	)
	snapshot := reader.DetectionHealth(tenancy.DefaultOrgScope())
	if len(snapshot.Sources) != 3 {
		t.Fatalf("sources = %d, want 3", len(snapshot.Sources))
	}
	if got := snapshot.Sources[0]; !got.Configured || got.Observation != "unknown" || got.LastSuccessfulPull != nil {
		t.Fatalf("configured source = %+v", got)
	}
	if got := snapshot.Sources[1]; got.Configured || got.Observation != "unknown" || got.ErrorClass != "configuration" {
		t.Fatalf("broken source = %+v", got)
	}
	if got := snapshot.Sources[2]; got.Configured || got.Observation != "unknown" || got.ErrorClass != "disabled" {
		t.Fatalf("disabled source = %+v", got)
	}
	for _, source := range snapshot.Sources {
		if source.ErrorClass == "https://user:secret@example.invalid failed" {
			t.Fatal("raw build error leaked into health snapshot")
		}
	}
	if len(snapshot.Categories) != 3 || !snapshot.Categories[0].Configured || snapshot.Categories[0].Dark || snapshot.Observation != "unknown" {
		t.Fatalf("categories = %+v", snapshot.Categories)
	}
}

// TestBuildGitRepos_EmptyNil asserts an empty repo list yields a nil
// slice so buildAnalyzeTools omits the recent_changes tool.
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
	want := commontools.GitRepo{URL: "https://example.com/x.git"}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("got %+v, want single %+v", got, want)
	}
}
