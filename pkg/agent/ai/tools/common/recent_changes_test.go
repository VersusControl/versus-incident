package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/VersusControl/versus-incident/pkg/core"
)

// fakeChangeFeed is a scripted ChangeFeed for tests. It records the
// `since` it was called with so assertions can verify window math.
type fakeChangeFeed struct {
	records  []ChangeRecord
	err      error
	gotSince time.Time
	calls    int
}

func (f *fakeChangeFeed) Changes(_ context.Context, since time.Time) ([]ChangeRecord, error) {
	f.calls++
	f.gotSince = since
	if f.err != nil {
		return nil, f.err
	}
	return f.records, nil
}

func mustInvoke(t *testing.T, rc RecentChanges, args map[string]any) *core.ToolResult {
	t.Helper()
	var raw json.RawMessage
	if args != nil {
		b, err := json.Marshal(args)
		if err != nil {
			t.Fatalf("marshal args: %v", err)
		}
		raw = b
	}
	res, err := rc.Invoke(context.Background(), raw)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	return res
}

func TestRecentChanges_Metadata(t *testing.T) {
	rc := RecentChanges{}
	if rc.Name() != "recent_changes" {
		t.Fatalf("Name = %q", rc.Name())
	}
	if rc.Description() == "" {
		t.Fatal("Description empty")
	}
	schema := rc.ArgsSchema()
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("schema properties missing: %+v", schema)
	}
	for _, k := range []string{"service", "time_range_minutes", "window_minutes", "start", "end"} {
		if _, ok := props[k]; !ok {
			t.Fatalf("schema missing property %q", k)
		}
	}
}

func TestRecentChanges_NilFeedIsUnavailable(t *testing.T) {
	rc := RecentChanges{Feed: nil}
	res := mustInvoke(t, rc, nil)
	if res.Tool != "recent_changes" {
		t.Fatalf("Tool = %q", res.Tool)
	}
	if res.IsAvailable() || res.Reason == "" {
		t.Fatalf("result = %+v, want unavailable with reason", res)
	}
}

func TestRecentChanges_WindowClamping(t *testing.T) {
	now := time.Now().UTC()
	tests := []struct {
		name       string
		window     int
		wantWindow int
		wantMaxAge time.Duration
	}{
		{"default when zero", 0, recentChangesDefaultWindow, time.Duration(recentChangesDefaultWindow) * time.Minute},
		{"default when negative", -5, recentChangesDefaultWindow, time.Duration(recentChangesDefaultWindow) * time.Minute},
		{"passthrough mid", 300, 300, 300 * time.Minute},
		{"cap at max", 100000, recentChangesMaxWindow, time.Duration(recentChangesMaxWindow) * time.Minute},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			feed := &fakeChangeFeed{}
			rc := RecentChanges{Feed: feed}
			args := map[string]any{}
			if tc.window != 0 {
				args["window_minutes"] = tc.window
			}
			res := mustInvoke(t, rc, args)
			if res.Data["time_range_minutes"] != tc.wantWindow {
				t.Fatalf("time_range_minutes = %v, want %d", res.Data["time_range_minutes"], tc.wantWindow)
			}
			// since should be ~now-wantMaxAge.
			gotAge := now.Sub(feed.gotSince)
			if gotAge < tc.wantMaxAge-time.Minute || gotAge > tc.wantMaxAge+time.Minute {
				t.Fatalf("since age = %s, want ~%s", gotAge, tc.wantMaxAge)
			}
		})
	}
}

func TestRecentChanges_ServiceFilterAndOrdering(t *testing.T) {
	now := time.Now().UTC()
	feed := &fakeChangeFeed{records: []ChangeRecord{
		{Timestamp: now.Add(-10 * time.Minute), Service: "api", Kind: "deploy", Summary: "newest api"},
		{Timestamp: now.Add(-30 * time.Minute), Service: "API", Kind: "config", Summary: "older api"},
		{Timestamp: now.Add(-20 * time.Minute), Service: "db", Kind: "deploy", Summary: "db change"},
	}}
	rc := RecentChanges{Feed: feed}

	res := mustInvoke(t, rc, map[string]any{"service": "api"})
	if !res.Found {
		t.Fatal("expected Found=true")
	}
	if res.Data["count"] != 2 {
		t.Fatalf("count = %v, want 2 (case-insensitive api match)", res.Data["count"])
	}
	changes, ok := res.Data["changes"].([]ChangeRecord)
	if !ok {
		t.Fatalf("changes wrong type: %T", res.Data["changes"])
	}
	if len(changes) != 2 {
		t.Fatalf("len(changes) = %d, want 2", len(changes))
	}
	// Newest first.
	if changes[0].Summary != "newest api" || changes[1].Summary != "older api" {
		t.Fatalf("ordering wrong: %+v", changes)
	}
}

func TestRecentChanges_DropsOutOfWindow(t *testing.T) {
	now := time.Now().UTC()
	feed := &fakeChangeFeed{records: []ChangeRecord{
		{Timestamp: now.Add(-5 * time.Minute), Service: "api", Summary: "in"},
		{Timestamp: now.Add(-200 * time.Minute), Service: "api", Summary: "out"},
	}}
	rc := RecentChanges{Feed: feed}
	res := mustInvoke(t, rc, map[string]any{"window_minutes": 60})
	if res.Data["count"] != 1 {
		t.Fatalf("count = %v, want 1", res.Data["count"])
	}
}

func TestRecentChanges_EmptyFeedIsMiss(t *testing.T) {
	rc := RecentChanges{Feed: &fakeChangeFeed{}}
	res := mustInvoke(t, rc, nil)
	if res.Found {
		t.Fatal("empty feed should be Found=false")
	}
	if _, ok := res.Data["changes"]; ok {
		t.Fatal("miss should not carry a changes key")
	}
}

func TestRecentChanges_FeedErrorSurfaces(t *testing.T) {
	rc := RecentChanges{Feed: &fakeChangeFeed{err: context.DeadlineExceeded}}
	_, err := rc.Invoke(context.Background(), nil)
	if err == nil {
		t.Fatal("expected error from feed to surface")
	}
}

func TestRecentChanges_BadArgs(t *testing.T) {
	rc := RecentChanges{Feed: &fakeChangeFeed{}}
	_, err := rc.Invoke(context.Background(), json.RawMessage(`{"window_minutes":"oops"}`))
	if err == nil {
		t.Fatal("expected parse error for non-integer window_minutes")
	}
}

func TestNewGitChangeFeed_EmptyReposNil(t *testing.T) {
	if NewGitChangeFeed(nil) != nil {
		t.Fatal("nil repos should yield nil feed")
	}
	if NewGitChangeFeed([]GitRepo{}) != nil {
		t.Fatal("empty repos should yield nil feed")
	}
	if NewGitChangeFeed([]GitRepo{{URL: "   "}}) != nil {
		t.Fatal("whitespace-only URL should be dropped, yielding nil feed")
	}
}

func TestServiceFromURL(t *testing.T) {
	tests := []struct {
		url  string
		want string
	}{
		{"git@github.com:acme/web.git", "web"},
		{"https://github.com/acme/api.git", "api"},
		{"https://github.com/acme/api", "api"},
		{"https://github.com/acme/api/", "api"},
		{"/srv/payments", "payments"},
		{"", ""},
	}
	for _, tc := range tests {
		if got := serviceFromURL(tc.url); got != tc.want {
			t.Fatalf("serviceFromURL(%q) = %q, want %q", tc.url, got, tc.want)
		}
	}
}

func TestGitAuth(t *testing.T) {
	t.Run("empty auth returns nil", func(t *testing.T) {
		auth := gitAuth(GitRepo{URL: "https://example.com/x.git"})
		if auth != nil {
			t.Fatalf("empty auth should return nil, got %v", auth)
		}
	})
	t.Run("token returns BasicAuth", func(t *testing.T) {
		auth := gitAuth(GitRepo{Token: "secret-token"})
		if auth == nil {
			t.Fatal("token should yield non-nil auth")
		}
		if auth.Name() != "http-basic-auth" {
			t.Fatalf("auth.Name() = %q, want http-basic-auth", auth.Name())
		}
	})
	t.Run("both token and ssh prefers token", func(t *testing.T) {
		// Token takes precedence in the current implementation.
		auth := gitAuth(GitRepo{Token: "t", SSHKeyPath: "/nonexistent"})
		if auth == nil {
			t.Fatal("expected non-nil auth")
		}
		if auth.Name() != "http-basic-auth" {
			t.Fatalf("auth.Name() = %q, want http-basic-auth (token wins)", auth.Name())
		}
	})
}

// initGitRepo builds a throwaway git repo with the given commits (oldest
// first) under a subdirectory named `name` (so the auto-detected service
// is predictable) and returns its path. Each commit stamps the same
// author and committer date so windowing is deterministic. The repo is
// written in-process with go-git, so no git binary is needed.
func initGitRepo(t *testing.T, name string, commits []struct {
	when    time.Time
	file    string
	message string
}) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("init repo: %v", err)
	}
	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}
	for _, c := range commits {
		path := filepath.Join(dir, c.file)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(path, []byte(c.message), 0o600); err != nil {
			t.Fatalf("write file: %v", err)
		}
		if _, err := worktree.Add(c.file); err != nil {
			t.Fatalf("add %s: %v", c.file, err)
		}
		signature := &object.Signature{Name: "Test", Email: "test@example.com", When: c.when.UTC()}
		if _, err := worktree.Commit(c.message, &git.CommitOptions{Author: signature, Committer: signature}); err != nil {
			t.Fatalf("commit %q: %v", c.message, err)
		}
	}
	return dir
}

type commitSpec = struct {
	when    time.Time
	file    string
	message string
}

func TestGitChangeFeed_EndToEnd_ServiceAutoDetect(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	repo := initGitRepo(t, "payments", []commitSpec{
		{now.Add(-300 * time.Minute), "old.go", "old deploy"},
		{now.Add(-10 * time.Minute), "new.go", "new deploy"},
		{now.Add(-5 * time.Minute), "schema.sql", "db change"},
	})

	// No explicit service: it is auto-detected from the repo name. A
	// local path is a valid git URL for the mirror clone.
	feed := newGitChangeFeed([]GitRepo{{URL: repo}}, t.TempDir())
	if feed == nil {
		t.Fatal("non-empty repos should yield a feed")
	}

	// Window covers only the last two commits (the -300m one is excluded).
	recs, err := feed.Changes(context.Background(), now.Add(-60*time.Minute))
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("got %d records, want 2: %+v", len(recs), recs)
	}
	for _, r := range recs {
		if r.Kind != changesGitKind {
			t.Fatalf("kind = %q", r.Kind)
		}
		if r.Service != "payments" {
			t.Fatalf("service = %q, want payments (auto-detected)", r.Service)
		}
	}
}

func TestGitChangeFeed_ExplicitServiceAndMultiRepo(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	apiRepo := initGitRepo(t, "api-src", []commitSpec{
		{now.Add(-8 * time.Minute), "main.go", "api deploy"},
	})
	webRepo := initGitRepo(t, "web", []commitSpec{
		{now.Add(-4 * time.Minute), "index.js", "web deploy"},
	})

	feed := newGitChangeFeed([]GitRepo{
		{URL: apiRepo, Service: "api"}, // explicit service overrides repo name
		{URL: webRepo},                 // auto-detected as "web"
	}, t.TempDir())

	recs, err := feed.Changes(context.Background(), now.Add(-60*time.Minute))
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("got %d records, want 2 (aggregated across repos): %+v", len(recs), recs)
	}
	services := map[string]bool{}
	for _, r := range recs {
		services[r.Service] = true
	}
	if !services["api"] || !services["web"] {
		t.Fatalf("services = %v, want both api and web", services)
	}
}

func TestGitChangeFeed_ThroughTool(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	repo := initGitRepo(t, "billing", []commitSpec{
		{now.Add(-3 * time.Minute), "main.go", "latest"},
	})

	rc := RecentChanges{Feed: newGitChangeFeed([]GitRepo{{URL: repo}}, t.TempDir())}
	res := mustInvoke(t, rc, map[string]any{"service": "billing"})
	if !res.Found {
		t.Fatal("expected Found=true from end-to-end git feed")
	}
	if res.Data["count"] != 1 {
		t.Fatalf("count = %v, want 1", res.Data["count"])
	}
}

func TestGitChangeFeed_BadRepoErrors(t *testing.T) {
	feed := newGitChangeFeed([]GitRepo{{URL: filepath.Join(t.TempDir(), "not-a-repo")}}, t.TempDir())
	if _, err := feed.Changes(context.Background(), time.Now().Add(-time.Hour)); err == nil {
		t.Fatal("expected error when the only repo fails to clone")
	}
}

func TestGitChangeFeed_PartialFailureDegrades(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	good := initGitRepo(t, "good", []commitSpec{
		{now.Add(-2 * time.Minute), "main.go", "good deploy"},
	})

	// One healthy repo plus one broken remote: the broken one is skipped
	// and the healthy repo's records still surface.
	feed := newGitChangeFeed([]GitRepo{
		{URL: filepath.Join(t.TempDir(), "missing")},
		{URL: good},
	}, t.TempDir())

	recs, err := feed.Changes(context.Background(), now.Add(-60*time.Minute))
	if err != nil {
		t.Fatalf("partial failure should not surface an error: %v", err)
	}
	if len(recs) != 1 || recs[0].Service != "good" {
		t.Fatalf("records = %+v, want one record for service good", recs)
	}
}

func TestGitChangeFeed_ReadsLocalBareRepository(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	source := initGitRepo(t, "source", []commitSpec{
		{now.Add(-6 * time.Minute), "main.go", "ledger deploy"},
	})
	bare := filepath.Join(t.TempDir(), "ledger.git")
	if _, err := git.PlainClone(bare, true, &git.CloneOptions{URL: source}); err != nil {
		t.Fatalf("bare clone: %v", err)
	}

	feed := newGitChangeFeed([]GitRepo{{URL: bare}}, t.TempDir())
	recs, err := feed.Changes(context.Background(), now.Add(-60*time.Minute))
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	if len(recs) != 1 || recs[0].Service != "ledger" || recs[0].Summary != "ledger deploy" {
		t.Fatalf("records = %+v, want one ledger deploy record", recs)
	}
}

func TestGitChangeFeed_WorkingTreeWithConfigFolder(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	repo := initGitRepo(t, "settings", []commitSpec{
		{now.Add(-7 * time.Minute), "config/app.yaml", "tune pool size"},
	})

	feed := newGitChangeFeed([]GitRepo{{URL: repo}}, t.TempDir())
	recs, err := feed.Changes(context.Background(), now.Add(-60*time.Minute))
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	if len(recs) != 1 || recs[0].Summary != "tune pool size" {
		t.Fatalf("records = %+v, want the config change", recs)
	}
}

func TestGitChangeFeed_WorkingTreeWithGitdirFile(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	source := initGitRepo(t, "source", []commitSpec{
		{now.Add(-9 * time.Minute), "main.go", "checkout deploy"},
	})
	// A linked checkout points at its repository through a ".git" file.
	checkout := filepath.Join(t.TempDir(), "checkout")
	if err := os.MkdirAll(checkout, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	gitdir := "gitdir: " + filepath.Join(source, ".git") + "\n"
	if err := os.WriteFile(filepath.Join(checkout, ".git"), []byte(gitdir), 0o600); err != nil {
		t.Fatalf("write .git file: %v", err)
	}

	feed := newGitChangeFeed([]GitRepo{{URL: checkout}}, t.TempDir())
	recs, err := feed.Changes(context.Background(), now.Add(-60*time.Minute))
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	if len(recs) != 1 || recs[0].Service != "checkout" || recs[0].Summary != "checkout deploy" {
		t.Fatalf("records = %+v, want one checkout deploy record", recs)
	}
}

func TestGitChangeFeed_ConcurrentFirstUseSharesOneMirror(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	repo := initGitRepo(t, "orders", []commitSpec{
		{now.Add(-4 * time.Minute), "main.go", "orders deploy"},
	})
	cacheDir := t.TempDir()
	feed := newGitChangeFeed([]GitRepo{{URL: repo}}, cacheDir)
	// A second feed over the same cache directory, as after a reload.
	other := newGitChangeFeed([]GitRepo{{URL: repo}}, cacheDir)

	const callers = 8
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			use := feed
			if i%2 == 1 {
				use = other
			}
			recs, err := use.Changes(context.Background(), now.Add(-60*time.Minute))
			if err == nil && len(recs) != 1 {
				err = fmt.Errorf("got %d records, want 1", len(recs))
			}
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Changes: %v", err)
		}
	}

	recs, err := feed.Changes(context.Background(), now.Add(-60*time.Minute))
	if err != nil || len(recs) != 1 {
		t.Fatalf("later Changes = %+v, %v; want one record", recs, err)
	}
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		t.Fatalf("read cache dir: %v", err)
	}
	if len(entries) != 1 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("cache dir holds %v, want only the one mirror", names)
	}
}

func TestGitChangeFeed_ReplacesUnusableMirror(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	repo := initGitRepo(t, "invoices", []commitSpec{
		{now.Add(-3 * time.Minute), "main.go", "invoices deploy"},
	})
	cacheDir := t.TempDir()
	feed := newGitChangeFeed([]GitRepo{{URL: repo}}, cacheDir).(*gitChangeFeed)

	// Leave behind a mirror directory that is not a repository.
	broken := feed.cachePathFor(repo)
	if err := os.MkdirAll(broken, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(broken, "leftover"), []byte("partial"), 0o600); err != nil {
		t.Fatalf("write leftover: %v", err)
	}

	recs, err := feed.Changes(context.Background(), now.Add(-60*time.Minute))
	if err != nil {
		t.Fatalf("Changes over an unusable mirror: %v", err)
	}
	if len(recs) != 1 || recs[0].Summary != "invoices deploy" {
		t.Fatalf("records = %+v, want one invoices deploy record", recs)
	}
	if _, err := os.Stat(filepath.Join(broken, "leftover")); !os.IsNotExist(err) {
		t.Fatalf("unusable mirror was not replaced: stat leftover err = %v", err)
	}
}

func TestGitChangeFeed_CancelledContextLeavesNoMirror(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	repo := initGitRepo(t, "refunds", []commitSpec{
		{now.Add(-3 * time.Minute), "main.go", "refunds deploy"},
	})
	cacheDir := t.TempDir()
	feed := newGitChangeFeed([]GitRepo{{URL: repo}}, cacheDir)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := feed.Changes(ctx, now.Add(-60*time.Minute)); err == nil {
		t.Fatal("expected an error for a cancelled context")
	}
	entries, err := os.ReadDir(cacheDir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read cache dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("cancelled clone left %d cache entries", len(entries))
	}

	recs, err := feed.Changes(context.Background(), now.Add(-60*time.Minute))
	if err != nil || len(recs) != 1 {
		t.Fatalf("Changes after cancellation = %+v, %v; want one record", recs, err)
	}
}
